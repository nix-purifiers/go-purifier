// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/mod/module"
)

// The lockfile sits at the lock root (root.go).
const lockFileName = "go-purifier.lock.json"

// The lock format version this binary writes and reads. nix/lib/lock-version.nix
// holds the same number.
const lockFormatVersion = 1

const defaultProxy = "https://proxy.golang.org"

// Lock is the content of go-purifier.lock.json. It holds what Nix needs
// and go.mod and go.sum cannot give it: the SHA-256 of each file from the
// proxy (go.sum holds hashes of module contents, which Nix cannot check a
// download against), the repository and revision of each module from
// version control, the main packages, and what tells a stale lockfile.
type Lock struct {
	raw           []byte
	Gopath        *gopathLock    `json:"gopath,omitempty"`
	ProjectInputs []projectInput `json:"projectInputs,omitempty"`
	Version       int            `json:"version"`
	// The module proxy the files were downloaded from. A file is at its
	// path under this URL.
	Proxy string `json:"proxy"`
	// The GONOPROXY patterns of the lock run: the modules it fetched from
	// version control. The next lock reads them again.
	NoProxy string `json:"noProxy"`
	// The files, relative to the lock root, that define the dependencies,
	// and their digest. A lockfile whose digest differs from the files is
	// stale.
	DefinitionFiles []string `json:"definitionFiles"`
	DefinitionHash  string   `json:"definitionHash"`
	// The main packages of the main modules: what a Nix build installs.
	Binaries []Binary `json:"binaries"`
	// The .mod and .zip files from the proxy, as their proxy path and
	// their SHA-256 in SRI form.
	Files map[string]string `json:"files"`
	// The checkouts of the repositories of modules from version control, as
	// entries of the shared library of the nix-purifiers.
	Checkouts []Artifact `json:"checkouts"`
	// The module versions fetched from version control, and their checkouts.
	VCSModules []VCSModule `json:"vcsModules"`
	// The digest of the rest of the lockfile (contentHash.go). A lockfile
	// whose content does not match it was changed by hand.
	ContentHash string `json:"contentHash"`
}

// Binary is a main package of a main module.
type Binary struct {
	// The command name `go install` gives the package.
	Name string `json:"name"`
	// The import path of the package.
	Package string `json:"package"`
}

// Artifact is a checkout of a repository, as an entry of the shared library
// of the nix-purifiers: exactly one of Git, Hg, Svn, Bzr and Fossil is set.
// Path names it; nix/lib/lock.nix makes the file entries of the proxy
// files from Lock.Files, with a URL and empty mirrors.
//
// The mirrors of a file entry are empty. Two module proxies serve zips
// with the same content, which go.sum verifies, but not necessarily the
// same bytes, so the other proxies in GOPROXY are not mirrors of the lock
// proxy.
type Artifact struct {
	Path   string     `json:"path"`
	Git    *GitSource `json:"git,omitempty"`
	Hg     *HgSource  `json:"hg,omitempty"`
	Svn    *SvnSource `json:"svn,omitempty"`
	Bzr    *RevSource `json:"bzr,omitempty"`
	Fossil *RevSource `json:"fossil,omitempty"`
	// The NAR hash of the checkout, in SRI form.
	Hash string `json:"hash"`
}

// GitSource is the repository and revision of a git entry.
type GitSource struct {
	URL        string `json:"url"`
	Rev        string `json:"rev"`
	Submodules bool   `json:"submodules"`
	// The shared entry kind keeps the .git directory of a checkout when
	// LeaveDotGit is true, rebuilt from CommitObject. go-purifier writes
	// false and null: module zips hold tracked files only.
	LeaveDotGit  bool    `json:"leaveDotGit"`
	CommitObject *string `json:"commitObject"`
}

// HgSource is the repository and revision of a Mercurial entry.
type HgSource struct {
	URL      string `json:"url"`
	Rev      string `json:"rev"`
	Subrepos bool   `json:"subrepos"`
}

// SvnSource is the repository and revision of a Subversion entry.
type SvnSource struct {
	URL             string `json:"url"`
	Rev             string `json:"rev"`
	IgnoreExternals bool   `json:"ignoreExternals"`
	IgnoreKeywords  bool   `json:"ignoreKeywords"`
}

// RevSource is the repository and revision of a Bazaar or Fossil entry.
type RevSource struct {
	URL string `json:"url"`
	Rev string `json:"rev"`
}

// goEnv is the go command's view of the user's environment and go env
// file, read before the lock run replaces both.
type goEnv struct {
	GOWORK     string
	GOMOD      string
	GOPROXY    string
	GONOPROXY  string
	GOPRIVATE  string
	GOINSECURE string
	GOVCS      string
}

func readGoEnv(goBin, dir string) (goEnv, error) {
	out, err := goOutput(goBin, dir, os.Environ(), "env", "-json", "GOWORK", "GOMOD", "GOPROXY", "GONOPROXY", "GOPRIVATE", "GOINSECURE", "GOVCS")
	if err != nil {
		return goEnv{}, err
	}
	var env goEnv
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		return goEnv{}, fmt.Errorf("reading go env: %w", err)
	}
	return env, nil
}

// lockRoot is the lock root of the go command's environment, and whether
// it is a workspace.
func (e goEnv) lockRoot() (string, bool, error) {
	if e.GOWORK != "" && e.GOWORK != "off" {
		return filepath.Dir(e.GOWORK), true, nil
	}
	if e.GOMOD == "" || e.GOMOD == os.DevNull {
		return "", false, errors.New("the current directory is not inside a Go module or workspace")
	}
	return filepath.Dir(e.GOMOD), false, nil
}

// lockCommand writes the lockfile of the module or workspace of the
// current directory. The lock run keeps the resolution of go.mod and
// go.sum as they are, and a lock of an unchanged project writes the same
// lockfile byte for byte.
func lockCommand(args []string) error {
	for i, argument := range args {
		if argument == "--gopath" {
			return lockGopathCommand(append(append([]string{}, args[:i]...), args[i+1:]...))
		}
	}
	references, args, err := moduleInputFlags(args)
	if err != nil {
		return err
	}
	lockFlag, args, err := lockfileFlag(args)
	if err != nil {
		return err
	}
	toolFlag, args, err := toolLockfileFlag(args)
	if err != nil {
		return err
	}
	if len(args) > 0 {
		return fmt.Errorf("usage: go-purifier lock [--lockfile <path>] [--tool-lockfile <path>]")
	}
	goBin, err := realGo()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	return withToolLock(goBin, cwd, toolFlag, func() error {
		return lockAtWithInputs(goBin, cwd, lockFlag, references)
	})
}

// lockAt locks the module or workspace of directory dir into the lockfile
// that lockFlag or the lock root names.
func lockAt(goBin, dir, lockFlag string) error {
	return lockAtWithInputs(goBin, dir, lockFlag, nil)
}
func lockAtWithInputs(goBin, dir, lockFlag string, references []string) error {
	env, err := readGoEnv(goBin, dir)
	if err != nil {
		return err
	}
	root, workspace, err := env.lockRoot()
	if err != nil {
		return err
	}
	lockPath := lockPathAt(root, lockFlag)
	var previous []projectInput
	if old, err := readLock(lockPath); err == nil {
		previous = old.ProjectInputs
	}
	inputs, err := resolveModuleInputs(root, previous, references)
	if err != nil {
		return err
	}
	work, err := inputs.bind(root)
	if err != nil {
		return err
	}
	if !workspace && len(inputs.Repositories) == 0 {
		if err := completeGoSum(goBin, root); err != nil {
			return err
		}
	}
	proxy := lockProxy(env.GOPROXY)
	// A lock without GONOPROXY or GOPRIVATE in the environment takes the
	// patterns of the previous lockfile (previousNoProxy).
	noProxy := env.GONOPROXY
	if noProxy == "" {
		noProxy = previousNoProxy(lockPath)
	}
	fmt.Fprintf(os.Stderr, "go-purifier: locking %s from %s\n", root, proxy)
	settings := lockSettings{Work: work, Repositories: inputs.Repositories, NoProxy: noProxy, Private: env.GOPRIVATE, Insecure: env.GOINSECURE, VCS: env.GOVCS}
	if settings.Private == "" {
		settings.Private = noProxy
	}
	lock, err := makeLock(goBin, root, workspace, proxy, settings, defaultVCSTools())
	if err != nil {
		return fmt.Errorf("%w\nThe lock reads go.mod as it is. When the go command says that go.mod lacks a module, run `go mod tidy`, then `go-purifier lock` again.", err)
	}
	if err := stripModuleInputSums(root, lock.DefinitionFiles, inputs.Repositories); err != nil {
		return err
	}
	lock.DefinitionHash, err = definitionHash(root, lock.DefinitionFiles, nil)
	if err != nil {
		return err
	}
	lock.ProjectInputs = inputs.Inputs
	if err := writeLock(lockPath, lock); err != nil {
		return err
	}
	var names []string
	for _, b := range lock.Binaries {
		names = append(names, b.Name)
	}
	fmt.Fprintf(os.Stderr, "go-purifier: wrote %s with %d locked downloads and the commands [%s]\n", lockPath, len(lock.Files)+len(lock.Checkouts), strings.Join(names, " "))
	return nil
}

// lockProxy is the proxy a lock downloads from: GO_PURIFIER_PROXY, or the
// first proxy URL in GOPROXY, or the default proxy. Every download of the
// lock run outside GONOPROXY goes through this one proxy, so each locked
// file has a URL.
func lockProxy(goproxy string) string {
	if p := os.Getenv("GO_PURIFIER_PROXY"); p != "" {
		return strings.TrimSuffix(p, "/")
	}
	for _, entry := range strings.FieldsFunc(goproxy, func(r rune) bool { return r == ',' || r == '|' }) {
		if strings.HasPrefix(entry, "https://") || strings.HasPrefix(entry, "http://") {
			return strings.TrimSuffix(entry, "/")
		}
	}
	return defaultProxy
}

// lockSettings are the go settings of the lock run that concern modules
// from version control.
type lockSettings struct {
	NoSumDB string
	// A generated native workspace binds supplied module identities.
	Work         string
	Repositories []string

	// The GONOPROXY patterns: the modules fetched from version control.
	NoProxy string
	// The GOPRIVATE patterns, which make GOVCS treat a module as private.
	Private string
	// The GOINSECURE patterns, which let the go command fetch the modules
	// they match over plain HTTP.
	Insecure string
	// GOVCS, the version control systems the go command may use.
	VCS string
}

// lockEnv is the environment of the lock run: an empty GOPATH, module
// cache and build cache under tmp, every module outside the GONOPROXY
// patterns from the one proxy, and none of the user's go env file. The
// modules GONOPROXY matches come from version control and skip the
// checksum database; go.sum still verifies them.
func lockEnv(tmp, proxy string, s lockSettings) []string {
	drop := map[string]bool{
		"GOENV": true, "GOPATH": true, "GOMODCACHE": true, "GOCACHE": true, "GOPROXY": true,
		"GOFLAGS": true, "GOTOOLCHAIN": true, "GOPRIVATE": true, "GONOPROXY": true,
		"GONOSUMDB": true, "GOINSECURE": true, "GOVCS": true, "GO_PURIFIER": true,
	}
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if !drop[key] {
			env = append(env, kv)
		}
	}
	env = append(env,
		"GOENV=off",
		"GOPATH="+filepath.Join(tmp, "gopath"),
		"GOMODCACHE="+filepath.Join(tmp, "mod"),
		"GOCACHE="+filepath.Join(tmp, "build"),
		"GOPROXY="+moduleInputProxy(s.Repositories, proxy),
		"GOFLAGS=-mod=readonly",
		"GOTOOLCHAIN=local",
	)
	if s.Work != "" {
		env = append(env, "GOWORK="+s.Work)
	}
	for _, kv := range [][2]string{
		{"GONOPROXY", s.NoProxy},
		{"GONOSUMDB", s.NoProxy},
		{"GOPRIVATE", s.Private},
		{"GOINSECURE", s.Insecure},
		{"GOVCS", s.VCS},
	} {
		if kv[1] != "" {
			env = append(env, kv[0]+"="+kv[1])
		}
	}
	if s.NoSumDB != "" {
		env = append(env, "GONOSUMDB="+s.NoSumDB)
	}
	return env
}

// listedModule is an entry of `go list -m -json all`.
type listedModule struct {
	Path    string
	Main    bool
	Dir     string
	Replace *struct {
		Path    string
		Version string
		Dir     string
	}
}

func decodeModules(out string) ([]listedModule, error) {
	var mods []listedModule
	dec := json.NewDecoder(strings.NewReader(out))
	for {
		var m listedModule
		if err := dec.Decode(&m); err == io.EOF {
			return mods, nil
		} else if err != nil {
			return nil, fmt.Errorf("reading go list -m: %w", err)
		}
		mods = append(mods, m)
	}
}

// makeLock runs the lock: it downloads what building and testing every
// package of the main modules needs, then records the downloads.
func makeLock(goBin, root string, workspace bool, proxy string, settings lockSettings, tools vcsTools) (*Lock, error) {
	noProxy := settings.NoProxy
	tmp, err := os.MkdirTemp("", "go-purifier-lock-")
	if err != nil {
		return nil, err
	}
	patterns, err := moduleInputPatterns(settings.Repositories)
	if err != nil {
		return nil, err
	}
	if patterns != "" {
		settings.NoSumDB = strings.Trim(settings.NoProxy+","+patterns, ",")
	}
	env := lockEnv(tmp, proxy, settings)
	defer removeModCache(goBin, root, env, tmp)

	// The module graph, which downloads the go.mod file of every module in
	// it.
	listed, err := goOutput(goBin, root, env, "list", "-m", "-json", "all")
	if err != nil {
		return nil, err
	}
	mods, err := decodeModules(listed)
	if err != nil {
		return nil, err
	}
	// Every package of the main modules and every package they and their
	// tests import, which downloads the module of each.
	mains, err := goOutput(goBin, root, env, "list", "-f", `{{if and (eq .Name "main") .Module .Module.Main}}{{.ImportPath}}{{end}}`, "all")
	if err != nil {
		return nil, err
	}
	binaries, err := binariesOf(strings.Fields(mains))
	if err != nil {
		return nil, err
	}

	lock := &Lock{
		Version:  lockFormatVersion,
		Proxy:    proxy,
		NoProxy:  noProxy,
		Binaries: binaries,
	}
	// The go.mod and go.sum of the main modules and of every directory a
	// replace directive points at define the dependencies.
	var dirs []string
	supplied, err := moduleInputNames(settings.Repositories)
	if err != nil {
		return nil, err
	}
	for _, m := range mods {
		switch {
		case m.Main:
			dirs = append(dirs, m.Dir)
		case m.Replace != nil && m.Replace.Version == "" && !supplied[m.Path]:
			dirs = append(dirs, m.Replace.Dir)
		}
	}
	if lock.DefinitionFiles, err = definitionFiles(root, workspace, dirs); err != nil {
		return nil, err
	}
	if lock.DefinitionHash, err = definitionHash(root, lock.DefinitionFiles, nil); err != nil {
		return nil, err
	}

	cacheDir := filepath.Join(tmp, "mod", "cache", "download")
	if lock.Files, err = collectFiles(cacheDir, noProxy); err != nil {
		return nil, err
	}
	if err := omitModuleInputs(lock.Files, settings.Repositories); err != nil {
		return nil, err
	}
	if lock.VCSModules, lock.Checkouts, err = vcsModules(cacheDir, noProxy, tools); err != nil {
		return nil, err
	}
	if lock.Checkouts == nil {
		lock.Checkouts = []Artifact{}
	}
	return lock, nil
}

var majorVersionElem = regexp.MustCompile(`^v[0-9]+$`)

// binaryName is the command name `go install` gives a main package: the
// last element of its import path, or the one before it when the last is a
// major version suffix such as v2.
func binaryName(importPath string) string {
	elems := strings.Split(importPath, "/")
	name := elems[len(elems)-1]
	if len(elems) > 1 && majorVersionElem.MatchString(name) {
		name = elems[len(elems)-2]
	}
	return name
}

func binariesOf(importPaths []string) ([]Binary, error) {
	binaries := []Binary{}
	byName := map[string]string{}
	for _, p := range importPaths {
		name := binaryName(p)
		if other, ok := byName[name]; ok {
			return nil, fmt.Errorf("main packages %s and %s both install as %q", other, p, name)
		}
		byName[name] = p
		binaries = append(binaries, Binary{Name: name, Package: p})
	}
	sort.Slice(binaries, func(i, j int) bool { return binaries[i].Name < binaries[j].Name })
	return binaries, nil
}

// lockable reports whether a file of the module download cache holds
// exactly the bytes the proxy served, and those bytes never change. That
// holds for the .mod and .zip file of a version, which go.sum verifies.
// The go command writes each .info file in the cache itself, so its bytes
// differ from the .info file at the proxy URL. A version list (@v/list)
// changes as versions are published. The .lock, .ziphash and .partial
// files are the cache's bookkeeping.
func lockable(rel string) bool {
	if !strings.Contains(rel, "/@v/") {
		return false
	}
	switch filepath.Ext(rel) {
	case ".mod", ".zip":
		return true
	}
	return false
}

// collectFiles maps the lockable files of a module download cache that came
// from the proxy, those of modules noProxy does not match, to their
// SHA-256. The download cache has the layout of a module proxy, so a
// file's path in it is also its path under the proxy URL. The checksum
// database's tiles (sumdb/) are left out: a build from the lockfile does
// not consult it.
func collectFiles(dir, noProxy string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == dir {
				return filepath.SkipDir
			}
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "sumdb" {
				return filepath.SkipDir
			}
			return nil
		}
		if !lockable(rel) {
			return nil
		}
		if noProxy != "" {
			escaped, _, _ := splitProxyPath(rel)
			modPath, err := module.UnescapePath(escaped)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			if module.MatchPrefixPatterns(noProxy, modPath) {
				return nil
			}
		}
		hash, err := fileHash(p)
		if err != nil {
			return err
		}
		files[rel] = hash
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return sri(h.Sum(nil)), nil
}

func sri(sum []byte) string {
	return "sha256-" + base64.StdEncoding.EncodeToString(sum)
}

// removeModCache deletes the lock run's caches. The go command makes the
// module cache read-only, and `go clean -modcache` is how it removes it.
func removeModCache(goBin, root string, env []string, tmp string) {
	_ = goRun(goBin, root, env, "clean", "-modcache")
	_ = os.RemoveAll(tmp)
}

// previousNoProxy is the GONOPROXY patterns a lockfile records, read
// without its checks, or "" when it has none or cannot be read.
func previousNoProxy(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var lock struct {
		NoProxy string `json:"noProxy"`
	}
	if json.Unmarshal(data, &lock) != nil {
		return ""
	}
	return lock.NoProxy
}

func readLock(path string) (*Lock, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lock Lock
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("%s is not a go-purifier lockfile (%v); run `go-purifier lock` to write it again", path, err)
	}
	lock.raw = append([]byte(nil), data...)
	if lock.Gopath != nil && !validGopathImportPath(lock.Gopath.ImportPath) {
		return nil, fmt.Errorf("invalid GOPATH project import path %q", lock.Gopath.ImportPath)
	}
	if lock.Version == lockFormatVersion {
		want, err := contentHash(data)
		if err != nil {
			return nil, fmt.Errorf("%s is not a go-purifier lockfile (%v); run `go-purifier lock` to write it again", path, err)
		}
		if lock.ContentHash != want {
			return nil, fmt.Errorf("%s was changed after go-purifier wrote it, since its contentHash does not match its content; run `go-purifier lock` to write it again", path)
		}
	}
	return &lock, nil
}

func writeLock(path string, lock *Lock) error {
	lock.ContentHash = ""
	unhashed, err := json.Marshal(lock)
	if err != nil {
		return err
	}
	if lock.ContentHash, err = contentHash(unhashed); err != nil {
		return err
	}
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func goRun(goBin, dir string, env []string, args ...string) error {
	cmd := exec.Command(goBin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func goOutput(goBin, dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command(goBin, args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}
