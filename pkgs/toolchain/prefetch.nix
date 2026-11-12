# SPDX-License-Identifier: MIT
#
# What `go-purifier lock` builds to learn the NAR hash of a Mercurial,
# Subversion, Bazaar or Fossil checkout: the entry, fetched with fetchEntry
# of the shared library of the nix-purifiers, which uses the nixpkgs
# fetcher a build uses, and a placeholder hash. The build fails with a hash
# mismatch that reports the hash of the checkout.
{
  # The nixpkgs the go-purifier flake was built with.
  nixpkgs,
  # The entry, as JSON, without its hash.
  entry,
  # The source of the shared library, which the toolchain links into the
  # go-purifier Nix library as `purifier`.
  purifierLib ? ./purifier,
}:
let
  pkgs = import nixpkgs { };
  inherit ((import (purifierLib + "/default.nix") { inherit (pkgs) lib; })) fetchEntry;
in
fetchEntry { inherit pkgs; } (builtins.fromJSON entry // { hash = pkgs.lib.fakeHash; })
