# SPDX-License-Identifier: MIT
{ pkgs, build }:
let
  built = build {
    src = ./fixtures/ffi;
    sharedLibraries.proof = "example.com/ffi";
  };
  package = built.libraries.proof;
  advanced = build {
    src = pkgs.runCommand "advanced-go-source" { } ''
      cp -r ${./fixtures/ffi} "$out"
      chmod -R u+w "$out"
      substituteInPlace "$out/main.go" --replace-fail 'C.int(42)' 'C.int(43)'
    '';
    lockFile = ./fixtures/ffi/go-purifier.lock.json;
    goSum = ./fixtures/ffi/go.sum;
    sharedLibraries.proof = "example.com/ffi";
  };
in
assert !((build { src = ./fixtures/ffi; }).packages.ffi.projections ? native);
assert
  !(builtins.tryEval (
    (build {
      src = ./fixtures/ffi;
      sharedLibraries.proof = "example.com/outside-lock";
    }).libraries
  )).success;
assert built.fetches == [ ];
assert package.projections.native.drvPath != advanced.libraries.proof.projections.native.drvPath;
pkgs.runCommand "go-c-shared-ordinary-consumers"
  {
    nativeBuildInputs = [
      pkgs.stdenv.cc
      pkgs.python3
    ];
  }
  ''
    test -e ${
      import ./native-consumer.nix {
        inherit pkgs;
        projection = package.projections;
        library = "proof";
        symbol = "answer";
      }
    }
    PYTHONPATH=${package.projections.python-ffi} python - <<'PY'
    import runpy, purifier_native
    original = purifier_native.load('proof')
    assert original.answer() == 42
    changed = runpy.run_path('${advanced.libraries.proof.projections.python-ffi}/purifier_native.py')
    assert changed['load']('proof').answer() == 43
    assert original.answer() == 42
    PY
    cat > ordinary.c <<'C'
    #include <libproof.h>
    int main(void) { return answer() == 42 ? 0 : 1; }
    C
    $CC ordinary.c -I${package}/include -L${package}/lib -lproof -Wl,-rpath,${package}/lib -o ordinary
    ${pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isDarwin ''
      otool -D ${package}/lib/libproof.dylib | tail -n +2 | grep -Fx ${package}/lib/libproof.dylib
      otool -L ./ordinary | grep -F ${package}/lib/libproof.dylib
    ''}
    ./ordinary
    touch $out
  ''
