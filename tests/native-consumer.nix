# SPDX-License-Identifier: MIT
{
  pkgs,
  projection,
  library,
  symbol,
}:
pkgs.runCommand "ordinary-native-foreign-consumers"
  {
    nativeBuildInputs = [
      pkgs.python3
      pkgs.jdk
      pkgs.coreutils
    ];
  }
  ''
    boundedStage() {
      local stage=$1
      shift
      printf 'NATIVE-STAGE %s:start\n' "$stage"
      set +e
      timeout --signal=TERM --kill-after=5s 90s "$@"
      local status=$?
      set -e
      printf 'NATIVE-STAGE %s:exit:%s\n' "$stage" "$status"
      return "$status"
    }

    export PYTHONPATH=${projection.python-ffi}
    boundedStage python-consumer python - <<'PY'
    import ctypes, json, os, purifier_native
    print('NATIVE-STAGE python-library-load:start', flush=True)
    library = purifier_native.load('${library}')
    print('NATIVE-STAGE python-library-load:done', flush=True)
    call = getattr(library, '${symbol}')
    call.argtypes = []
    call.restype = ctypes.c_int
    print('NATIVE-STAGE python-downcall:start', flush=True)
    assert call() == 42
    print('NATIVE-STAGE python-downcall:done', flush=True)
    catalog = json.load(open('${projection.native.catalog}'))
    assert catalog['targets']['runtimeLifetime'] == 'process'
    assert catalog['system'] == '${pkgs.stdenv.hostPlatform.system}'
    class DlInfo(ctypes.Structure):
        _fields_ = [('filename', ctypes.c_char_p), ('base', ctypes.c_void_p),
                    ('symbol', ctypes.c_char_p), ('address', ctypes.c_void_p)]
    dladdr = ctypes.CDLL(None).dladdr
    dladdr.argtypes = [ctypes.c_void_p, ctypes.POINTER(DlInfo)]
    dladdr.restype = ctypes.c_int
    actual = DlInfo()
    assert dladdr(ctypes.cast(call, ctypes.c_void_p), ctypes.byref(actual)) != 0
    expected = os.path.realpath(catalog['libraries']['${library}'])
    assert os.path.realpath(actual.filename.decode()) == expected, actual.filename
    open('library-path', 'w').write(catalog['libraries']['${library}'])
    open('runtime-lifetime', 'w').write(catalog['targets']['runtimeLifetime'])
    print('NATIVE-STAGE python-origin-check:done', flush=True)
    PY
    cat > Foreign.java <<'JAVA'
    import java.lang.foreign.*;
    public class Foreign {
      private static void mark(String stage) {
        System.err.println("NATIVE-STAGE " + stage);
        System.err.flush();
      }
      public static void main(String[] args) throws Throwable {
        mark("java-main:start");
        if (args.length != 2 || !args[1].equals("process")) {
          throw new IllegalArgumentException("Go producer requires process runtime lifetime");
        }
        // Go c-shared includes a runtime: retain its library for process lifetime.
        var arena = Arena.global();
        mark("java-arena-create:done");
        mark("java-library-load:start");
        var lookup = SymbolLookup.libraryLookup(args[0], arena);
        mark("java-library-load:done");
        mark("java-symbol-find:start");
        var symbol = lookup.find("${symbol}").orElseThrow();
        mark("java-symbol-find:done");
        mark("java-downcall-handle:start");
        var call = Linker.nativeLinker().downcallHandle(symbol, FunctionDescriptor.of(ValueLayout.JAVA_INT));
        mark("java-downcall-handle:done");
        mark("java-downcall:start");
        if ((int) call.invokeExact() != 42) throw new AssertionError();
        mark("java-downcall:done");
        mark("java-process-arena:retained");
        mark("java-main:return");
      }
    }
    JAVA
    boundedStage java-compile javac --enable-preview --release ${pkgs.lib.versions.major pkgs.jdk.version} Foreign.java
    if boundedStage java-reject-wrong-lifetime java --enable-preview --enable-native-access=ALL-UNNAMED \
      Foreign "$(cat library-path)" confined > rejected-lifetime.log 2>&1; then
      cat rejected-lifetime.log
      exit 1
    fi
    grep -F 'Go producer requires process runtime lifetime' rejected-lifetime.log
    ! grep -F 'NATIVE-STAGE java-library-load:start' rejected-lifetime.log
    boundedStage java-execute java --enable-preview --enable-native-access=ALL-UNNAMED Foreign "$(cat library-path)" "$(cat runtime-lifetime)"
    printf 'NATIVE-STAGE consumer:complete\n'
    touch $out
  ''
