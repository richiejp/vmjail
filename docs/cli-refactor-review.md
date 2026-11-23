# Local CLI duplication and readability review

Three Sol reviews covered the files introduced or extended by the nsjail-style
CLI experiment, comparing them against the existing namespace, attach/job,
image-bundle, sandbox, and CLI implementations. The review identified shared
mechanisms to consolidate and distinct policies that must remain explicit.
The refactor and verification are complete.

## Entry point and namespaces

| Files | Finding and action |
| --- | --- |
| `cmd/vmjail/main.go` | Removed the external `unshare` invocation. Reuse the command parser for execution dispatch instead of a separate inspection parse. Re-exec the current binary with native pre-exec namespace attributes supplied by `internal/userns`. Preserve workload status and allow cancellation cleanup. |
| `cmd/vmjail/main_test.go` | Replace fake `unshare` scripts with namespace-attribute, parsing, exit-status and cancellation checks. Group setup, action and assertions. |
| `cmd/vmjail/integration_test.go` | Retain actual host-command, mount, EOF, deadline, listener, rerun, terminal and network scenarios. Add an empty-PATH namespace test checking distinct namespaces, caller-to-root maps, and private propagation. Replace the external `script` test helper with a native PTY. |
| `internal/cgroupwrap/cgroupwrap_linux.go`, `cgroupwrap_other.go` | The fresh-scope entry point reuses existing delegation machinery; keep its EBUSY recovery purpose. Review launcher-neutral diagnostics rather than introducing another delegation implementation. |

The existing subordinate-ID `userns.Reexec` and the new caller-to-root mapping
serve different purposes. The former populates subordinate ranges through its
existing synchronization flow. The CLI needs mount privileges immediately on
exec. Go's `SysProcAttr.Unshareflags`, UID/GID mappings, and disabled setgroups
install that mapping before exec; Go also makes mount propagation private for
`CLONE_NEWNS`. Merely calling the older helper would not supply those semantics.

## Streaming and guest command execution

| Files | Finding and action |
| --- | --- |
| `internal/cli/command_linux.go`, `command_linux_test.go` | Move argument-independent session handling to `internal/cli`; reuse the job output decoder rather than another stdout/stderr/exit switch. Keep local terminal and input cancellation responsibilities explicit. Test EOF, errors, early command completion and short writes. |
| `internal/agent/host_command_linux.go`, `host_command_linux_test.go` | Share framed output writers, PTY startup and process exit decoding with existing job/attach execution. Reuse the initial-container command builder. Preserve independent stdin EOF and output lifetime. |
| `internal/agent/host_command_other.go` | Keep the platform boundary for unsupported initial-command execution; it is not a second implementation. |
| `internal/agent/agent.go` | The host-command dispatch selects initial container execution and its completion path. It uses existing configuration, share, listener and gadget setup. |
| `internal/agent/share_linux.go`, `share_linux_test.go` | Keep guest mount-option translation. The guest mounting virtiofs and the host constructing a filtered source view are different operations. |
| `proto/agent/v1/config.proto`, generated `internal/genpb/agent/v1/config.pb.go` | Retain `host_command` as a lifecycle selector; it is not an alternative configuration transport. Generated code follows its schema. |

The job path has separate stdout/stderr and exit frames; its existing host
reader and guest writers are shared with initial commands. Attach sessions
retain their interactive frame format and resize semantics. Their EOF behavior
is not interchangeable: a job connection close cancels the job, closing an
attach input ends its session, and initial-command stdin EOF must leave output
open. Shared encoding and process helpers remove duplication without silently
changing those lifecycle contracts.

PTY allocation now belongs to `internal/pty` and is shared by guest execution,
CLI terminal tests and the real-VM TTY test. Local-session tests verify the
initial terminal dimensions, raw mode during execution, and restoration after
both successful completion and a guest error.

## CLI planning, mounts and OCI configuration

| Files | Finding and action |
| --- | --- |
| `internal/nsjail/options.go` | Share version handling with the existing CLI, organize flag groups, and remove test-only constants from production. Keep nsjail argument semantics and validation separate from sandboxd's image commands. |
| `internal/nsjail/options_test.go` | Keep behavior-focused argument, environment and OCI assertions, with common test fixtures in tests. |
| `internal/nsjail/spec.go` | Replace nested untyped maps and duplicate OCI wire shapes with the shared `internal/ocibundle` types extracted from imagestore. Retain distinct root, mount, capability and namespace policies. |
| `internal/nsjail/run_linux.go` (orchestration), `run_linux_test.go` | Share embedded-artifact defaults with sandboxd, isolate bind staging, remove redundant context setup, and preserve the underlying VM startup failure. Separate preparation, execution and cleanup blocks. |
| `internal/nsjail/view_linux.go`, `view_linux_test.go` | No existing implementation provides its recursive mount cloning, post-clone inspection and pseudo-filesystem masking semantics. Retain these protections; separate parsing, selection, masking and final mount attributes for readability. |
| `internal/nsjail/observe_linux.go` | Retain the per-run host/guest observation layout and required setup errors. Existing sandboxd observation setup has different fallback behavior, so merging them would obscure policy. Separate directory preparation from configuration assignment. |

The shared OCI package contains wire types, not a universal policy engine.
Image-backed and host-root containers use those same shapes with explicit
caller-owned policies. Rerun input must support cancellation and joining, so
an arbitrary blocking `io.Reader` cannot accumulate abandoned readers across
iterations. One-shot callers supplying a custom reader remain responsible for
making their blocked read return; production file/socket inputs have explicit
cancellation handling.

## Sandbox integration and packaging

| Files | Finding and action |
| --- | --- |
| `internal/sandbox/sandbox.go`, `vsock.go` | Keep the host-command config field and propagation through the existing guest-config path. Keep overlay-specific virtiofs settings; no second VM runner or config server is needed. |
| `internal/sandbox/vsocknet_test.go` | Retain the bounded wait for asynchronously updated counters; it addresses a test timing race rather than duplicating production functionality. |
| `internal/sandbox/config.go`, `internal/sandboxboot/sandboxboot.go` | Share bundled-artifact defaults across all three launchers while preserving explicit paths. Keep the bundled real-VM test in an external test package to avoid an import cycle. |
| `Makefile` | Consolidate identical Go binary build recipes while preserving public targets and real-VM verification. |
| `flake.nix` | The existing derivation already builds both host entry points. Keep package aliases and embedded assets; remove the obsolete external-unshare requirement description. Kernel/PVH work is independent of this refactor. |
| `scripts/smoke-release.sh` | Consolidate repeated binary/version/help checks into one loop. |

New common behavior is tested at its owning layer: namespace mapping in
`internal/userns`, OCI serialization through both existing builders, artifact
default precedence in sandbox/launcher tests, shared version handling in
`internal/cli`, and framed stream behavior in agent/job/local-session tests.

## Verification

- `make verify` passes: formatting, lint, unit/coverage gates, real KVM tests,
  benchmark gates and all five fuzz targets.
- Total coverage is 94.79%, up from the prior 94.60% measurement; the committed
  94.43% baseline is unchanged and every gated file meets 80%.
- The empty-PATH namespace test verifies native mappings and private propagation.
  Real-VM tests cover the existing command, mount, terminal and network behavior.
- Both Nix outputs build and the portable release archive passes its smoke test.
  The packaged CLI boots using embedded assets and preserves stdin, stdout,
  stderr and workload exit status 7.

The review found a test-only import cycle after centralizing artifact defaults:
the bundled integration test imported sandbox from within the bundled package.
It now uses an external test package and a test-only loader export, without
expanding the production asset API.

The compatibility package is now named `internal/nsjail`. Shared CLI command
sessions and workload exit status live in `internal/cli`; mount policy and
option-to-VM translation remain with the compatibility launcher.
