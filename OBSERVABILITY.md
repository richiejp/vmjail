# Sandbox observability

Retain both host resource accounting and guest workload instrumentation as the
project becomes a local sandbox. Neither depends on workspace CAS. Diagnostic
files should remain available on the host after the VM exits.

The old remote observation commands are not currently a working interface:
their launch/catalog dependencies are being removed. The local launcher still
needs to extract guest observation-share setup from `sandboxexec` and connect
the existing CLI renderer to local files. See [PLAN.md](PLAN.md).

## Host resource accounting

`internal/observability` samples the Cloud Hypervisor PID, the per-VM cgroup and
optional sandbox network counters. The observer records CPU, memory, IO and
network samples into rotating JSONL. Set `sandbox.Config.ObserverLogPath` to
choose the file. `internal/sandbox.startObserverIfConfigured` starts sampling
when the VMM starts.

The per-VM cgroup is created before the shares and VMM. Both Cloud Hypervisor
and virtiofsd sidecars are placed in it at process creation using the cgroup FD.
Thus the cgroup view includes sharing-process resources, while the `/proc` view
is specific to the VMM PID. These measurements are complementary.

Keep the lifecycle ordering: close listeners, reap child processes, then remove
the empty cgroup. `vmTeardown`, process-group termination, parent-death signals
and the reap watchdog make failures diagnosable without letting one blocked
child prevent cleanup of another.

Logs rotate into a live file and one backup, bounded by the observer's configured
byte limit. `internal/observertail` supports reading/following the retained logs.
The local CLI should consume these files without a remote transport.

## Guest workload instrumentation

The guest helper in `internal/agent` starts the configured gadgets before the
container workload. It waits for attachment readiness so early workload events
are not missed merely because a gadget is still starting.

| Gadget | Implementation | Evidence |
| --- | --- | --- |
| opensnoop | `internal/observability/ebpf/opensnoop` | Opened paths, calls and errors. |
| iosnoop | `internal/observability/ebpf/iosnoop` | Aggregated per-process/FD IO, with file/socket/pipe targets and byte counts. |
| execsnoop | `internal/observability/ebpf/execsnoop` | Executed programs and related process activity. |
| sleepsnoop | `internal/observability/ebpf/sleepsnoop` | Sleep activity that helps explain idle workloads. |

`internal/observability/progress` combines these into a bounded summary. Guest
`observe_summary.go` writes the summary; the retained CLI rendering code in
`internal/cli/observe.go` presents the evidence.

Keep `sandboxexec.applyObserveSummary`'s useful setup when extracting the local
launcher: create a private host observation directory, export it as a virtiofs
share, and set the guest gadget log paths and `ObserveSummaryPath` inside that
share. `ObserverLogPath` is a host path; the guest log and summary paths refer
to the guest-mounted observation share. This separation lets guest output
persist locally after teardown without copying files through CAS.

## What storage IO evidence means

Host `/proc` and cgroup counters describe resource consumption, not which exact
file operation happened. Guest iosnoop tracks instrumented IO syscalls and
aggregates per PID/FD; it resolves targets through guest `/proc`. Short-lived
file descriptors, sampling and uninstrumented access paths limit attribution.
Guest opensnoop supplies additional path-level context.

These are diagnostic summaries, not a complete ordered read/write audit and
not access-control enforcement. Direct virtiofs host mounts do not preserve the
deleted CAS FUSE layer's chunk-read policy hook. A complete host-directory audit
or read-interception policy would need additional instrumentation.

## Kernel and build requirements

Keep the guest kernel's BPF, BTF and tracepoint configuration, the gadget C
sources, generated Go loaders and committed `*_bpfel.o` objects. Normal builds
consume the committed objects. `make gen-bpf` regenerates them with the BPF
compiler toolchain; runtime gadget loading happens inside the guest.

Guest operation-specific trace profiles in `internal/agent/kernel_trace_linux.go`
remain available for retained filesystem operations such as XFS growth. They
are separate from direct host mounts, which do not require an XFS workspace
disk. Preserve bounded trace buffers and named event allowlists if that optional
functionality is kept.

## Tests

Retain host observer/cgroup tests, all eBPF aggregator/progress tests, guest
gadget-wiring tests, VM teardown tests and direct-share VM tests. Real VM tests
need `/dev/kvm`, built guest artifacts and suitable namespace/cgroup privileges.
Passing host-side unit tests alone does not verify guest gadget loading.
