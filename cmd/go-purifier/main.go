// SPDX-License-Identifier: MIT

// Command go-purifier makes Nix the dependency backend of a Go build.
//
// `go-purifier lock` runs the build once with an empty module cache and
// records every file the go command downloads in go-purifier.lock.json,
// next to go.mod, or next to go.work for a workspace. The Nix library of
// this repository (nix/lib) builds from that lockfile in the Nix sandbox,
// offline.
//
// Invoked as `go` (the flake's `toolchain` package links it under that
// name), the same binary runs the real go command. When the build has a
// current lockfile, it first realizes the locked files through Nix and
// points GOPROXY at them, so interactive builds resolve offline from
// exactly the files the Nix build uses.
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const usageText = `usage: go-purifier <command> [--lockfile <path>]

Commands:
  lock               write go-purifier.lock.json for the module or
                     workspace of the current directory, from go.mod and
                     go.sum as they are
  lock --gopath      capture the native GOPATH graph and Git checkout revisions
  update [<module>...]
                     update every module the build needs to its latest
                     version, or only the named modules, with go get and
                     go mod tidy, then lock
  prefetch           realize the locked downloads and the toolchain in the
                     Nix store, with GC roots in .go-purifier/ at the lock
                     root, so that go and gopls work with no network
  publish <version>  build the release archives of the module's commands
                     through Nix, tag the release, push the tag, create a
                     GitHub release and ask the module proxy for the
                     version; --dry-run stops after the tag
  go ...             run the go command against the locked downloads; the
                     same as invoking this binary as "go"

lock accepts repeated --input <flake#package> options for Nix-built Go
projects. Their source pins are retained on subsequent locks; their modules
are excluded from download hashes. Native workspace bindings select them
regardless of the upstream versions in go.mod. --no-inputs removes all such
pins and returns to normal module resolution.

--lockfile names a lockfile outside the source tree. The go command takes
it from GO_PURIFIER_LOCKFILE. For lock and update, --tool-lockfile names a
go.sum outside the source tree, which they write in place of the go.sum
in the tree; the go command takes it from GO_PURIFIER_TOOL_LOCKFILE for
its check of the lockfile.

Environment:
  GO_PURIFIER_GO       the go command to run (the flake's wrapper sets it)
  GO_PURIFIER_NIX_LIB  the go-purifier Nix library (the flake's wrapper
                       sets it)
  GO_PURIFIER_PROXY    the module proxy a lock downloads from; the default
                       is the first proxy URL in GOPROXY, or
                       https://proxy.golang.org
  GO_PURIFIER_LOCKFILE the lockfile, when it lies outside the source tree
  GO_PURIFIER_TOOL_LOCKFILE
                       the go.sum, when it lies outside the source tree
  GONOPROXY, GOPRIVATE the modules a lock fetches from version control
  GO_PURIFIER=0        run go without the locked downloads
`

func main() {
	if filepath.Base(os.Args[0]) == "go" {
		os.Exit(runGo(os.Args[1:]))
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "lock":
		err = lockCommand(os.Args[2:])
	case "update":
		err = updateCommand(os.Args[2:])
	case "prefetch":
		err = prefetchCommand(os.Args[2:])
	case "publish":
		err = publishCommand(os.Args[2:])
	case "go":
		os.Exit(runGo(os.Args[2:]))
	// The builders of the Nix derivations nix/lib makes; see vcs.go and
	// wrapper.go.
	case "bind-module-inputs":
		if len(os.Args) < 5 {
			err = fmt.Errorf("usage: bind-module-inputs <root> <go.work> <repository>...")
		} else {
			err = bindModuleInputs(os.Args[4:], os.Args[2], os.Args[3])
		}
	case "project-modules":
		if len(os.Args) != 4 {
			err = fmt.Errorf("usage: project-modules <request> <out>")
		} else {
			err = projectModules(os.Args[2], os.Args[3])
		}
	case "module-files":
		err = moduleFilesCommand(os.Args[2:])
	case "link-tree":
		err = linkTreeCommand()
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return
	default:
		fmt.Fprintf(os.Stderr, "go-purifier: unknown command %q\n\n%s", os.Args[1], usageText)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "go-purifier: %v\n", err)
		os.Exit(1)
	}
}
