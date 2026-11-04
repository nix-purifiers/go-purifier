# SPDX-License-Identifier: MIT
{
  system ? builtins.currentSystem,
}:
let
  flake = builtins.getFlake (toString ../.);
  pkgs = import flake.inputs.nixpkgs { inherit system; };
  package = builtins.derivation {
    name = "sdk-context-must-remain-unbuilt";
    inherit system;
    builder = "/sdk-context-output-must-not-build";
    outputs = [
      "out"
      "secondary"
    ];
  };
  sdk = package.secondary // {
    outputSpecified = true;
  };
in
{
  carrier = pkgs.writeText "sdk-reference-expression.nix" (import ../nix/lib/sdk-expression.nix sdk)
  .expression;
  stringReference = import ../nix/lib/sdk-expression.nix sdk.outPath;
  runtimeShell = pkgs.runtimeShell;
  descriptor = builtins.unsafeDiscardStringContext sdk.drvPath;
  expectedOutput = builtins.unsafeDiscardStringContext sdk.outPath;
}
