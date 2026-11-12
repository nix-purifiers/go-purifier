# SPDX-License-Identifier: MIT
#
# The packages of the `go-purifier` scope, which caisson makes with
# lib.makeScope. caisson passes the scope's callPackage, and the composed
# library as `lib`, whose `purifier` namespace has the functions of the
# shared library of the nix-purifiers and whose `go-purifier` namespace has
# the source of that library.
{
  callPackage,
  lib,
  ...
}:
{
  # The builder of a Go module or workspace from its lockfile
  # (pkgs/build-go-project/default.nix). It takes go, the fetchers and the other
  # packages it builds with from the scope, so overrideScope on the scope
  # and .override on the builder replace them.
  buildGoPathProject = callPackage ./build-go-path-project/default.nix { inherit (lib) purifier; };
  buildGoProject = callPackage ./build-go-project/default.nix { inherit (lib) purifier; };
  # The go-purifier command, built by buildGoProject from this
  # repository's own lockfile.
  cli = callPackage ./cli { };
  # The go toolchain with `go` running through go-purifier, gopls, and the
  # go-purifier command beside them.
  toolchain = callPackage ./toolchain {
    purifierSrc = lib.go-purifier.sharedLibrarySource;
  };
}
