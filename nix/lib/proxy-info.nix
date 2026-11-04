# Go's proxy protocol requires a version in .info, but Time is optional.
# Derive only what the immutable lock knows; cache .info bytes and upstream
# release timestamps are not part of the download integrity contract.
{ lib }:
paths:
let
  lower = lib.stringToCharacters "abcdefghijklmnopqrstuvwxyz";
  unescape = lib.replaceStrings (map (c: "!${c}") lower) (map lib.toUpper lower);
  modules = builtins.filter (path: lib.hasInfix "/@v/" path && lib.hasSuffix ".mod" path) paths;
in
builtins.listToAttrs (
  map (path: {
    name = lib.removeSuffix ".mod" path + ".info";
    value.Version = unescape (lib.removeSuffix ".mod" (baseNameOf path));
  }) modules
)
