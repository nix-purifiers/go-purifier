# SPDX-License-Identifier: MIT
# Serialize SDK references for an embedded Nix evaluation, retaining the
# original output dependency context and explicit-output selection. Recipes
# needed by that expression are declared runtime references of the wrapper.
sdk:
let
  value = toString sdk;
  context = builtins.getContext value;
  quoteNix = input: builtins.replaceStrings [ "\${" ] [ "\\\${" ] (builtins.toJSON input);
  expression =
    if builtins.isAttrs sdk && sdk ? drvPath then
      "((import (builtins.storePath ${quoteNix (builtins.unsafeDiscardOutputDependency sdk.drvPath)})).${
        quoteNix (sdk.outputName or "out")
      } // { outputSpecified = ${builtins.toJSON (sdk.outputSpecified or false)}; })"
    else if builtins.isPath sdk then
      "(/. + ${quoteNix value})"
    else
      "builtins.appendContext ${quoteNix value} (builtins.fromJSON ${quoteNix (builtins.toJSON context)})";
  escaped = builtins.replaceStrings [ "\\" "\"" "$" "`" ] [ "\\\\" "\\\"" "\\$" "\\`" ] expression;
  retained = builtins.mapAttrs (_: _: { path = true; }) context;
in
{
  expression = builtins.appendContext expression retained;
  shell = builtins.appendContext escaped retained;
}
