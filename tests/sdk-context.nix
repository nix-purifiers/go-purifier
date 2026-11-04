# SPDX-License-Identifier: MIT
# Evaluate with allow-import-from-derivation=false. The unbuildable multi-output
# SDK proves that reading the declared recipe never realizes SDK outputs.
{
  system ? builtins.currentSystem,
}:
let
  flake = builtins.getFlake (toString ../.);
  pkgs = import flake.inputs.nixpkgs { inherit system; };
  sdk = pkgs.go;
  restore =
    value:
    let
      expression = (import ../nix/lib/sdk-expression.nix value).expression;
    in
    import (
      builtins.toFile "sdk-context-expression.nix" (builtins.unsafeDiscardStringContext expression)
    );
  consume = value: pkgs.runCommand "sdk-context-consumer" { buildInputs = [ value ]; } "touch $out";
  multi = pkgs.runCommand "unbuildable-multi-output-sdk" {
    outputs = [
      "out"
      "dev"
      "secondary"
    ];
  } "exit 1";
  original = multi.secondary;
  restored = restore original;
  contextual = sdk.outPath;
  raw = builtins.storePath (builtins.unsafeDiscardStringContext sdk.outPath);
  rawLiteral = /. + builtins.unsafeDiscardStringContext sdk.outPath;
  rawRestored = restore raw;
in
assert (consume sdk).drvPath == (consume (restore sdk)).drvPath;
assert (consume multi).drvPath == (consume (restore multi)).drvPath;
assert (consume original).drvPath == (consume restored).drvPath;
assert builtins.getContext (toString sdk) == builtins.getContext (toString (restore sdk));
assert builtins.getContext (toString original) == builtins.getContext (toString restored);
assert (consume contextual).drvPath == (consume (restore contextual)).drvPath;
assert system != builtins.currentSystem || (consume raw).drvPath == (consume rawRestored).drvPath;
assert
  system != builtins.currentSystem
  || (consume rawLiteral).drvPath == (consume (restore rawLiteral)).drvPath;
{
  inherit system;
  canonicalDerivation = sdk.drvPath;
  selectedOutput = restored.outputName;
  restoredDerivation = restored.drvPath;
  noSdkBuildRequired = true;
  rawPathFallbackChecked = system == builtins.currentSystem;
}
