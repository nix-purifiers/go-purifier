# SPDX-License-Identifier: MIT
{
  nixpkgs,
  repo,
  rev,
  rootRel,
  lockFile,
  version,
  goReference,
  purifierLib ? ./purifier,
}:
let
  pkgs = import nixpkgs { };
  checkout = builtins.fetchGit {
    url = repo;
    inherit rev;
  };
  src = if rootRel == "" then checkout else checkout + "/${rootRel}";
  inRepo = pkgs.lib.removePrefix (repo + "/") lockFile;
  builder = pkgs.callPackage ./build-go-path-project.nix {
    go = import (/. + goReference);
    purifier = import (purifierLib + "/default.nix") { inherit (pkgs) lib; };
  };
in
(builder {
  inherit src version;
  lockFile = if inRepo != lockFile then checkout + "/${inRepo}" else /. + lockFile;
}).release
