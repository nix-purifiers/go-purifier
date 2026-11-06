# SPDX-License-Identifier: MIT
# Both upstream-supported release lines, using the repository's pinned SDKs.
{
  pkgs,
  overlay,
  readLock,
}:
let
  lib = pkgs.lib;
  versions = {
    "1-26" = pkgs.go_1_26;
    "1-27" = pkgs.go_1_27;
  };
  perVersion =
    label: sdk:
    let
      selectedOverlay =
        final: prev:
        let
          original = overlay final prev;
        in
        original
        // {
          go-purifier = original.go-purifier.overrideScope (_: _: { go = sdk; });
        };
      checks = import ./default.nix {
        inherit pkgs readLock;
        overlay = selectedOverlay;
        supportedVersions = false;
      };
      selected = lib.getAttrs [
        "fixture-hello-greet"
        "fixture-hello-tests"
        "fixture-workspace"
        "fixture-workspace-tests"
        "fixture-replace"
        "fixture-vcs"
        "real-world-fzf"
        "real-world-fzf-version"
        "real-world-fzf-tests"
        "cli-tests"
      ] checks;
    in
    {
      "supported-go-${label}-tools" = checks.toolchain;
      "supported-go-${label}" =
        pkgs.runCommand "supported-go-${label}"
          {
            passthru = { inherit sdk; };
          }
          ''
            test "$(${sdk}/bin/go version | cut -d' ' -f3)" = "go${sdk.version}"
            ${lib.concatMapStringsSep "\n" (check: "test -e ${check}") (builtins.attrValues selected)}
            printf '%s\n' 'Go ${sdk.version}: locked fixtures, fzf and purifier tests passed' > "$out"
          '';
    };
in
lib.foldlAttrs (
  acc: label: sdk:
  acc // perVersion label sdk
) { } versions
