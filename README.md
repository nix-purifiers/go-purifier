# go-purifier

go-purifier keeps Go's development workflow on locked,
Nix-resolved dependencies. Use the normal build and test commands in your
working tree; building the project itself in the Nix sandbox is an
optional next step.

## Quickstart

From the project root:

```zsh
nix shell 'github:nix-purifiers/go-purifier'
go-purifier lock
go build ./...
go test ./...
```

Commit the lockfiles that the lock command writes, including the build
tool's own lockfile when it has one. Keep using the project's usual build,
test and editor commands with this setup. Nix provides the locked
dependencies; the development commands above run outside the Nix sandbox.
No project Nix files are needed for this workflow.

Sandboxed builds of the whole project are an optional
[next step](#building-the-project-with-nix).

The shell provides the `go` wrapper. Use it for the locked
development workflow. With no purifier lockfile, it warns on standard
error and runs the ordinary tool with impure resolution.

For persistent use, add these packages through your usual Nix configuration.


## Development loop

The locked proxy also supplies version-only `.info` metadata for locked
modules. This lets native module inspection work from a fresh cache without
inventing upstream release timestamps or adding mutable metadata to the lock.

`nix shell 'github:nix-purifiers/go-purifier'` gives a shell with the
flake's `go`, `gofmt`, gopls and `go-purifier`. Run go as usual. In a
build with a current lockfile, the first go command builds the proxy tree
of the lockfile through Nix, under a GC root in `.go-purifier/` at the
lock root. Every go command then runs with `GOPROXY=file://<that tree>`,
`GOSUMDB=off`, `GOTOOLCHAIN=local`, and a `GONOPROXY` that matches no
module, so `GOPRIVATE` does not send the go command to version control.
`go.sum` still verifies every module.

To change a dependency, run `go get`, `go mod tidy` or another command
that changes `go.mod`. These commands read the locked downloads first and
then your own `GOPROXY`, so they can add modules the lockfile does not hold
yet. The next build stops and says to run `go-purifier lock`; after that
the build reads only the locked downloads again.

The nix-purifiers contract has every command that needs a download
outside the lockfile stop and name `go-purifier lock`. go-purifier does
that for builds, tests and runs. A command that changes the dependencies
downloads through your `GOPROXY` instead, because in Go the command that
changes `go.mod` is the one that chooses the new versions.

`go-purifier update` updates every module the build needs to its latest
minor or patch version with `go get -u -t ./...`, then runs `go mod tidy`
and the lock, so that `go.mod`, `go.sum` and the lockfile change in one
run. `go-purifier update github.com/BurntSushi/toml` updates only the
named modules, with `go get <module>@latest`. In a workspace it updates
each module and then runs `go work sync`.

A build without a lockfile resolves the usual way, and `GO_PURIFIER=0`
turns the locked downloads off.

## Building the project with Nix

Once the native development workflow is working, you can also build and
test the whole project in the Nix sandbox. For this step, keep a flake or
another Nix build definition in the project's repository, or maintain an
external packaging repository that points at its sources and lockfiles.
The builder consumes the same lockfiles as the development workflow.

The flake's `overlays.default` adds one attribute to a package set, the
scope `go-purifier`. The scope holds the builder `buildGoProject`, the
`go-purifier` command and the toolchain package.

A flake that packages the project calls the builder the same way. It can
live in the project's repository or in another one:

```nix
{
  inputs.go-purifier.url =
    "github:nix-purifiers/go-purifier";
  inputs.nixpkgs.follows = "go-purifier/nixpkgs";

  outputs =
    { nixpkgs, go-purifier, ... }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs {
        inherit system;
        overlays = [ go-purifier.overlays.default ];
      };
      build =
        pkgs.go-purifier.buildGoProject { src = ./.; };
    in
    {
      packages.${system}.hello = build.packages.hello;
      checks.${system}.tests = build.tests;
    };
}
```

`buildGoProject` returns these attributes:

- `packages.<name>` is the command of each main package of the main
  modules, named the way `go install` names it.
- `tests` runs `go test` on every package of the main modules in the
  sandbox.
- `release` is the release archives of the commands for the host
  platform, with a `SHA256SUMS` file. `go-purifier publish` uploads it.
- `fetches` is the list of downloads of the build, as entries of the
  shared library of the nix-purifiers, for SBOM, license and vulnerability
  tools.
- `all` is one derivation with every command in `bin/`, which `packages`
  link into, and `proxy` is the module proxy tree of the locked downloads.

`nix/lib/build-go-project.nix` documents the arguments. `modRoot` is the
lock root inside `src`, for a build whose `replace` directives point
outside the lock root; `src` then has to contain those directories.
`fetchers` replaces the fetchers of the shared library, such
as a `fetchurl` with credentials or a company mirror. `version`, `tags`,
`ldflags`, `buildInputs`, `nativeBuildInputs` and `srcFilter` do what
their names say.

`go-purifier.overrideScope` replaces what the scope shares, and
`go-purifier.buildGoProject.override` replaces what the builder takes.
Both build with another go toolchain:

```nix
pkgs.go-purifier.overrideScope (
  final: prev: { go = pkgs.go_1_26; }
)
```

A flake can also keep the build of a project external, such as to
package a project whose repository holds no go-purifier lockfile. The
flake keeps the lockfile outside the tree and passes it as `lockFile`. It
can also keep its own copy of `go.sum` and pass it as `goSum`, which the
build uses in place of the `go.sum` in `src`. `go-purifier lock` and
`go-purifier update` write to those copies with `--lockfile <path>` and
`--tool-lockfile <path>`, or with the variables `GO_PURIFIER_LOCKFILE` and
`GO_PURIFIER_TOOL_LOCKFILE`, and leave the files in the tree as they are.
`lock` starts from the `go.sum` in the tree when the outside copy does not
exist yet. `--tool-lockfile` replaces the `go.sum` of one module, so a
workspace takes only `--lockfile`.

`lib.go-purifier.readLock { src; }` reads the lockfile into the shape all
nix-purifiers share: the download entries, the commands as outputs, and
the problems that keep the lockfile from being used.

### One-off builds without a project Nix file

For an ad hoc build, the builder can also be evaluated directly from the
project root. Replace the example package name with one from your project:

```zsh
nix build --impure --expr 'let
  purifier = builtins.getFlake "github:nix-purifiers/go-purifier";
  pkgs = import purifier.inputs.nixpkgs {
    system = builtins.currentSystem;
    overlays = [ purifier.overlays.default ];
  };
in
(pkgs.go-purifier.buildGoProject { src = ./.; }).packages.hello'
```

This uses the purifier's pinned nixpkgs, matching the lock command above.
To build with ambient `<nixpkgs>` from your Nix search path instead,
replace `import purifier.inputs.nixpkgs` with `import <nixpkgs>`.
Both forms need no Nix files in your project. The build tool and language
versions must match the lockfile; with ambient nixpkgs, lock with the
purifier from that package set or pass a compatible toolchain to the builder.

## Working offline

`go-purifier prefetch` realizes in the Nix store everything the lockfile
lists: the proxy files, the checkouts of modules from version control, and
the toolchain of the development shell. It keeps them alive with GC roots
in `.go-purifier/` at the lock root. After a prefetch, `go build`,
`go test`, `go run` and gopls work with the network cut off. Add
`.go-purifier/` to `.gitignore`.

## Editor

The development shell has gopls. gopls runs the `go` on its `PATH`, so an
editor that starts gopls from the shell resolves from the same locked
downloads, offline after a prefetch. An editor that starts gopls itself
needs the shell's `PATH`, such as through direnv with `use flake`.

## Troubleshooting

Every error names the command that fixes it.

| Mistake | What happens |
|---|---|
| `go.mod`, `go.sum` or `go.work` changed after the lock | `buildGoProject` stops at evaluation, and a build through the `go` wrapper stops, both saying to run `go-purifier lock`. |
| A download is missing from the lockfile | The go command fails to read it from the proxy tree, and both the Nix build and the `go` wrapper say to run `go-purifier lock`. |
| The lockfile was edited by hand | Its `contentHash` no longer matches its content, and both say to run `go-purifier lock`. |
| The lockfile is not valid JSON | Both name the file and say to run `go-purifier lock`. |
| The lockfile has another format version | Both name the version and say to run `go-purifier lock`. |
| There is no lockfile | `buildGoProject` names the path where it looked and says to run `go-purifier lock`. |
| A `replace` directive points outside `src` | The Nix build says to widen `src` and to set `modRoot`. |
| `go.mod` lacks a module that the code imports | `go-purifier lock` stops with the go command's error and says to run `go mod tidy`, then `go-purifier lock` again. |

The last row is an error of the lock run. A missing download and a
`replace` directive outside `src` show when the go command runs, so the
build reports them. The nix-purifiers contract has an incomplete lockfile
fail at Nix evaluation. In go-purifier, an incomplete lockfile fails when
the go command in the build looks for the missing download, because only
the go command knows which modules a build loads. `readLock` reports each
of the other mistakes as an entry of `problems`. A lockfile that is not JSON fails evaluation with
the file named and the command to run, because Nix cannot catch an error
of `builtins.fromJSON`.

## Supported language repositories

`go-purifier lock` runs the go command at the lock root with an empty
`GOPATH`, module cache and build cache, with `-mod=readonly`, and with
every module outside `GONOPROXY` coming from one proxy. It runs
`go list -m all`, which downloads the `go.mod` file of every module in the
module graph, and `go list all`, which downloads every module whose
packages the main modules or their tests import. The lock run keeps the
resolution of `go.mod` as it is, and a lock of an unchanged project
writes the same lockfile byte for byte. When `go.sum` of a module lacks
entries and `go.mod` needs no change, the lock first runs `go mod tidy`,
which adds them and leaves `go.mod` as it is. The nix-purifiers contract
has `lock` complete the build tool's lockfile wherever it is incomplete.
In a workspace, go-purifier leaves `go.sum` as it is, because
`go mod tidy` acts on one module and can resolve it differently from the
workspace. An incomplete `go.sum` in a workspace stops the lock with the
go command's error, which names the `go mod download` command that adds
the missing entry.

The lockfile holds what Nix needs and `go.mod` and `go.sum` cannot give
it, since `go.sum` records hashes of module contents and not of the files
Nix downloads:

- **Files from a module proxy.** The SHA-256 of each `.mod` and `.zip`
  file, by its path under the proxy. The lock proxy is `GO_PURIFIER_PROXY`
  when it is set, or else the first proxy URL in `GOPROXY`, or else
  `https://proxy.golang.org`. Each file comes from the lock proxy alone,
  because two proxies can serve zips with the same content in different
  bytes.
- **Modules from git, Mercurial, Subversion, Bazaar and Fossil.** The go
  command fetches each module that `GONOPROXY` or `GOPRIVATE` matches from
  its repository. The lockfile records the repository, the revision and
  the NAR hash of a checkout, and the Nix build makes the module's `.mod`
  and `.zip` files from the checkout with `go-purifier module-files`,
  following the go command's own rules, which `go.sum` then verifies. An
  SSH URL of a git or Mercurial repository is recorded as the HTTPS URL of
  the same host, since the Nix sandbox has no SSH keys. The lockfile
  records the `GONOPROXY` patterns, and a later lock without `GONOPROXY` or
  `GOPRIVATE` in the environment uses them again.
- **Local directories** that a `replace` directive points at, which come
  from `src`.

The lockfile also records the commands of the build, and the digest of
the files that define the dependencies (`go.work` and `go.work.sum` for a
workspace, and the `go.mod` and `go.sum` of every main module and of every
directory a `replace` directive points at), which tells a stale lockfile.

These limits affect a build:

- **The go toolchain comes from nixpkgs.** Every go command runs with
  `GOTOOLCHAIN=local`, because Go's toolchain downloads are per-platform
  modules and would tie a lockfile to the system that made it. `go.mod`'s
  `go` line has to be at most the version of the toolchain in use;
  `go-purifier.overrideScope` with a newer `go` selects it.
- **`.gitattributes` export rules break a module from git.** The go
  command makes the zip of such a module with `git archive`, which drops
  files marked `export-ignore` and expands `export-subst`. go-purifier
  makes the zip from a checkout, which keeps them, so `go.sum` rejects the
  zip and the build fails with a checksum mismatch.
- **Only the go command downloads.** A step that fetches files itself,
  such as a `go generate` that downloads, fails in the sandbox. C
  libraries for cgo packages come from `buildInputs`.

## Nix project dependencies

Consume another purified Go project without changing its version in `go.mod`:

```zsh
go-purifier lock --input 'github:your-org/library#default'
go run .
go test ./...
```

The lock retains the Nix source pin. Supplied modules contribute no download
hashes to the purifier lock or `go.sum`. The wrapper binds them through
native Go workspaces; `go mod tidy` and `go get` use temporary module files.
Use `go-purifier lock --no-inputs` to return to registry resolution.

`buildGoProject` accepts `dependencies = [ library ];`, or reads the source
pins from the lock. Its result, `all`, command packages, and release expose
`projections.go`: a native Go module proxy with `.zip`, `.mod`, and `.info`
files. Exported `go.mod` files reference Nix-derived dependency versions
and contain no local replacements. Supplied archives are reused unchanged.
Ordinary Go consumers can use this proxy through `GOPROXY`, with the usual
`GONOSUMDB` policy for modules absent from the public checksum database.
The existing `publish` command publishes Git tags and command archives;
serving a projected module proxy is separate.

An ordinary consumer can select a higher version of a projected dependency
through Go's minimal version selection. Exported `go.mod` files cannot impose
a hard source constraint, so the projection does not guarantee that the Nix
source survives a competing native requirement. An explicit purifier input
restores that source binding when the dependency returns to a purified project.

The native graph regression exercises transitive selection, retained indirect
pins, root-only replacements, fresh-cache module inspection, and both kinds of
purifier/ordinary sandwich:

```zsh
python3 -B tests/composition-graphs.py toolchain --state-root .composition-graphs
```

Its report marks the ordinary-consumer source guarantee as blocked. Add
`--require-strict-publication` to make that limitation fail the command; the
default command requires every supported case to pass.

## Examples

For day-to-day work on these projects, use the native-tool setup and
commands in [Quickstart](#quickstart) and [Development loop](#development-loop).
The Nix expressions below show optional sandboxed packaging of those
projects.

### fzf

To build [fzf](https://github.com/junegunn/fzf) from a clone of its
repository, lock it in the clone and build it from there. fzf's Makefile
passes the version to the linker, so the build does the same with
`ldflags`:

```zsh
nix shell 'github:nix-purifiers/go-purifier'
git clone --branch v0.74.5 https://github.com/junegunn/fzf
cd fzf
go-purifier lock
nix build --impure --expr 'let
  purifier = builtins.getFlake "github:nix-purifiers/go-purifier";
  pkgs = import purifier.inputs.nixpkgs {
    system = builtins.currentSystem;
    overlays = [ purifier.overlays.default ];
  };
in
(pkgs.go-purifier.buildGoProject {
  src = ./.;
  ldflags = [ "-X main.version=0.74.5" ];
}).packages.fzf'
```

The result links to `./result/bin/fzf`. Replacing `.packages.fzf` with
`.tests` runs fzf's tests in the sandbox.

To build fzf without changing its repository, keep the lockfile in a
directory of your own. Run the lock in a clone at the tag, and write the
lockfile to that directory:

```zsh
nix shell 'github:nix-purifiers/go-purifier'
git clone --branch v0.74.5 https://github.com/junegunn/fzf
(cd fzf && go-purifier lock \
  --lockfile ~/fzf-build/fzf.lock.json)
cd ~/fzf-build
```

Then build from that directory. The build fetches the source and reads
the lockfile beside it:

```zsh
nix build --impure --expr 'let
  purifier = builtins.getFlake "github:nix-purifiers/go-purifier";
  pkgs = import purifier.inputs.nixpkgs {
    system = builtins.currentSystem;
    overlays = [ purifier.overlays.default ];
  };
in
(pkgs.go-purifier.buildGoProject {
  src = pkgs.fetchFromGitHub {
    owner = "junegunn";
    repo = "fzf";
    rev = "v0.74.5";
    hash = "sha256-+v3oSlf3HBpPkNqS3QxRV/B1yHYb07/a70WYuS0aopY=";
  };
  lockFile = ./fzf.lock.json;
  ldflags = [ "-X main.version=0.74.5" ];
}).packages.fzf'
```

A flake that packages fzf for others calls `pkgs.go-purifier.buildGoProject`
with the same arguments, as in the Quickstart.

## Credits

The purifier idea was inspired by nixcloud's
[cargo (libnix)](https://github.com/nixcloud/cargo).
