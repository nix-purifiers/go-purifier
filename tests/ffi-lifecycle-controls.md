# Isolated FFM lifecycle controls

This is an explicit diagnostic target, not an added default check and not a
lifetime fix. `tests/native-consumer.nix` stays unchanged, including the
confined arena, origin/result assertions and failing macOS process-exit case.

The new target compares eight fresh JVMs with the same host SDKs:

| Library | Operation | Lifetime |
|---|---|---|
| Ordinary tiny C / existing Go C-shared fixture | Load only / call and assert 42 | Confined / process |

Only the diagnostic process-lifetime comparison uses `Arena.global()`. It
never replaces the confined case. No `System.exit`, `halt`, production ABI
metadata or SDK selection changes are made.

Every case prints flushed load/symbol/handle/call/close/main-return markers.
The parent records actual exit status and elapsed time. If the owned JVM is
still alive after eight seconds, it runs that SDK's `jcmd PID Thread.print -l`
with an eight-second attach limit. If attachment fails while that owned JVM
remains alive, it sends that child QUIT and retains its output. Such a signal
may itself change process behavior; its outcome is diagnostic, never a clean
pass. The total case deadline is 45 seconds;
TERM and a three-second grace precede KILL of that owned process group.
Attachment processes are likewise bounded and reaped. A diagnostic
intervention, timeout, signal exit or nonzero result is not a clean pass.

The runner records all cases even after a failure, prints a structured
`LIFECYCLE-REPORT` to the build log, and returns nonzero if any case is not a
clean exit. It also writes report/log files under its output, but a failed
Nix build may discard those files: the structured build log is the durable
failure evidence. Attach failure is recorded and does not prove a JVM state.

## Explicit invocation

From the reviewed Go checkout:

```sh
export GO_FFI_SOURCE="$PWD"
nix build --impure --no-link --print-build-logs --expr 'let
  f = builtins.getFlake ("path:" + builtins.getEnv "GO_FFI_SOURCE");
  p = import f.inputs.nixpkgs {
    system = builtins.currentSystem;
    overlays = [ f.overlays.default ];
  };
in import (/. + builtins.getEnv "GO_FFI_SOURCE" + "/tests/ffi-lifecycle-controls.nix") {
  pkgs = p;
  build = p.go-purifier.buildGoProject;
}'
```

Keep compiler/runtime versions pinned by the same Go source lock. The report
records the selected Go/JDK versions and actual `java -version` output.
No SDK release matrix is expanded by this control.

A separate `.controllerCheck` passthru target evaluates failure handling with
an owned sleeping JVM and an owned TERM-ignoring process. It checks the JVM's
reported PID against the parent's child PID, verifies nonzero timeout outcomes
and reaping, and records whether a real JVM thread dump was available. Run the
same Nix expression with `.controllerCheck` on the imported result. This is a
controller check, not a supported SDK tuple.

## Proposed native CI plan

The feature owner can add one temporary, read-only diagnostic job on
`ubuntu-latest` and `macos-latest`, after reviewing this patch. Run the explicit
target and its controller check separately from the default flake checks so the original confined-case
failure cannot cancel the comparison before all cases finish. Allow a bounded
15-minute job for SDK substitution/build and the sequential process controls.

Capture the derivation path before building. Preserve both build output and
`nix log <derivation>` with `if: always()` artifact collection. Keep the build's
nonzero status; do not use `continue-on-error` to turn the diagnostic into a
passing gate. No job cancellation/retry or feature-head publication is part of
this proposal.

Compare ordinary C against Go, load against call, and confined against process
lifetime. A process-lifetime success alone is not a production fix or evidence
that every C ABI library needs that lifetime. Interpretation requires the exact
SDK/platform results and the captured JVM/thread state. The original confined
case remains a separate failed requirement until the intended producer and
consumer lifetime contract is established.
