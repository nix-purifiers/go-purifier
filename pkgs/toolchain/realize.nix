# SPDX-License-Identifier: MIT
#
# The proxy tree the go wrapper builds for interactive use: every locked
# proxy file at its proxy path, and the files of every module from version
# control, as links into the store, in one derivation. `nix-build` of this
# file fetches every entry in parallel, and its out link is the GC root
# that keeps them all alive.
#
# The entries are fetched with fetchEntry of the shared library of the
# nix-purifiers. A file entry needs only Nix's built-in fetcher; a checkout
# needs the fetchers of the nixpkgs the go-purifier flake was built with,
# so a lockfile with only proxy files never imports its package set. The
# go-purifier command that runs the wrapper is the builder of the tree and
# of the module files.
{
  # The lockfile, as an absolute path.
  lockFile,
  # The go-purifier command, as an absolute path into the store.
  cli,
  # The nixpkgs the go-purifier flake was built with.
  nixpkgs,
  libDir ? ./.,
  # The source of the shared library, which the toolchain links into the
  # go-purifier Nix library as `purifier`.
  purifierLib ? libDir + "/purifier",
}:
let
  lock = import (libDir + "/../../lib-overlays/default/lock.nix") { inherit lockFile; };
  inherit ((import (purifierLib + "/default.nix") { lib = import (nixpkgs + "/lib"); })) fetchEntry;
  fetchFile = fetchEntry { };
  fetchCheckout = fetchEntry { pkgs = import nixpkgs { }; };
  fetch = entry: if entry ? url then fetchFile entry else fetchCheckout entry;
  # The store object of the command, and the path of the command inside
  # it. storePath adds the store object to the string context of the
  # path, so the builds that run the command may read it.
  cliMatch = builtins.match "(${builtins.storeDir}/[^/]+)(/.*)?" cli;
  cliParts =
    if cliMatch == null then
      throw "go-purifier: the go-purifier command ${cli} is not in the Nix store"
    else
      cliMatch;
  cli' =
    builtins.storePath (builtins.elemAt cliParts 0)
    + (
      let
        rest = builtins.elemAt cliParts 1;
      in
      if rest == null then "" else rest
    );
  system = builtins.currentSystem;
  vcsModule = import (libDir + "/../build-go-project/vcs-module.nix") {
    inherit lock fetch system;
    cli = cli';
  };
  gopathLines = map (a: "${a.path}\t${fetch a}\n") (builtins.attrValues lock.checkouts);
  fileLines = map (a: "${a.path}\t${fetch a}\n") lock.files;
  vcsLines = map (
    m:
    let
      files = vcsModule m;
    in
    builtins.concatStringsSep "" (
      map (ext: "${lock.proxyDir m.module}/${m.version}${ext}\t${files}/${m.version}${ext}\n") [
        ".mod"
        ".zip"
      ]
    )
  ) lock.vcsModules;
  infos =
    import (libDir + "/../build-go-project/proxy-info.nix") { lib = import (nixpkgs + "/lib"); }
      (
        map (entry: entry.path) lock.files
        ++ map (m: "${lock.proxyDir m.module}/${m.version}.mod") lock.vcsModules
      );
in
derivation {
  name = "go-purifier-proxy";
  inherit system;
  builder = cli';
  args = [ "link-tree" ];
  manifest = builtins.concatStringsSep "" (
    if lock ? gopath then gopathLines else fileLines ++ vcsLines
  );
  infos = builtins.concatStringsSep "" (
    map (path: "${path}\t${builtins.toJSON infos.${path}}\n") (builtins.attrNames infos)
  );
  passAsFile = [
    "manifest"
    "infos"
  ];
  preferLocalBuild = true;
  allowSubstitutes = false;
}
