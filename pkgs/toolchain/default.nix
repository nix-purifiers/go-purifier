# SPDX-License-Identifier: MIT
#
# The go toolchain of the package set, with `go` replaced by go-purifier
# under that name, and gopls. `go` runs the real go against the locked
# downloads of a build that has a lockfile, and the usual way otherwise;
# gopls runs that `go`. `go-purifier` sits beside them, for
# `go-purifier lock`, `prefetch` and `publish`.
#
# Both wrappers take the clients of the version control systems the go
# command fetches modules with (git, Mercurial, Subversion, Breezy for
# Bazaar, and Fossil), nix-prefetch-git for locking git modules, and Nix
# itself, for an editor that starts gopls without Nix on its PATH, from the
# end of PATH, so the tools the user installed come first. Interactive mode,
# the lock of checkouts that are not git checkouts, and
# `go-purifier publish` use the nixpkgs of this package set.
{
  lib,
  runCommand,
  makeWrapper,
  go,
  gopls,
  git,
  mercurial,
  subversion,
  breezy,
  fossil,
  nix-prefetch-git,
  nix,
  path,
  cli,
  # The source of the shared library of the nix-purifiers.
  purifierSrc,
  python3,
  writeShellScript,
  writeText,
}:
let
  # The go-purifier Nix library, with the shared library linked in as
  # `purifier`, for the files the go wrapper evaluates with nix-build.
  nixLib = runCommand "go-purifier-nix-lib" { } ''
    cp -r ${../../nix/lib} $out
    chmod u+w $out
    ln -s ${purifierSrc} $out/purifier
  '';
  inputHelper = writeShellScript "go-purifier-inputs" ''
    export PYTHONPATH=${lib.escapeShellArg ("${purifierSrc}/tools")}
    exec ${python3}/bin/python3 ${../../tools/resolve_inputs.py}
  '';
  sdkReference = writeText "go-sdk-reference.nix" (import ../../nix/lib/sdk-expression.nix go)
  .expression;
  wrapperArgs = lib.escapeShellArgs [
    "--set"
    "GO_PURIFIER_GO_NIX"
    "${sdkReference}"

    "--set"
    "GO_PURIFIER_INPUT_HELPER"
    "${inputHelper}"
    "--set-default"
    "GO_PURIFIER_GO"
    "${go}/bin/go"
    "--set"
    "GO_PURIFIER_NIX_LIB"
    "${nixLib}"
    "--set-default"
    "GO_PURIFIER_NIXPKGS"
    "${path}"
    "--suffix"
    "PATH"
    ":"
    (lib.makeBinPath [
      git
      mercurial
      subversion
      breezy
      fossil
      nix-prefetch-git
      nix
    ])
  ];
in
runCommand "go-purifier-go-${go.version or (lib.getVersion go)}"
  {
    nativeBuildInputs = [ makeWrapper ];
    passthru = { inherit go gopls cli; };
    meta = {
      description = "go with its module downloads from a go-purifier lockfile";
      mainProgram = "go-purifier";
    };
  }
  ''
    mkdir -p "$out/bin" "$out/nix-support"
    ln -s ${sdkReference} "$out/nix-support/project-go-sdk.nix"
    for tool in ${go}/bin/*; do
      name=$(basename "$tool")
      if [ "$name" != go ]; then
        ln -s "$tool" "$out/bin/$name"
      fi
    done
    ln -s ${gopls}/bin/gopls "$out/bin/gopls"
    # GO_PURIFIER_TOOLCHAIN names this package, which `go-purifier prefetch`
    # keeps alive with a GC root.
    makeWrapper ${lib.getExe cli} "$out/bin/go" --argv0 go ${wrapperArgs} --set GO_PURIFIER_TOOLCHAIN "$out"
    makeWrapper ${lib.getExe cli} "$out/bin/go-purifier" ${wrapperArgs} --set GO_PURIFIER_TOOLCHAIN "$out"
  ''
