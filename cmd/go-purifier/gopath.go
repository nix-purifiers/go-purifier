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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// GOPATH capture asks the selected native go for its graph. In particular,
// Go 1.0 has no `list -deps`; its JSON Deps field supplies the next query.
type gopathPackage struct {
	Dir, ImportPath, Name, Root     string
	Goroot, Standard                bool
	Deps, TestImports, XTestImports []string
}
type gopathLock struct {
	ImportPath string `json:"importPath"`
	GoVersion  string `json:"goVersion"`
	CGOEnabled string `json:"cgoEnabled,omitempty"`
	GOOS       string `json:"goos"`
	GOARCH     string `json:"goarch"`
}

func nativeGopathList(goBin, dir string, patterns []string) ([]gopathPackage, error) {
	args := append([]string{"list", "-json"}, patterns...)
	cmd := exec.Command(goBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GO111MODULE=off", "GOTOOLCHAIN=local")
	var data, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &data, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("native GOPATH go list: %w: %s", err, stderr.String())
	}
	decoder := json.NewDecoder(&data)
	var packages []gopathPackage
	for {
		var p gopathPackage
		if err := decoder.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		packages = append(packages, p)
	}
	return packages, nil
}
func validGopathImportPath(name string) bool {
	if strings.ContainsAny(name, "\x00\n\r\t\\:") {
		return false
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
	}
	return true
}

func gopathImportPath(dir, gopath string) (string, error) {
	for _, root := range filepath.SplitList(gopath) {
		if canonical, err := filepath.EvalSymlinks(root); err == nil {
			root = canonical
		}
		rel, err := filepath.Rel(filepath.Join(root, "src"), dir)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			name := filepath.ToSlash(rel)
			if !validGopathImportPath(name) {
				return "", fmt.Errorf("invalid GOPATH import path %q", name)
			}
			return name, nil
		}
	}
	return "", fmt.Errorf("%s is outside GOPATH/src; capture from the project's native GOPATH checkout", dir)
}
func lockGopathCommand(args []string) error {
	lockFlag, args, err := lockfileFlag(args)
	if err != nil {
		return err
	}
	if len(args) != 0 {
		return errors.New("usage: go-purifier lock --gopath [--lockfile <path>]")
	}
	goBin, err := realGo()
	if err != nil {
		return err
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	gopath, err := output(root, goBin, "env", "GOPATH")
	if err != nil {
		return err
	}
	importPath, err := gopathImportPath(root, gopath)
	if err != nil {
		return err
	}
	roots, err := nativeGopathList(goBin, root, []string{"./..."})
	if err != nil {
		return err
	}
	lock := &Lock{Version: lockFormatVersion, Files: map[string]string{}, Checkouts: []Artifact{}, VCSModules: []VCSModule{}, DefinitionFiles: []string{}, Binaries: []Binary{}}
	version, err := output(root, goBin, "version")
	if err != nil {
		return err
	}
	goos, err := output(root, goBin, "env", "GOOS")
	if err != nil {
		return err
	}
	goarch, err := output(root, goBin, "env", "GOARCH")
	if err != nil {
		return err
	}
	cgo, err := output(root, goBin, "env", "CGO_ENABLED")
	if err != nil {
		return err
	}
	lock.Gopath = &gopathLock{ImportPath: importPath, GoVersion: version, CGOEnabled: cgo, GOOS: goos, GOARCH: goarch}
	var queue []string
	packages := map[string]gopathPackage{}
	binaryNames := map[string]string{}
	for _, p := range roots {
		packages[p.ImportPath] = p
		queue = append(queue, p.Deps...)
		queue = append(queue, p.TestImports...)
		queue = append(queue, p.XTestImports...)
		if p.Name == "main" {
			name := filepath.Base(p.ImportPath)
			if previous, exists := binaryNames[name]; exists {
				return fmt.Errorf("GOPATH commands %s and %s have the same executable name %s", previous, p.ImportPath, name)
			}
			binaryNames[name] = p.ImportPath
			lock.Binaries = append(lock.Binaries, Binary{Name: name, Package: p.ImportPath})
		}
	}
	for len(queue) != 0 {
		next := map[string]bool{}
		for _, name := range queue {
			if name != "C" && packages[name].ImportPath == "" {
				next[name] = true
			}
		}
		queue = nil
		if len(next) == 0 {
			break
		}
		names := make([]string, 0, len(next))
		for name := range next {
			names = append(names, name)
		}
		sort.Strings(names)
		for offset := 0; offset < len(names); offset += 128 {
			end := offset + 128
			if end > len(names) {
				end = len(names)
			}
			listed, err := nativeGopathList(goBin, root, names[offset:end])
			if err != nil {
				return err
			}
			for _, p := range listed {
				packages[p.ImportPath] = p
				queue = append(queue, p.Deps...)
			}
		}
	}
	repositories := map[string]Artifact{}
	for _, p := range packages {
		if p.Goroot || p.Standard || p.Dir == "" {
			continue
		}
		dir, err := filepath.EvalSymlinks(p.Dir)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, dir)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if p.Root != "" {
				pool, poolErr := filepath.Rel(root, p.Root)
				if poolErr == nil && pool != ".." && !strings.HasPrefix(pool, ".."+string(filepath.Separator)) {
					return fmt.Errorf("GOPATH dependency %s uses a project-local source workspace; its source binding is not supported yet", p.ImportPath)
				}
			}
			continue
		}
		top, err := output(dir, "git", "rev-parse", "--show-toplevel")
		if err != nil {
			return fmt.Errorf("GOPATH dependency %s needs an immutable Git checkout: %w", p.ImportPath, err)
		}
		prefix, err := gopathImportPath(top, gopath)
		if err != nil {
			return err
		}
		if _, exists := repositories[prefix]; exists {
			continue
		}
		dirty, err := output(top, "git", "status", "--porcelain")
		if err != nil {
			return err
		}
		if dirty != "" {
			return fmt.Errorf("GOPATH dependency %s has uncommitted files; commit them before capture", prefix)
		}
		rev, err := output(top, "git", "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		url, err := output(top, "git", "remote", "get-url", "origin")
		if err != nil {
			return err
		}
		hash, err := prefetchGit(url, rev)
		if err != nil {
			return err
		}
		repositories[prefix] = Artifact{Path: "src/" + prefix, Git: &GitSource{URL: url, Rev: rev}, Hash: hash}
	}
	prefixes := make([]string, 0, len(repositories))
	for prefix := range repositories {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	for _, prefix := range prefixes {
		lock.Checkouts = append(lock.Checkouts, repositories[prefix])
	}
	sort.Slice(lock.Binaries, func(i, j int) bool { return lock.Binaries[i].Package < lock.Binaries[j].Package })
	lock.DefinitionHash, err = definitionHash(root, nil, nil)
	if err != nil {
		return err
	}
	if err := writeLock(lockPathAt(root, lockFlag), lock); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "go-purifier: captured %s with %s, %d GOPATH Git checkouts\n", importPath, version, len(lock.Checkouts))
	return nil
}
func findGopathRoot(dir string) (string, *Lock, error) {
	for {
		lock, err := readLock(lockPathAt(dir, ""))
		if err != nil && !os.IsNotExist(err) {
			return "", nil, err
		}
		if err == nil && lock.Gopath != nil {
			if lock.Version != lockFormatVersion {
				return "", nil, fmt.Errorf("unsupported GOPATH lock version %d; run go-purifier lock --gopath", lock.Version)
			}
			if os.Getenv("GO_PURIFIER_LOCKFILE") != "" {
				for _, workspace := range filepath.SplitList(os.Getenv("GOPATH")) {
					candidate := filepath.Join(workspace, "src", filepath.FromSlash(lock.Gopath.ImportPath))
					if rel, err := filepath.Rel(candidate, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						return candidate, lock, nil
					}
				}
			}
			return dir, lock, nil
		}
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return "", nil, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil, nil
		}
		dir = parent
	}
}

// gopathEnvironment creates a private native GOPATH. Project sources stay live;
// dependency sources come only from the pinned Nix checkout tree.
func gopathEnvironment(goBin, root string, lock *Lock) (string, error) {
	// A capture/SDK pair gets an immutable source view and its own compiled
	// package cache. Concurrent commands never unlink another reader's sources.
	sum := sha256.Sum256([]byte(lock.ContentHash + "\n" + goBin))
	workspace := filepath.Join(root, stateDirName, "gopath", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return "", err
	}
	if len(lock.raw) == 0 {
		return "", errors.New("GOPATH capture has no original lockfile bytes")
	}
	snapshot, err := os.CreateTemp(workspace, ".capture-*.json")
	if err != nil {
		return "", err
	}
	defer os.Remove(snapshot.Name())
	if _, err := snapshot.Write(lock.raw); err != nil {
		snapshot.Close()
		return "", err
	}
	if err := snapshot.Close(); err != nil {
		return "", err
	}
	lockPath := filepath.Join(workspace, "capture.json")
	if err := os.Rename(snapshot.Name(), lockPath); err != nil {
		return "", err
	}
	tree, err := realize(workspace, lockPath)
	if err != nil {
		return "", err
	}
	source := filepath.Join(workspace, "src")
	marker := ".go-purifier-view"
	valid := func() bool {
		saved, err := os.ReadFile(filepath.Join(source, marker))
		return err == nil && string(saved) == lock.ContentHash
	}
	if valid() {
		return workspace, nil
	}
	if _, err := os.Lstat(source); err == nil {
		return "", fmt.Errorf("incomplete GOPATH source view at %s; remove that private view and prefetch again", source)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	staging, err := os.MkdirTemp(workspace, ".src-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	for _, artifact := range lock.Checkouts {
		if !strings.HasPrefix(artifact.Path, "src/") || !validGopathImportPath(strings.TrimPrefix(artifact.Path, "src/")) {
			return "", fmt.Errorf("invalid GOPATH checkout location %q", artifact.Path)
		}
		target := filepath.Join(staging, filepath.FromSlash(strings.TrimPrefix(artifact.Path, "src/")))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		if err := os.Symlink(filepath.Join(tree, filepath.FromSlash(artifact.Path)), target); err != nil {
			return "", err
		}
	}
	project := filepath.Join(staging, filepath.FromSlash(lock.Gopath.ImportPath))
	if err := os.MkdirAll(filepath.Dir(project), 0o755); err != nil {
		return "", err
	}
	if err := os.Symlink(root, project); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(staging, marker), []byte(lock.ContentHash), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(staging, source); err != nil && !valid() {
		return "", err
	}
	return workspace, nil
}

func runGopathGo(goBin, dir, root string, lock *Lock, args []string) int {
	if len(args) > 0 && args[0] == "get" {
		// Native go get owns VCS resolution. Capture/prefetch pins the resulting
		// checkout afterward; a locked build never invokes go get.
		cmd := exec.Command(goBin, args...)
		cmd.Dir = dir
		cmd.Env = os.Environ()
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	workspace, err := gopathEnvironment(goBin, root, lock)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cwd := filepath.Join(workspace, "src", filepath.FromSlash(lock.Gopath.ImportPath), rel)
	cmd := exec.Command(goBin, args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "GOPATH="+workspace, "PWD="+cwd, "GO111MODULE=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "go-purifier: GOPATH build uses pinned checkouts; after native go get, run go-purifier lock --gopath and prefetch")
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		return 1
	}
	return 0
}

func publishGopath(root string, lock *Lock, lockFlag, version string, push bool) error {
	nixLib, nixpkgs, sdk := os.Getenv("GO_PURIFIER_NIX_LIB"), os.Getenv("GO_PURIFIER_NIXPKGS"), os.Getenv("GO_PURIFIER_GO_NIX")
	if nixLib == "" || nixpkgs == "" || sdk == "" {
		return errors.New("GOPATH releases need the configured Nix toolchain; override its go package to select the project SDK")
	}
	top, err := output(root, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	rev, err := output(root, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	rootRel, err := filepath.Rel(top, root)
	if err != nil {
		return err
	}
	if rootRel == "." {
		rootRel = ""
	}
	built, err := output(top, "nix-build", filepath.Join(nixLib, "gopath-release.nix"),
		"--argstr", "nixpkgs", nixpkgs, "--argstr", "repo", top, "--argstr", "rev", rev,
		"--argstr", "rootRel", filepath.ToSlash(rootRel), "--argstr", "lockFile", lockPathAt(root, lockFlag),
		"--argstr", "version", version, "--argstr", "goReference", sdk, "--no-out-link", "--quiet")
	if err != nil {
		return err
	}
	dist := filepath.Join(root, "dist", version)
	files, err := copyDir(built, dist)
	if err != nil {
		return err
	}
	if err := run("git", "-C", top, "tag", "-a", version, "-m", lock.Gopath.ImportPath+" "+version, rev); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "go-purifier: wrote native GOPATH release %s from %s to %s\n", version, rev, dist)
	if !push {
		fmt.Fprintln(os.Stderr, "go-purifier: stopped before pushing (--dry-run)")
		return nil
	}
	if err := run("git", "-C", top, "push", "origin", version); err != nil {
		return err
	}
	if _, err := exec.LookPath("gh"); err == nil {
		args := append([]string{"release", "create", version, "--title", version, "--notes", lock.Gopath.ImportPath + " " + version}, files...)
		return runIn(top, "gh", args...)
	}
	return nil
}
