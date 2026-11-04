// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// A Go module is published by tagging a commit of its repository; the
// module proxy serves any tagged version on request. `go-purifier publish`
// builds the release archives of the module's commands through Nix from
// the commit it tags, so the archives hold what the Nix build of that
// commit produces, and tags the commit. It then pushes the tag, creates a
// GitHub release with the archives, and asks the lock proxy for the new
// version so the proxy caches it. With --dry-run it stops after the tag.
//
//	go-purifier publish <version> [--dry-run] [--lockfile <path>]

const publishUsage = "usage: go-purifier publish <version> [--dry-run] [--lockfile <path>]"

func publishCommand(args []string) error {
	lockFlag, args, err := lockfileFlag(args)
	if err != nil {
		return err
	}
	var version string
	push := true
	for _, a := range args {
		switch {
		case a == "--dry-run":
			push = false
		case version == "" && !strings.HasPrefix(a, "-"):
			version = a
		default:
			return errors.New(publishUsage)
		}
	}
	if version == "" {
		return errors.New(publishUsage)
	}

	goBin, err := realGo()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if root, lock, err := findGopathRoot(cwd); err != nil {
		return err
	} else if lock != nil {
		if !semver.IsValid(version) {
			return fmt.Errorf("%s is not a semantic release version", version)
		}
		return publishGopath(root, lock, lockFlag, version, push)
	}
	env, err := readGoEnv(goBin, cwd)
	if err != nil {
		return err
	}
	if env.GOMOD == "" || env.GOMOD == os.DevNull {
		return errors.New("run go-purifier publish in the directory of the module to publish")
	}
	modDir := filepath.Dir(env.GOMOD)
	modPath, err := modulePath(env.GOMOD)
	if err != nil {
		return err
	}
	if err := checkReleaseVersion(modPath, version); err != nil {
		return err
	}

	root, _, err := env.lockRoot()
	if err != nil {
		return err
	}
	lockPath := lockPathAt(root, lockFlag)
	lock, err := readLock(lockPath)
	if err != nil {
		return fmt.Errorf("a release builds from the lockfile: %w", err)
	}
	if current, err := definitionHash(root, lock.DefinitionFiles, toolLockOverrides()); err != nil {
		return err
	} else if current != lock.DefinitionHash {
		return errors.New("go.mod, go.sum or go.work changed since the lockfile was written; run `go-purifier lock` and commit the lockfile first")
	}

	top, err := output(modDir, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	rev, err := output(modDir, "git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if dirty, err := output(top, "git", "status", "--porcelain", "--untracked-files=no"); err != nil {
		return err
	} else if dirty != "" {
		fmt.Fprintln(os.Stderr, "go-purifier: the working tree has uncommitted changes; the release builds and tags HEAD without them")
	}
	// git prints the top level with symlinks resolved, such as macOS's
	// /private/var for /var, so the other two are resolved too before
	// filepath.Rel compares them.
	realModDir, err := filepath.EvalSymlinks(modDir)
	if err != nil {
		return err
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	modRel, err := filepath.Rel(top, realModDir)
	if err != nil {
		return err
	}
	rootRel, err := filepath.Rel(top, realRoot)
	if err != nil {
		return err
	}
	if rootRel == "." {
		rootRel = ""
	}
	tag := releaseTag(filepath.ToSlash(modRel), modPath, version)

	nixLib := os.Getenv("GO_PURIFIER_NIX_LIB")
	nixpkgs := os.Getenv("GO_PURIFIER_NIXPKGS")
	if nixLib == "" || nixpkgs == "" {
		return errors.New("GO_PURIFIER_NIX_LIB or GO_PURIFIER_NIXPKGS is not set; run go-purifier from the go-purifier flake")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "go-purifier: building the %s release archives of %s from %s\n", version, modPath, rev)
	built, err := output(top, "nix-build", filepath.Join(nixLib, "release.nix"),
		"--argstr", "nixpkgs", nixpkgs,
		"--argstr", "repo", top,
		"--argstr", "rev", rev,
		"--argstr", "modRoot", filepath.ToSlash(rootRel),
		"--argstr", "lockFile", lockPath,
		"--argstr", "version", version,
		"--argstr", "cli", self,
		"--no-out-link", "--quiet")
	if err != nil {
		return err
	}
	dist := filepath.Join(modDir, "dist", version)
	files, err := copyDir(built, dist)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "go-purifier: wrote the release archives to %s\n", dist)

	if err := run("git", "-C", top, "tag", "-a", tag, "-m", modPath+" "+version, rev); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "go-purifier: tagged %s as %s\n", rev, tag)
	if !push {
		fmt.Fprintf(os.Stderr, "go-purifier: stopped before pushing (--dry-run); push %s and create the release yourself, or delete the tag and run go-purifier publish %s\n", tag, version)
		return nil
	}

	if err := run("git", "-C", top, "push", "origin", tag); err != nil {
		return err
	}
	if _, err := exec.LookPath("gh"); err == nil {
		ghArgs := append([]string{"release", "create", tag, "--title", tag, "--notes", modPath + " " + version}, files...)
		if err := runIn(top, "gh", ghArgs...); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(os.Stderr, "go-purifier: gh is not on PATH; attach the files in %s to a release of %s yourself\n", dist, tag)
	}
	if lock.NoProxy == "" || !module.MatchPrefixPatterns(lock.NoProxy, modPath) {
		fmt.Fprintf(os.Stderr, "go-purifier: asking %s for %s@%s\n", lock.Proxy, modPath, version)
		cmd := exec.Command(goBin, "list", "-m", modPath+"@"+version)
		cmd.Dir = os.TempDir()
		cmd.Env = append(os.Environ(), "GOPROXY="+lock.Proxy, "GOFLAGS=-mod=mod", "GO111MODULE=on", "GOWORK=off")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("the tag is pushed, but %s did not serve %s@%s yet: %w", lock.Proxy, modPath, version, err)
		}
	}
	return nil
}

// modulePath is the module path a go.mod file declares.
func modulePath(gomod string) (string, error) {
	data, err := os.ReadFile(gomod)
	if err != nil {
		return "", err
	}
	path := modfile.ModulePath(data)
	if path == "" {
		return "", fmt.Errorf("%s declares no module path", gomod)
	}
	return path, nil
}

// checkReleaseVersion accepts a canonical semantic version whose major
// version matches the module path's major version suffix.
func checkReleaseVersion(modPath, version string) error {
	if !semver.IsValid(version) || semver.Canonical(version) != version {
		return fmt.Errorf("%q is not a canonical semantic version such as v1.2.3", version)
	}
	if semver.Build(version) != "" {
		return fmt.Errorf("%q has build metadata, which module versions cannot have", version)
	}
	return module.Check(modPath, version)
}

// releaseTag is the tag the go command looks up for a version of a module
// in directory modRel of its repository: the version itself at the root,
// or the directory, a slash and the version. A module in a major version
// subdirectory, such as example.com/m/v2 in v2/, takes the tag of the
// directory above it.
func releaseTag(modRel, modPath, version string) string {
	if modRel == "." || modRel == "" {
		return version
	}
	if _, major, ok := module.SplitPathVersion(modPath); ok && major != "" {
		suffix := strings.TrimPrefix(major, "/")
		if modRel == suffix {
			return version
		}
		modRel = strings.TrimSuffix(modRel, "/"+suffix)
	}
	return modRel + "/" + version
}

// copyDir copies the files of a store directory into dir as ordinary,
// writable files, and returns their paths.
func copyDir(from, to string) ([]string, error) {
	entries, err := os.ReadDir(from)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		src, dst := filepath.Join(from, e.Name()), filepath.Join(to, e.Name())
		in, err := os.Open(src)
		if err != nil {
			return nil, err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err != nil {
			in.Close()
			return nil, err
		}
		_, err = io.Copy(out, in)
		in.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
		files = append(files, dst)
	}
	return files, nil
}

func runIn(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}
