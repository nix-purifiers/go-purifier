# SPDX-License-Identifier: MIT
{
  lib,
  stdenv,
  runCommand,
  go,
  python3,
  purifier,
  fetchurl,
  fetchgit,
  writeText,
}:
{
  src,
  lockFile ? src + "/go-purifier.lock.json",
  version ? "dev",
  fetchers ? { },
  nativeBuildInputs ? [ ],
  buildInputs ? [ ],
  cgoEnabled ? null,
  srcFilter ? null,
}:
let
  lock = import ./lock.nix { inherit lockFile; };
  layout =
    lock.gopath
      or (throw "go-purifier: this builder needs a GOPATH lock; run go-purifier lock --gopath");
  validImport =
    layout.importPath != ""
    && !(lib.hasPrefix "/" layout.importPath)
    && lib.all (
      part:
      !(builtins.elem part [
        ""
        "."
        ".."
      ])
    ) (lib.splitString "/" layout.importPath);
  cgoMode =
    if cgoEnabled == null then
      layout.cgoEnabled or "0"
    else if cgoEnabled then
      "1"
    else
      "0";
  source =
    assert lib.assertMsg validImport "go-purifier: invalid GOPATH project import path";
    if builtins.isPath src then
      builtins.path {
        path = src;
        name = "go-gopath-source";
        filter =
          if srcFilter != null then
            srcFilter
          else
            path: type:
            !(
              builtins.elem (baseNameOf path) [
                ".git"
                ".go-purifier"
                "dist"
              ]
              || (type == "symlink" && lib.hasPrefix "result" (baseNameOf path))
            );
      }
    else
      src;
  dependencies = purifier.tree (
    {
      inherit runCommand fetchurl fetchgit;
      name = "go-gopath-dependencies";
      entries = builtins.attrValues lock.checkouts;
    }
    // fetchers
  );
  repository = runCommand "go-gopath-source-repository" { } ''
    mkdir -p "$out/src"
    cp -rs ${dependencies}/. "$out/"
    mkdir -p "$out/src/$(dirname ${lib.escapeShellArg layout.importPath})"
    ln -s ${source} "$out/src/"${lib.escapeShellArg layout.importPath}
  '';
  build = stdenv.mkDerivation {
    name = "go-gopath-project";
    src = source;
    nativeBuildInputs = [ go ] ++ nativeBuildInputs;
    inherit buildInputs;
    dontConfigure = true;
    buildPhase = ''
      runHook preBuild
      export HOME="$TMPDIR/home" GOPATH="$TMPDIR/gopath" GOCACHE="$TMPDIR/cache"
      export GO111MODULE=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off CGO_ENABLED=${lib.escapeShellArg cgoMode}
      mkdir -p "$HOME" "$GOPATH/src"
      cp -rL ${repository}/. "$GOPATH/"
      chmod -R u+w "$GOPATH"
      cd "$GOPATH/src/"${lib.escapeShellArg layout.importPath}
      go version > "$TMPDIR/target-go-version"
      # Modern Go embeds absolute build paths unless asked to trim them.
      # Original Go has no such flag, so query the selected native SDK.
      buildFlags=()
      help=$(go help build)
      case "$help" in *-trimpath*) buildFlags+=(-trimpath) ;; esac
      go test "''${buildFlags[@]}" ./...
      mkdir -p "$TMPDIR/bin"
      ${lib.concatMapStrings (binary: ''
        go build "''${buildFlags[@]}" -o "$TMPDIR/bin/"${lib.escapeShellArg binary.name} ${lib.escapeShellArg binary.package}
      '') lock.binaries}
      runHook postBuild
    '';
    installPhase = ''
      mkdir -p "$out/bin" "$out/nix-support"
      cp -r "$TMPDIR/bin/." "$out/bin/"
      cp "$TMPDIR/target-go-version" "$out/nix-support/target-go-version"
    '';
    passthru = { inherit repository; };
  };
  release = runCommand "go-gopath-release" { } ''
    mkdir -p "$out"
    ${lib.concatMapStrings (binary: ''
      tar --sort=name --mtime=@1 --owner=0 --group=0 --numeric-owner \
        -C ${build}/bin -cf - ${lib.escapeShellArg binary.name} | gzip -n > "$out/"${lib.escapeShellArg "${binary.name}-${version}-${stdenv.hostPlatform.system}.tar.gz"}
    '') lock.binaries}
    (cd "$out"; find . -maxdepth 1 -type f -name '*.tar.gz' -printf '%f\n' | sort | xargs -r sha256sum > SHA256SUMS)
  '';
in
{
  inherit release repository lock;
  fetches = lock.entries;
  all = build;
  tests = build;
  packages = lib.listToAttrs (
    map (
      binary:
      lib.nameValuePair binary.name (
        runCommand "go-gopath-${binary.name}"
          {
            passthru = {
              inherit repository;
              projections.gopath = repository;
            };
            meta.mainProgram = binary.name;
          }
          ''
            mkdir -p "$out/bin"
            ln -s ${build}/bin/${lib.escapeShellArg binary.name} "$out/bin/"${lib.escapeShellArg binary.name}
          ''
      )
    ) lock.binaries
  );
  projections.gopath = repository;
}
