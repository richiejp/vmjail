# Observability

> Two-tier observability for sandbox VMs. Always on, designed to make
> "what was the VM doing when this test failed?" answerable from the
> failing test's log without running anything inside the guest at the
> moment of failure.

## TL;DR for agents

1. Every sandbox VM that gets launched with `sandbox.Config.ObserverLogPath`
   set has an in-process observer that samples its cloud-hypervisor
   PID at 1 Hz into a JSON-lines file at that path. Tests and CLI
   surfaces should set the path so the data is collected; only set
   it to empty if you have a specific reason.
2. The log survives the VM exiting. To debug a failing integration
   test, look at `<test-tempdir>/.../observer-<sandbox>.jsonl` for
   the host-side resource trace; in-guest profiler logs sitting
   alongside cover only what the guest's userspace can see.
3. Cgroup-level metrics and an in-guest eBPF library are deliberate
   follow-up work — see [Roadmap](#roadmap). Today's Tier 1 covers
   the idle-vs-busy and "is the cloud-hypervisor process making
   forward progress" questions.

## Why this exists

A 4-minute integration test that fails with "sentinel never landed"
is useless if there's no record of what the VM was doing during
those 4 minutes. Was it CPU-bound? Blocked on virtio? Idle because
the workload had already crashed? Were the host's resources saturated?
Without telemetry, every failure becomes a follow-up interactive
investigation — which contradicts the project's "fail fast, fail
loud" ethos (CONTRIBUTING.md "Slowness is a bug").

The observer earns its keep when:

- A test stalls and we want to confirm the guest is idle (low CPU,
  no I/O bytes accumulating) before chasing kernel-level causes.
- A test passes faster than expected and we want to confirm the
  workload actually ran (CPU jiffies climbed, I/O bytes flowed)
  rather than short-circuited.
- A regression makes one specific phase slower; we have a per-
  second time series of every resource counter to localise it.

## Tier 1 — host-side `/proc` sampler + per-VM cgroup wrap

**What:** `internal/observability.Observer` runs as a goroutine
inside the sandboxd process. For each tick (default 1 Hz) it reads
`/proc/<cloud-hypervisor-pid>/{stat,status,io}` and, when a per-VM
cgroup wrap is in place, the cgroup v2 files
`cpu.stat` / `memory.{current,peak,max,swap.current}` / `io.stat` /
`pids.current` from the workload's own cgroup. One JSON line per
tick is appended to a configured log path. The total on-disk
footprint is bounded by rotation: when the live file reaches
`MaxBytes / 2` it's renamed to `<path>.1`, a new file is opened,
and one backup is kept. Default `MaxBytes` is 50 MB; total disk
per sandbox is therefore ~50 MB worst case (~25 MB live + ~25 MB
rotated), `~30 KB / min` at 1 Hz, `~43 MB / day` before rotation.

**Where it runs:** `internal/sandbox/sandbox.go` (`cloudHypervisorRunner.run`).
Before `cmd.Start()` we try to create a per-VM cgroup v2
sub-directory under our own cgroup via `observability.CreateSubCgroup`,
then pass an O_PATH fd to `SysProcAttr.UseCgroupFD` so the
cloud-hypervisor child is placed in the cgroup atomically at fork
via `CLONE_INTO_CGROUP`. The observer is started with the CH PID
*and* the cgroup directory, so each sample carries both views.
The cleanup `stop` closure calls `obs.Stop()` before `cmd.Wait()`
so the final samples flush before the process is reaped, then
`removeVMCgroupBestEffort` rmdir's the cgroup after the kernel
has reaped the task. A failed observer or cgroup setup is
reported via `oops.Log` and the sandbox proceeds without the
affected surface — observability is a debugging surface, not a
correctness invariant.

**Cgroup wrap requires delegation.** On stock systemd hosts the
user session scope doesn't have `Delegate=yes`, so mkdir under
`/sys/fs/cgroup/user.slice/user-<uid>.slice/session-<n>.scope`
returns EACCES. When that happens, `setupVMCgroup` logs and
returns `("", -1)`; the observer falls back to `/proc`-only
sampling. To get the cgroup view on a stock host, launch
`daft-sandboxd` under a delegated transient unit:

```sh
systemd-run --user --scope --property=Delegate=yes daft-sandboxd ...
```

CI / production deployments running under a delegated systemd
service unit (or as root) get the cgroup wrap automatically.

**What's in each sample:**

```json
{
  "ts": "2026-05-30T07:01:32.123456789Z",
  "pid": 12345,
  "proc": {
    "comm": "cloud-hyperviso",
    "state": "S",
    "utime_jiffies": 4321,
    "stime_jiffies": 2107,
    "num_threads": 8,
    "vm_rss_kb": 268432,
    "vm_size_kb": 1048576,
    "rchar": 73925155,
    "wchar": 1443918,
    "syscr": 18047,
    "syscw": 21146,
    "read_bytes": 0,
    "write_bytes": 0
  },
  "cgroup": {
    "path": "/sys/fs/cgroup/.../daft-vm-<hash>-<seq>",
    "cpu": {
      "usage_usec": 5234567,
      "user_usec": 3800000,
      "system_usec": 1434567
    },
    "memory": {
      "current_bytes": 268435456,
      "peak_bytes": 312345600,
      "max_bytes": -1
    },
    "io": [
      {"device": "8:0", "rbytes": 1048576, "wbytes": 524288, "rios": 100, "wios": 50}
    ],
    "pids": {"current": 8}
  }
}
```

The `cgroup` block is omitted when the cgroup wrap couldn't
be set up (no delegation); `proc` is omitted when the target
exited between samples. `memory.max_bytes: -1` is the JSON
encoding of the kernel's `"max"` sentinel — no limit.

**Reading them in post-mortem:** counters are cumulative; deltas
between two samples give per-second rates. `jq` or `awk` is enough:

```sh
# CPU jiffies/sec — distinguishes "guest is idle" from "guest is
# pegging a vcpu". A 4-CPU guest at full tilt → ~400 user+system
# jiffies/sec on Linux defaults (HZ=100).
jq -r '.proc | "\(.utime_jiffies+.stime_jiffies)"' obs.jsonl | \
  awk 'NR>1 {print $1-prev} {prev=$1}'

# Bytes read by the VMM per sample — pulls during virtio device
# emulation, virtiofs, etc.
jq -r '.proc.rchar' obs.jsonl | \
  awk 'NR>1 {print $1-prev} {prev=$1}'
```

When the target dies between samples (e.g. CH crashed mid-run), the
sample comes through with `proc` absent and `err: "pid gone"` set,
so a flatline at the end of a log is unambiguous.

**Where the log lives:** the *caller* sets the absolute path on
`Config.ObserverLogPath`. There is no implicit default — each
caller picks a location appropriate for its lifetime:

- **Integration tests:** `t.TempDir() + "/observer-<id>.jsonl"`.
  Auto-cleaned with the test; failure reports cite the path so the
  log is grabbable before the temp dir is removed.
- **`daft-sandboxd` user-facing launches:** placed under a
  per-sandbox state dir, surfaced via a future
  `daft sandbox observer-tail <name>` subcommand. Path lookup is
  recorded on the sandbox object so an operator can `cat` it
  directly.
- **`daft sandbox hello`:** uses a tempdir under the user's cache
  dir; surfaces the path on stdout at the end of the run.

**What it does NOT do (yet):**

- No virtiofsd in the cgroup. Today the cgroup wraps only
  cloud-hypervisor — `memory.peak` / `io.stat` therefore miss the
  virtiofsd sidecar's contributions to host I/O. Follow-up:
  thread the cgroup fd through `virtiofsdSidecar.start` so the
  whole VM tree is one accounting unit.
- No network metrics. The guest network goes through
  `gvisor-tap-vsock`, a userspace TCP/IP stack in sandboxd's
  address space — there is no host tap, so `/proc/net/dev` doesn't
  see it. Follow-up: hook a byte/packet counter into the
  gvisor-tap-vsock switch.

## Tier 2 — guest-side eBPF library (follow-up)

`bpftrace` is not a goal. We want a curated library of focused Go
programs, each using `cilium/ebpf` + `bpf2go` to embed a small BPF
C program targeting one diagnostic question. The shape we want is
[dkorunic/pktstat-bpf](https://github.com/dkorunic/pktstat-bpf):
one binary, single static ELF with the bytecode inside, structured
output, no shell dependencies.

eBPF runs **inside the guest**, not on the host. We control the
guest kernel — any kconfig needed (BTF, kprobes, uprobes, perf
events) is just a flake change. The guest's init userns has all the
capabilities (CAP_BPF, CAP_PERFMON) eBPF programs need. The host
sees the VM as one cloud-hypervisor process; host-side eBPF (KVM
tracepoints) would only see vcpu transitions, not what the guest is
actually doing.

### Likely first gadgets

| Gadget          | Question                                            | Probe         |
| --------------- | --------------------------------------------------- | ------------- |
| `open-snoop`    | "what files is the workload trying to open?"        | tracepoint:syscalls:sys_enter_openat |
| `biolatency`    | "where is virtio-blk latency coming from?"          | tracepoint:block:block_rq_issue/complete |
| `tcpstat`       | "what TCP connections is the guest making?"         | kprobe:tcp_connect, tcp_sendmsg, tcp_recvmsg |
| `runqlat`       | "is the scheduler delaying anything?"               | tracepoint:sched:sched_wakeup, sched_switch |
| `pktstat`       | "which proc/cgroup is the network traffic?"         | TC ingress/egress |

The first one to land is most likely **`open-snoop`** — the lint
cache-miss bug that motivated this work was an `openat()` returning
ENOENT on a path the test cared about; this gadget would have shown
the failure as one line in a guest-side log.

### Kernel-side prerequisites for Tier 2

Currently set in the microVM kernel
(`third_party/kernel-config/daft.config`):

- `CONFIG_BPF=y`
- `CONFIG_BPF_SYSCALL=y`
- `CONFIG_BPF_JIT=y`
- `CONFIG_PERF_EVENTS=y`
- `CONFIG_FTRACE=y`
- `CONFIG_KPROBES=y`
- `CONFIG_BPF_LSM=y`

Still needed before CO-RE eBPF can land:

- `CONFIG_DEBUG_INFO_BTF=y` — required for CO-RE; adds a few MB
  to the kernel image.
- `CONFIG_UPROBES=y` — userspace probes (for virtiofsd, guest
  binaries).
- `CONFIG_KALLSYMS_ALL=y` — symbol resolution for kprobes.
- `CONFIG_FTRACE_SYSCALLS=y` — `sys_enter/sys_exit` tracepoints.
- `CONFIG_BPF_EVENTS=y` — BPF on tracepoints.

These get added in the same commit that adds the `cilium/ebpf`
dependency and the first gadget, gated by the existing
`kconfig-verify` script.

### Library choice

`cilium/ebpf`. Pure-Go (no cgo), ships `cmd/bpf2go` for embedding
compiled BPF objects via `//go:generate`, mature CO-RE support.
`aquasecurity/libbpfgo` was considered but rejected: requires cgo
+ vendored libbpf at build time, no upside for this project's
scale.

### Library layout (planned)

```
internal/observability/
  observer.go              ← Tier 1 (this commit)
  sample_linux.go          ← Tier 1
  ebpf/                    ← Tier 2 (follow-up)
    open_snoop/
      open_snoop.go        ← Go wrapper, ringbuf reader, CLI
      open_snoop_bpf.c     ← BPF C source
      open_snoop_bpfel.go  ← bpf2go-generated, committed
      open_snoop_bpfel.o   ← compiled object, committed
    biolatency/
      ...
```

Each gadget is one subpackage, owning its own ringbuf reader and
output format, with a documented `Why this was written` block at
the top tying it to a specific recurring symptom.

## Roadmap

In rough order:

1. **Tier 1** (landed) — `/proc` sampler + per-VM cgroup wrap in
   `internal/observability`, wired into
   `sandbox.Config.ObserverLogPath`.
2. **gvisor-tap-vsock byte/packet counter** — instrument the switch
   in `internal/sandboxnet` so the observer can record per-VM
   network bytes/packets.
4. **`daft-sandboxd` per-sandbox state dir** — pick a stable path
   (likely `$XDG_STATE_HOME/daft/sandboxes/<id>/`) and default
   `ObserverLogPath` there for user-facing launches.
5. **`daft sandbox observer-tail <name>` CLI surface** — convenience
   for `tail -f` against a running sandbox's observer log without
   needing to know the path.
6. **`testdaemons.Cluster` integration** — every integration test
   workspace gets an observer log path under `t.TempDir()`, surfaced
   in failure messages so the file is grabbable before tempdir
   cleanup.
7. **Tier 2 kickoff** — kconfig commit (BTF/UPROBES/KALLSYMS_ALL/
   FTRACE_SYSCALLS/BPF_EVENTS) + `cilium/ebpf` dependency + first
   gadget (`open-snoop`).
8. **More gadgets** — `biolatency`, `tcpstat`, `runqlat`, `pktstat`,
   each motivated by a real recurring symptom.

## Related

- `internal/observability/` — the Tier 1 package.
- `internal/sandbox/sandbox.go` — wiring (`cloudHypervisorRunner.run`).
- `internal/testdaemons/workspace_profiler_test.go` — the in-guest
  `attachWorkspaceProfiler` is **transitional** and will be retired
  once Tier 1 is wired through `testdaemons.Cluster` and Tier 2
  gadgets cover the cases the in-guest sampler currently does.
- `CONTRIBUTING.md` "Slowness is a bug" — the policy this
  infrastructure exists to support.
