# Commands

All from the repository root.

```zsh
# Unit tests of the go-purifier command
go test ./...

# Every check in the sandbox: the conformance checks the sandbox can run,
# each fixture built from its lockfile and run, readLock, the release
# archives, the command with its unit tests, and formatting
nix flake check

# Evaluate the checks for aarch64-linux as well
nix flake check --all-systems --no-build

# The checks that need Nix or the network, and so run outside the sandbox
nix build '.#toolchain' --out-link toolchain
tests/interactive.sh toolchain/bin   # conformance-offline
tests/update.sh toolchain/bin        # conformance-update
tests/negative.sh toolchain/bin
tests/release.sh toolchain/bin

# Modules from Mercurial, Subversion, Bazaar and Fossil repositories, which
# the script serves on this host. Where the Nix daemon cannot reach this
# host's loopback network, VCS_SYSTEMS_SKIP_NIX=1 runs only the check of
# the module zips against go.sum.
tests/vcs-systems.sh toolchain/bin

# Lock this repository and the fixtures again (needs the network; the vcs
# fixture also reaches github.com with git), then write the conformance
# fixtures from the hello fixture
for dir in . tests/fixtures/hello tests/fixtures/workspace tests/fixtures/replace/app tests/fixtures/vcs; do
  (cd $dir && $OLDPWD/toolchain/bin/go-purifier lock)
done
tests/conformance-fixtures.sh

nix fmt
```

After a change to `go.mod` of this repository or of a fixture, lock it
again, or the build of `cli` or of the fixture fails for want of a locked
file. After a change to the hello fixture, run
`tests/conformance-fixtures.sh`.

## The fixtures

| Fixture | What it covers |
|---|---|
| `hello` | proxy modules, a case-encoded module path, a dependency only tests load |
| `workspace` | a `go.work` workspace of two modules |
| `replace` | a `replace` directive to a directory next to the lock root (`modRoot = "app"`) |
| `vcs` | modules fetched from git through `GONOPROXY`, one in a repository subdirectory |
| `real-world/fzf` | the lockfile of fzf v0.74.5, which `tests/default.nix` builds from its GitHub source; lock it again from a clone of the tag with `go-purifier lock --lockfile <this repository>/tests/real-world/fzf/go-purifier.lock.json` |
| `conformance/*` | copies of `hello` with a stale, hand-edited, incomplete, malformed, missing or foreign-version lockfile, written by `tests/conformance-fixtures.sh` |

`tests/vcs-systems.sh` makes its Mercurial, Subversion, Bazaar and Fossil
modules when it runs, since they need servers.

## CI

`.github/workflows/check.yml` runs on every pull request and every push to
main, on x86_64-linux, aarch64-linux and aarch64-darwin. The `flake-check`
job runs `nix flake check`. The `conformance-relock`, `conformance-rebuild`,
`conformance-offline` and `conformance-update` jobs run the conformance
checks of the purifier contract that need the network or the Nix daemon.
The `outside-the-sandbox` job runs `tests/negative.sh`,
`tests/release.sh` and `tests/vcs-systems.sh`.
