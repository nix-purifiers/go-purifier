# Isolated FFM lifecycle controls

This is an explicit lifecycle control target, separate from default flake
checks. The opt-in unsupported comparison preserves the original confined Go
case and its failure behavior; it does not replace producer contract validation
in ordinary consumers.

The mandatory target compares six fresh JVMs with the same host SDKs: four C
controls (load/call, confined/process) and two process-lifetime Go controls
(load/call). Go C-shared runtime libraries require process lifetime; explicit
unload is unsupported. The absence of such metadata on another producer does
not guarantee unload safety.

An opt-in `.unsupportedComparison` passthru retains all eight original cases,
including both unsupported confined Go cases:

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
reads the selected Go version and library path from the actual producer native
catalog, requires its `runtimeLifetime` to be `process`, checks that catalog
against the loaded library, and records the pinned
JDK version and actual `java -version` output.
No SDK release matrix is expanded by this control.

Use `.unsupportedComparison` on the imported Nix result to reproduce all eight
cases. It passes `--include-unsupported-confined-go` to the controller. The
unsupported comparison retains nonzero outcomes and is excluded from the
mandatory job; it is not an expected-failure success gate.

A separate `.controllerCheck` passthru target evaluates failure handling with
an owned sleeping JVM and an owned TERM-ignoring process. It checks the JVM's
reported PID against the parent's child PID, verifies nonzero timeout outcomes
and reaping, and records whether a real JVM thread dump was available. Run the
same Nix expression with `.controllerCheck` on the imported result. This is a
controller check, not a supported SDK tuple. It also verifies that missing or
confined producer lifetime metadata is rejected before launching any JVM.

## Dedicated native CI job

The workflow adds an isolated diagnostic job on `ubuntu-latest` and
`macos-latest`, with `fail-fast: false` and a 15-minute timeout. Existing default
flake check wiring remains unchanged by this control split. The job runs the
explicit target and its controller check; the controller check still runs when
the lifecycle cases fail, and either failure keeps the job nonzero.

The job records source and derivation identities, both build outputs and
`nix log` output. Artifact collection uses `if: always()` so the structured
`LIFECYCLE-REPORT` in the log survives a failing case. No `continue-on-error`,
job cancellation/retry or feature-head publication is part of this proposal.

Compare ordinary C against Go, load against call, and confined against process
lifetime. A process-lifetime success alone is not a production fix or evidence
that every C ABI library needs that lifetime. Interpretation requires the exact
SDK/platform results and the captured JVM/thread state. The confined Go comparison remains a failing
diagnostic of an unsupported lifetime. Mandatory Go consumers must follow the producer process-lifetime
contract; no shutdown mechanism is inferred from this comparison.
