// SPDX-License-Identifier: MIT

package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The lock root is the directory the go command treats as the root of the
// build: the directory of go.work in workspace mode, or else the directory
// of the main module's go.mod. The lockfile sits there.

// findLockRoot is the lock root for a go command run in dir, found the way
// the go command finds go.work and go.mod. GOWORK=off disables workspace
// mode, and GOWORK set to a file names the workspace.
func findLockRoot(dir string) (string, bool) {
	switch gowork := os.Getenv("GOWORK"); {
	case gowork == "off":
	case gowork != "" && gowork != "auto":
		if !filepath.IsAbs(gowork) {
			gowork = filepath.Join(dir, gowork)
		}
		return filepath.Dir(gowork), true
	default:
		if root, ok := findUp(dir, "go.work"); ok {
			return root, true
		}
	}
	return findUp(dir, "go.mod")
}

// lockfileFlag takes a `--lockfile <path>` or `--lockfile=<path>` flag out
// of a command's arguments. A lockfile outside the source tree lets a
// project whose author does not use go-purifier be locked and built.
func lockfileFlag(args []string) (string, []string, error) {
	return pathFlag("--lockfile", args)
}

// toolLockfileFlag takes --tool-lockfile <path> out of the arguments of a
// command: the go.sum of an external build, kept outside the source tree.
func toolLockfileFlag(args []string) (string, []string, error) {
	return pathFlag("--tool-lockfile", args)
}

// pathFlag takes the flag name and its path out of args, as an absolute
// path, and returns the other arguments.
func pathFlag(name string, args []string) (string, []string, error) {
	var path string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == name:
			if i+1 == len(args) {
				return "", nil, fmt.Errorf("%s needs a path", name)
			}
			path = args[i+1]
			i++
		case strings.HasPrefix(a, name+"="):
			path = strings.TrimPrefix(a, name+"=")
		default:
			rest = append(rest, a)
		}
	}
	if path != "" && !filepath.IsAbs(path) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", nil, err
		}
		path = filepath.Join(cwd, path)
	}
	return path, rest, nil
}

// lockPathAt is the lockfile of the build at a lock root: the one a flag
// or GO_PURIFIER_LOCKFILE names, or go-purifier.lock.json at the root.
func lockPathAt(root, flag string) string {
	if flag != "" {
		return flag
	}
	if env := os.Getenv("GO_PURIFIER_LOCKFILE"); env != "" {
		if filepath.IsAbs(env) {
			return env
		}
		if cwd, err := os.Getwd(); err == nil {
			return filepath.Join(cwd, env)
		}
	}
	return filepath.Join(root, lockFileName)
}

// toolLockPath is the go.sum of an external build: the one a flag or
// GO_PURIFIER_TOOL_LOCKFILE names, or "" for the go.sum in the source
// tree.
func toolLockPath(flag string) string {
	if flag != "" {
		return flag
	}
	if env := os.Getenv("GO_PURIFIER_TOOL_LOCKFILE"); env != "" {
		if filepath.IsAbs(env) {
			return env
		}
		if cwd, err := os.Getwd(); err == nil {
			return filepath.Join(cwd, env)
		}
	}
	return ""
}

// toolLockOverrides maps the definition file go.sum to the go.sum that
// GO_PURIFIER_TOOL_LOCKFILE names, for the digest of definitionHash, or is
// empty without it.
func toolLockOverrides() map[string]string {
	if path := toolLockPath(""); path != "" {
		return map[string]string{"go.sum": path}
	}
	return nil
}

// findUp is the nearest directory at or above dir that holds a file name.
func findUp(dir, name string) (string, bool) {
	for {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// definitionHash is a digest, in SRI form, of the files that define a
// build's dependencies, given relative to the lock root. It is the SHA-256
// of a line per file, in sorted order: the file's path, a space, and the
// file's SHA-256 in hex, or "absent" for a missing file. lib-overlays/default/lock.nix
// computes the same digest with builtins.hashFile.
func definitionHash(root string, files []string, overrides map[string]string) (string, error) {
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	h := sha256.New()
	for _, name := range sorted {
		path := filepath.Join(root, filepath.FromSlash(name))
		if override, ok := overrides[name]; ok {
			path = override
		}
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			fmt.Fprintf(h, "%s absent\n", name)
		case err != nil:
			return "", err
		default:
			fmt.Fprintf(h, "%s %x\n", name, sha256.Sum256(data))
		}
	}
	return sri(h.Sum(nil)), nil
}

// definitionFiles are the files that define the dependencies of a build
// whose module directories (workspace modules and local replacements) are
// dirs: go.work and go.work.sum in workspace mode, and the go.mod and
// go.sum of every module directory. Paths are relative to the lock root.
func definitionFiles(root string, workspace bool, dirs []string) ([]string, error) {
	seen := map[string]bool{}
	var files []string
	add := func(rel string) {
		if !seen[rel] {
			seen[rel] = true
			files = append(files, rel)
		}
	}
	if workspace {
		add("go.work")
		add("go.work.sum")
	}
	for _, dir := range dirs {
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return nil, err
		}
		add(filepath.ToSlash(filepath.Join(rel, "go.mod")))
		add(filepath.ToSlash(filepath.Join(rel, "go.sum")))
	}
	sort.Strings(files)
	return files, nil
}
