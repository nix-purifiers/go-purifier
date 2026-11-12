# SPDX-License-Identifier: MIT
#
# What `go-purifier publish` builds: the release archives of a module's
# commands, from a commit of its repository. builtins.fetchGit takes the
# commit's tracked files only, so the archives hold what the Nix build of
# that commit produces.
{
  # The nixpkgs the go-purifier flake was built with.
  nixpkgs,
  # The repository and the commit to build.
  repo,
  rev,
  # The lock root inside the repository.
  modRoot,
  # The lockfile: one outside the repository, as an absolute path, or one
  # inside it, which the commit holds.
  lockFile,
  version,
  # The source of the shared library, which the toolchain links into the
  # go-purifier Nix library as `purifier`.
  purifierLib ? ./purifier,
}:
let
  pkgs = import nixpkgs { };
  src = builtins.fetchGit {
    url = repo;
    inherit rev;
  };
  inRepo = pkgs.lib.removePrefix (repo + "/") lockFile;
  buildGoProject = pkgs.callPackage ../build-go-project/default.nix {
    purifier = import (purifierLib + "/default.nix") { inherit (pkgs) lib; };
  };
in
(buildGoProject {
  inherit
    src
    modRoot
    version
    ;
  lockFile = if inRepo != lockFile then src + "/${inRepo}" else /. + lockFile;
}).release
