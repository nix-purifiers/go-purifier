# SPDX-License-Identifier: MIT
# Explicit diagnostic target, deliberately not added to the default checks.
{ pkgs, build }:
let
  extension = pkgs.stdenv.hostPlatform.extensions.sharedLibrary;
  producer = build {
    src = ./fixtures/ffi;
    sharedLibraries.proof = "example.com/ffi";
  };
  ordinary =
    pkgs.runCommand "ordinary-c-lifecycle-control" { nativeBuildInputs = [ pkgs.stdenv.cc ]; }
      ''
        mkdir -p "$out/lib"
        printf 'int answer(void) { return 42; }\n' > ordinary.c
        $CC ${if pkgs.stdenv.hostPlatform.isDarwin then "-dynamiclib" else "-shared"} -fPIC \
          ordinary.c -o "$out/lib/libcontrol${extension}"
      '';
in
pkgs.runCommand "ffi-lifecycle-diagnostics"
  {
    nativeBuildInputs = [
      pkgs.python3
      pkgs.jdk
    ];
    passthru.controllerCheck =
      pkgs.runCommand "ffi-lifecycle-controller-check"
        {
          nativeBuildInputs = [
            pkgs.python3
            pkgs.jdk
          ];
        }
        ''
          python ${./ffi_lifecycle_controller_check.py} \
            --controller ${./ffi_lifecycle_controls.py} \
            --java ${pkgs.jdk}/bin/java --javac ${pkgs.jdk}/bin/javac --jcmd ${pkgs.jdk}/bin/jcmd \
            --output "$out"
        '';
  }
  ''
    python ${./ffi_lifecycle_controls.py} \
      --java ${pkgs.jdk}/bin/java --javac ${pkgs.jdk}/bin/javac --jcmd ${pkgs.jdk}/bin/jcmd \
      --java-major ${pkgs.lib.versions.major pkgs.jdk.version} \
      --c-library ${ordinary}/lib/libcontrol${extension} \
      --go-library ${producer.libraries.proof}/lib/libproof${extension} \
      --sdk-description '${pkgs.stdenv.hostPlatform.system}; Go ${pkgs.go.version}; JDK ${pkgs.jdk.version}' \
      --output "$out"
  ''
