# SPDX-License-Identifier: MIT
#
# The module proxy of a lockfile, for the sandboxed build: every locked
# proxy file at its proxy path, and the files of every module from version
# control, each as a link into the store. The go command reads it as
# GOPROXY=file://<this>; buildGoProject's `proxy` is this.
{
  lib,
  stdenv,
  runCommand,
  lockFile,
  # The entries of the lock in memory for the build platform, from
  # entriesFor of the shared library.
  entries,
  # The go-purifier command, which makes the files of modules from version
  # control. Only a lockfile with such modules builds it.
  cli,
  # Fetches an entry: fetchEntry of the shared library of the
  # nix-purifiers, with the fetchers the builder was given.
  fetch,
}:
let
  lock = import ../../lib-overlays/default/lock.nix { inherit lockFile; };
  vcsModule = import ./vcs-module.nix {
    inherit lock fetch;
    cli = lib.getExe cli;
    system = stdenv.buildPlatform.system;
  };
  vcsLines = lib.concatMapStrings (
    m:
    let
      files = vcsModule m;
    in
    lib.concatMapStrings
      (ext: "${lock.proxyDir m.module}/${m.version}${ext}\t${files}/${m.version}${ext}\n")
      [
        ".mod"
        ".zip"
      ]
  ) lock.vcsModules;
  infos = import ./proxy-info.nix { inherit lib; } (
    map (entry: entry.path) entries
    ++ map (m: "${lock.proxyDir m.module}/${m.version}.mod") lock.vcsModules
  );
in
# The manifest goes to the builder as a file, since a module with many
# dependencies locks thousands of files and the list would not fit in an
# environment variable.
runCommand "go-module-proxy"
  {
    manifest =
      lib.concatMapStrings (a: "${a.path}\t${fetch a}\n") (builtins.filter (a: a ? url) entries)
      + vcsLines;
    infos = lib.concatMapStrings (path: "${path}\t${builtins.toJSON infos.${path}}\n") (
      builtins.attrNames infos
    );
    passAsFile = [
      "manifest"
      "infos"
    ];
    preferLocalBuild = true;
    allowSubstitutes = false;
    passthru = { inherit lock; };
  }
  ''
    mkdir -p $out
    while IFS=$'\t' read -r rel target; do
      mkdir -p "$out/$(dirname "$rel")"
      ln -s "$target" "$out/$rel"
    done < "$manifestPath"
    while IFS=$'\t' read -r rel metadata; do
      mkdir -p "$out/$(dirname "$rel")"
      printf '%s\n' "$metadata" > "$out/$rel"
    done < "$infosPath"
  ''
