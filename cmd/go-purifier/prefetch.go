// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// prefetchCommand realizes, in the Nix store, everything day-to-day work on
// the build in the current directory needs: the proxy tree of the locked
// downloads, which holds what building, testing and the editor read, and
// the toolchain, gopls with it. Each gets a GC root in the state directory
// at the lock root, so that afterwards `go build`, `go test`, `go run` and
// gopls work with the network cut off.
func prefetchCommand(args []string) error {
	lockFlag, args, err := lockfileFlag(args)
	if err != nil {
		return err
	}
	if len(args) > 0 {
		return errors.New("usage: go-purifier prefetch [--lockfile <path>]")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if root, lock, err := findGopathRoot(cwd); err != nil {
		return err
	} else if lock != nil {
		goBin, err := realGo()
		if err != nil {
			return err
		}
		if _, err := gopathEnvironment(goBin, root, lock); err != nil {
			return err
		}
		if err := pinGopathToolchain(root); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "go-purifier: prefetched native GOPATH checkouts and toolchain")
		return nil
	}
	root, ok := findLockRoot(cwd)
	if !ok {
		return errors.New("the current directory is not inside a Go module or workspace")
	}
	lockPath := lockPathAt(root, lockFlag)
	lock, err := readLock(lockPath)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s does not exist; run `go-purifier lock` first", lockPath)
	}
	if err != nil {
		return err
	}
	if lock.Version != lockFormatVersion {
		return fmt.Errorf("%s has lock format version %d and this go-purifier reads version %d; run `go-purifier lock`", lockPath, lock.Version, lockFormatVersion)
	}
	if current, err := definitionHash(root, lock.DefinitionFiles, toolLockOverrides()); err != nil {
		return err
	} else if current != lock.DefinitionHash {
		return fmt.Errorf("go.mod, go.sum or go.work changed since %s was written; run `go-purifier lock` first", lockPath)
	}
	if _, err := realize(root, lockPath); err != nil {
		return err
	}

	if err := pinGopathToolchain(root); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "go-purifier: prefetched the downloads of %s and the toolchain\n", lockPath)
	return nil
}

func pinGopathToolchain(root string) error {
	dir := filepath.Join(root, stateDirName)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	roots := map[string]string{"go-purifier": self}
	if goBin, err := realGo(); err == nil {
		roots["go"] = goBin
	}
	if toolchain := os.Getenv("GO_PURIFIER_TOOLCHAIN"); toolchain != "" {
		roots["toolchain"] = toolchain
	}
	for name, path := range roots {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		storePath, ok := storeObject(resolved)
		if !ok {
			continue
		}
		if _, err := output("", "nix-store", "--add-root", filepath.Join(dir, name), "--realise", storePath); err != nil {
			return err
		}
	}
	return nil
}

// storeObject is the store object a path lies in, such as
// /nix/store/<hash>-go-1.26.10 for /nix/store/<hash>-go-1.26.10/bin/go.
func storeObject(path string) (string, bool) {
	const store = "/nix/store/"
	if !strings.HasPrefix(path, store) {
		return "", false
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(path, store), "/")
	return store + name, name != ""
}
