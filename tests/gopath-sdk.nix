# SPDX-License-Identifier: MIT
{ pkgs }:
let
  source = builtins.fetchTarball {
    url = "https://github.com/NixOS/nixpkgs/archive/770822493e3962d795739fafd522e771bfe06e3d.tar.gz";
    sha256 = "sha256-rw241C89ocMlDTYP9c2fAb06Yf4Khpjy5PRnoPEb/qQ=";
  };
  old = import source {
    system = "x86_64-linux";
    config = { };
  };
in
old.go_1_0
