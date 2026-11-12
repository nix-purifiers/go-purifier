// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

// A module that GONOPROXY (or GOPRIVATE) names is fetched by the go
// command from its version control repository rather than from the proxy:
// git, Mercurial, Subversion, Bazaar or Fossil. The lock records the
// repository checkout as an entry of that system's kind, and the Nix build
// makes the module's .mod and .zip files from the checkout with
// `go-purifier module-files`, the same way the go command makes them.
// go.sum then verifies both.

// VCSModule is a module version the lock run fetched from version control.
// lib-overlays/default/lock.nix derives its proxy directory from Module.
type VCSModule struct {
	Module  string `json:"module"`
	Version string `json:"version"`
	// The path of the checkout entry that holds the module.
	Entry string `json:"entry"`
	// The module's directory in the repository, or "".
	Subdir string `json:"subdir"`
}

// origin is the part of a cached .info file that records where the go
// command fetched a module from.
type origin struct {
	Version string
	Time    string
	Origin  *struct {
		VCS    string
		URL    string
		Subdir string
		Hash   string
		Ref    string
	}
}

// splitProxyPath splits a proxy path such as
// github.com/!burnt!sushi/toml/@v/v1.4.0.zip into the escaped module path
// and the file name.
func splitProxyPath(rel string) (escapedModule, file string, ok bool) {
	i := strings.LastIndex(rel, "/@v/")
	if i < 0 {
		return "", "", false
	}
	return rel[:i], rel[i+len("/@v/"):], true
}

// vcsTools are what the lock run asks of the version control systems and
// of Nix; tests replace them.
type vcsTools struct {
	// resolve is the fixed revision of a repository at a reference, for a
	// system whose module origins name only the reference.
	resolve func(vcs, url, ref string) (string, error)
	// prefetch is the NAR hash of the checkout an entry names, as its
	// fetcher produces it.
	prefetch func(entry Artifact) (string, error)
}

func defaultVCSTools() vcsTools {
	return vcsTools{
		resolve: resolveRevision,
		prefetch: func(entry Artifact) (string, error) {
			if entry.Git != nil {
				return prefetchGit(entry.Git.URL, entry.Git.Rev)
			}
			return prefetchWithNixpkgs(entry)
		},
	}
}

// vcsModules finds the module versions in a module download cache that
// the go command fetched from version control, which are the ones
// noProxy matches, and records each with an entry for its checkout.
// Modules of one repository revision share an entry.
func vcsModules(cacheDir, noProxy string, tools vcsTools) ([]VCSModule, []Artifact, error) {
	if noProxy == "" {
		return []VCSModule{}, nil, nil
	}
	mods := []VCSModule{}
	entries := map[string]Artifact{}
	var order []string
	err := filepath.WalkDir(cacheDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == cacheDir {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() || filepath.Ext(p) != ".info" {
			return nil
		}
		rel, err := filepath.Rel(cacheDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		escaped, file, ok := splitProxyPath(rel)
		if !ok {
			return nil
		}
		modPath, err := module.UnescapePath(escaped)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if !module.MatchPrefixPatterns(noProxy, modPath) {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var info origin
		if err := json.Unmarshal(data, &info); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		version := strings.TrimSuffix(file, ".info")
		if info.Origin != nil && info.Origin.Hash == "" && info.Origin.Ref == "" {
			info.Origin.Ref = pseudoVersionRevision(version)
		}
		if info.Origin == nil || info.Origin.URL == "" || (info.Origin.Hash == "" && info.Origin.Ref == "") {
			return fmt.Errorf("%s@%s: the go command recorded no repository and revision for this module (%s)", modPath, version, strings.TrimSpace(string(data)))
		}
		entry, err := checkoutEntry(info.Origin.VCS, info.Origin.URL, info.Origin.Hash, info.Origin.Ref, tools)
		if err != nil {
			return fmt.Errorf("%s@%s: %w", modPath, version, err)
		}
		if _, ok := entries[entry.Path]; !ok {
			if entry.Hash, err = tools.prefetch(entry); err != nil {
				return err
			}
			entries[entry.Path] = entry
			order = append(order, entry.Path)
		}
		entryPath := entry.Path
		mods = append(mods, VCSModule{
			Module:  modPath,
			Version: version,
			Entry:   entryPath,
			Subdir:  info.Origin.Subdir,
		})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	var arts []Artifact
	for _, p := range order {
		arts = append(arts, entries[p])
	}
	return mods, arts, nil
}

var plainRev = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// pseudoVersionRevision is the revision number a pseudo-version of a
// Subversion or Bazaar module encodes, or "". The go command records no
// revision in the .info file of such a version, but the last part of the
// pseudo-version is the revision number, zero-padded to twelve digits.
func pseudoVersionRevision(version string) string {
	if !module.IsPseudoVersion(version) {
		return ""
	}
	rev, err := module.PseudoVersionRev(version)
	if err != nil || strings.Trim(rev, "0123456789") != "" {
		return ""
	}
	if trimmed := strings.TrimLeft(rev, "0"); trimmed != "" {
		return trimmed
	}
	return "0"
}

// checkoutEntry is the entry, without its hash, for the checkout of a
// repository of system vcs at url, at the revision the go command
// recorded: hash, or else the reference ref, which resolve fixes to a
// revision. The entry kinds and their fields are those the nix-purifiers
// share. A Subversion entry also says to leave out externals and to keep
// keywords unexpanded, as the go command's `svn export` does.
func checkoutEntry(vcs, url, hash, ref string, tools vcsTools) (Artifact, error) {
	rev := hash
	if rev == "" {
		var err error
		if rev, err = tools.resolve(vcs, url, ref); err != nil {
			return Artifact{}, err
		}
	}
	if vcs == "git" || vcs == "hg" {
		url = httpsURL(url)
	}
	urlSum := sha256.Sum256([]byte(url))
	name := rev
	if !plainRev.MatchString(rev) {
		revSum := sha256.Sum256([]byte(rev))
		name = hex.EncodeToString(revSum[:12])
	}
	entry := Artifact{Path: "vcs/" + hex.EncodeToString(urlSum[:6]) + "-" + name}
	switch vcs {
	case "git":
		entry.Git = &GitSource{URL: url, Rev: rev, Submodules: false}
	case "hg":
		entry.Hg = &HgSource{URL: url, Rev: rev, Subrepos: false}
	case "svn":
		entry.Svn = &SvnSource{URL: url, Rev: rev, IgnoreExternals: true, IgnoreKeywords: true}
	case "bzr":
		entry.Bzr = &RevSource{URL: url, Rev: rev}
	case "fossil":
		entry.Fossil = &RevSource{URL: url, Rev: rev}
	default:
		return Artifact{}, fmt.Errorf("the go command fetched it with %q, which go-purifier does not know", vcs)
	}
	return entry, nil
}

// resolveRevision fixes a reference of a Subversion or Bazaar repository
// to a revision, the way the go command resolved it: the revision number
// `svn log` reports for the reference, or the revision ID `bzr log`
// reports.
func resolveRevision(vcs, url, ref string) (string, error) {
	switch vcs {
	case "svn":
		out, err := output("", "svn", "log", "-l1", "--xml", "--non-interactive", "--", url+"@"+ref)
		if err != nil {
			return "", err
		}
		m := regexp.MustCompile(`<logentry\s+revision="([0-9]+)"`).FindStringSubmatch(out)
		if m == nil {
			return "", fmt.Errorf("svn log printed no revision for %s@%s", url, ref)
		}
		return m[1], nil
	case "bzr":
		out, err := output("", "bzr", "log", "-l1", "--long", "--show-ids", "--revision="+ref, url)
		if err != nil {
			return "", err
		}
		m := regexp.MustCompile(`(?m)^revision-id:\s*(\S+)`).FindStringSubmatch(out)
		if m == nil {
			return "", fmt.Errorf("bzr log printed no revision ID for %s at %s", url, ref)
		}
		return "revid:" + m[1], nil
	}
	return "", fmt.Errorf("the go command recorded no revision of the %s repository %s", vcs, url)
}

// prefetchWithNixpkgs is the NAR hash of the checkout of a Mercurial,
// Subversion, Bazaar or Fossil entry. It builds the entry with the nixpkgs
// fetcher a build uses, and a placeholder hash, and reads the hash of the
// result from the hash mismatch Nix reports.
func prefetchWithNixpkgs(entry Artifact) (string, error) {
	nixLib, nixpkgs := os.Getenv("GO_PURIFIER_NIX_LIB"), os.Getenv("GO_PURIFIER_NIXPKGS")
	if nixLib == "" || nixpkgs == "" {
		return "", errors.New("locking a module from a Mercurial, Subversion, Bazaar or Fossil repository needs GO_PURIFIER_NIX_LIB and GO_PURIFIER_NIXPKGS; run go-purifier from the go-purifier flake")
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("nix-build", filepath.Join(nixLib, "prefetch.nix"),
		"--argstr", "nixpkgs", nixpkgs, "--argstr", "entry", string(data), "--no-out-link")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if m := regexp.MustCompile(`got:\s+(sha256-[A-Za-z0-9+/=]+)`).FindStringSubmatch(stderr.String()); m != nil {
		return m[1], nil
	}
	os.Stderr.Write(stderr.Bytes())
	if runErr == nil {
		return "", fmt.Errorf("prefetching %s: the placeholder hash matched", entry.Path)
	}
	return "", fmt.Errorf("prefetching %s: %w", entry.Path, runErr)
}

// httpsURL is the HTTPS URL of the repository at an SSH URL, such as
// https://github.com/org/repo for git@github.com:org/repo.git or
// ssh://git@github.com/org/repo. The Nix sandbox has no SSH keys, so the
// nix-purifiers record a git entry under the HTTPS URL of the same host;
// a private repository then takes a fetchgit with credentials. Other URLs
// stay as they are.
func httpsURL(url string) string {
	var host, path string
	switch {
	case strings.HasPrefix(url, "ssh://"):
		rest := strings.TrimPrefix(url, "ssh://")
		if i := strings.Index(rest, "@"); i >= 0 {
			rest = rest[i+1:]
		}
		var ok bool
		if host, path, ok = strings.Cut(rest, "/"); !ok {
			return url
		}
		if h, _, ok := strings.Cut(host, ":"); ok {
			host = h
		}
	case !strings.Contains(url, "://") && strings.Contains(url, "@") && strings.Contains(url, ":"):
		rest := url[strings.Index(url, "@")+1:]
		host, path, _ = strings.Cut(rest, ":")
	default:
		return url
	}
	return "https://" + host + "/" + strings.TrimPrefix(path, "/")
}

// prefetchGit is the NAR hash of a git checkout without submodules, as
// nixpkgs' fetchgit and builtins.fetchGit produce it, from
// nix-prefetch-git.
func prefetchGit(url, rev string) (string, error) {
	cmd := exec.Command("nix-prefetch-git", "--url", url, "--rev", rev, "--quiet")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("nix-prefetch-git %s %s: %w", url, rev, err)
	}
	var out struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || !strings.HasPrefix(out.Hash, "sha256-") {
		return "", fmt.Errorf("nix-prefetch-git %s %s printed no SRI hash", url, rev)
	}
	return out.Hash, nil
}

// moduleFilesCommand writes the .mod and .zip file of a module version
// from a checkout of its repository into the directory $out. It is the
// builder of the Nix derivation for each locked VCS module:
//
//	go-purifier module-files <checkout> <subdir> <module> <version>
func moduleFilesCommand(args []string) error {
	if len(args) != 4 {
		return errors.New("usage: go-purifier module-files <checkout> <subdir> <module> <version>")
	}
	checkout, subdir, modPath, version := args[0], args[1], args[2], args[3]
	out := os.Getenv("out")
	if out == "" {
		return errors.New("module-files writes into $out, which is not set")
	}
	return writeModuleFiles(checkout, subdir, modPath, version, out)
}

// writeModuleFiles writes the files of the module in directory subdir of
// a checkout. The zip holds what the go command's zip of the module holds
// (cmd/go/internal/modfetch/coderepo.go): every file under the module
// directory, filtered by the rules of golang.org/x/mod/zip, and the
// LICENSE file of the repository root when the module is in a
// subdirectory without one of its own.
func writeModuleFiles(checkout, subdir, modPath, version, out string) error {
	dir := filepath.Join(checkout, filepath.FromSlash(subdir))
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	// The go command serves a module without a go.mod file at the version
	// with a go.mod that holds only the module statement.
	gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if errors.Is(err, fs.ErrNotExist) {
		gomod = []byte("module " + modfileQuote(modPath) + "\n")
	} else if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, version+".mod"), gomod, 0o444); err != nil {
		return err
	}
	var files []modzip.File
	haveLicense := false
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "LICENSE" {
			haveLicense = true
		}
		files = append(files, checkoutFile{name: rel, path: p})
		return nil
	})
	if err != nil {
		return err
	}
	if !haveLicense && strings.Trim(subdir, "/") != "" {
		root := filepath.Join(checkout, "LICENSE")
		if info, err := os.Lstat(root); err == nil && info.Mode().IsRegular() {
			files = append(files, checkoutFile{name: "LICENSE", path: root})
		}
	}
	f, err := os.Create(filepath.Join(out, version+".zip"))
	if err != nil {
		return err
	}
	if err := modzip.Create(f, module.Version{Path: modPath, Version: version}, files); err != nil {
		f.Close()
		return fmt.Errorf("zipping %s@%s: %w", modPath, version, err)
	}
	return f.Close()
}

// checkoutFile is a file of a checkout, under its name in the module.
type checkoutFile struct {
	name, path string
}

func (f checkoutFile) Path() string                 { return f.name }
func (f checkoutFile) Lstat() (fs.FileInfo, error)  { return os.Lstat(f.path) }
func (f checkoutFile) Open() (io.ReadCloser, error) { return os.Open(f.path) }

// modfileQuote quotes a module path in a go.mod statement when it needs
// quoting.
func modfileQuote(s string) string {
	if strings.ContainsAny(s, " \t\"'`") {
		b, _ := json.Marshal(s)
		return string(b)
	}
	return s
}
