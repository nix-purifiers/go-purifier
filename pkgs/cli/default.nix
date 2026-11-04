# SPDX-License-Identifier: MIT
{
  lib,
  buildGoProject,
  pkgs,
  helperGo ? pkgs.go,
}:
let
  root = ../..;
in
((buildGoProject.override { go = helperGo; }) {
  src = lib.fileset.toSource {
    inherit root;
    fileset = lib.fileset.unions [
      (root + "/go.mod")
      (root + "/go.sum")
      (root + "/go-purifier.lock.json")
      (lib.fileset.fileFilter (file: file.hasExt "go") (root + "/cmd"))
    ];
  };
}).packages.go-purifier
