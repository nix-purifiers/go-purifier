# SPDX-License-Identifier: MIT
#
# readLock: the lock in memory of the purifier contract (CONTRACT.md of
# the shared library of the nix-purifiers). It reads go.mod, go.sum and
# the go-purifier lockfile into the shape every purifier shares:
#
#   purifier  "go-purifier"
#   entries   the download entries of the shared library: a file entry per
#             proxy file, and a checkout entry per repository of a module
#             from version control. Every entry is needed on every system.
#   outputs   a set per command of the main modules, with the import path
#             of its main package as `package`
#   problems  why the lockfile cannot be used, each naming the command
#             that fixes it
#
# The flake publishes it as lib.go-purifier.readLock. It needs no nixpkgs.
{
  # The source tree that holds the lock root.
  src,
  # The lock root inside src, or "" when src is the lock root.
  modRoot ? "",
  lockFile ? (if modRoot == "" then src else src + "/${modRoot}") + "/go-purifier.lock.json",
  # The go.sum of the lock root. An external build can pass a copy kept
  # outside src, which takes the place of the go.sum in src.
  goSum ? (if modRoot == "" then src else src + "/${modRoot}") + "/go.sum",
}:
let
  lock = import ./lock.nix {
    inherit lockFile;
    check = false;
  };

  # A source tree given as a path is read at evaluation, to find a lockfile
  # older than go.mod or go.sum. A source tree that is the output of a
  # derivation is not read at evaluation.
  stale =
    if builtins.isPath src then
      lock.staleProblems {
        root = if modRoot == "" then src else src + "/${modRoot}";
        overrides."go.sum" = goSum;
      }
    else
      [ ];
in
{
  purifier = "go-purifier";
  inherit (lock) entries;
  outputs = builtins.listToAttrs (
    map (b: {
      inherit (b) name;
      value = { inherit (b) package; };
    }) lock.binaries
  );
  problems = lock.problems ++ stale;
}
