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

## Tier 1 — host-side `/proc` sampler + per-VM cgroup wrap + sandboxnet byte counters

**What:** `internal/observability.Observer` runs as a goroutine
inside the sandboxd process. For each tick (default 1 Hz) it reads
`/proc/<cloud-hypervisor-pid>/{stat,status,io}`, the per-VM
sandboxnet network's rx/tx byte counters (when sandboxnet is wired
in), and — when a per-VM cgroup wrap is in place — the cgroup v2
files `cpu.stat` / `memory.{current,peak,max,swap.current}` /
`io.stat` / `pids.current` from the workload's own cgroup. One JSON
line per tick is appended to a configured log path. The total on-disk
footprint is bounded by rotation: when the live file reaches
`MaxBytes / 2` it's renamed to `<path>.1`, a new file is opened,
and one backup is kept. Default `MaxBytes` is 50 MB; total disk
per sandbox is therefore ~50 MB worst case (~25 MB live + ~25 MB
rotated), `~30 KB / min` at 1 Hz, `~43 MB / day` before rotation.

**Where it runs:** `internal/sandbox/sandbox.go` (orchestration in
`helloWith` / `serveWith`). `createVMCgroup` runs FIRST, before
the share sidecars and the VMM, so the cgroup directory exists
when virtiofsd is spawned. `placeInCgroup` then opens one O_PATH
fd per spawn — each `cmd.Start` does its own `CLONE_INTO_CGROUP`
via `SysProcAttr.UseCgroupFD`, so every virtiofsd sidecar AND the
cloud-hypervisor process all land in the same per-VM cgroup
atomically at fork. The observer is started with the CH PID *and*
the cgroup directory, so each sample carries both views.

Teardown is orchestrated by a single `vmTeardown` (replacing the
old defer-LIFO chain) so a wedged child can't strand the cleanup
of a healthy one. It runs three phases: (1) the pure-Go receivers
(trace + net listeners) close first — fast, and closing their
tracked conns unblocks any per-conn goroutine still pumping to the
about-to-die VMM; (2) the process children (the VMM and every
virtiofsd) are reaped **concurrently**, so one wedged in
uninterruptible kernel sleep (D-state) doesn't block the reap of
its siblings; (3) `removeVMCgroupBestEffort` rmdir's the directory
last, only after every task is reaped — the kernel refuses rmdir
while any task is still inside.

Each spawn is hardened (`hardenSpawn`): `Setpgid` puts it in its
own process group so teardown can group-kill the whole tree (the
VMM plus any helper it forks) with one negative-PID `SIGKILL`;
`Pdeathsig` SIGKILLs it if sandboxd dies; `WaitDelay` bounds
`cmd.Wait` against an orphan that escaped the group and is holding
a stdio pipe open. None of that can kill a child wedged in
D-state — nothing in userspace can. That case is handled by
`reapWithWatchdog`: it **keeps waiting** (abandoning reclaims
nothing — the cgroup rmdir EBUSYs and the PID stays unreapable
either way) and emits an escalating loud `oops` warning naming the
process and PID, so a host-level resource leak is attributable to
daft rather than mysterious. If you see `sandbox: reap stalled`
logs, a VMM/virtiofsd is wedged in the kernel and its cgroup +
memory stay held until the kernel releases it.

A failed observer or cgroup setup is reported via `oops.Log` and
the sandbox proceeds without the affected surface — observability
is a debugging surface, not a correctness invariant.

**Cgroup wrap requires delegation.** On stock systemd hosts the
user session scope doesn't have `Delegate=yes`, so mkdir under
`/sys/fs/cgroup/user.slice/user-<uid>.slice/session-<n>.scope`
returns EACCES. When that happens, `createVMCgroup` logs and
returns `""`; the observer falls back to `/proc`-only sampling.
To get the cgroup view on a stock host, launch `daft-sandboxd`
under a delegated transient unit:

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
  },
  "net": {
    "rx_bytes": 4096,
    "tx_bytes": 8192
  }
}
```

The `cgroup` block is omitted when the cgroup wrap couldn't
be set up (no delegation); `proc` is omitted when the target
exited between samples; `net` is omitted for sandboxes without
a vsock-net device, AND for the pre-conn window of sandboxes
that have one — until the guest dials in and the netstack's
`Accept` runs, `net` stays absent so a flat `{0,0}` block can
only mean "wired and idle since first connect", never "wired
but never connected". `rx_bytes` counts bytes the guest sent to
sandboxd over the StdioProtocol vsock conn; `tx_bytes` counts
bytes sandboxd sent to the guest. Both include the 2-byte
StdioProtocol size prefix per frame. `memory.max_bytes: -1`
is the JSON encoding of the kernel's `"max"` sentinel — no
limit.

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

- **Integration tests:** `testdaemons.Cluster` points the sandboxd
  subprocess's state root (`DAFT_STATE_DIR`) at `Cluster.ObserverDir`
  under the test's `t.TempDir()`, so every dispatched VM writes to
  `<ObserverDir>/<uid>/observer.jsonl`. On test failure a Cleanup
  dumps the tail of each log into the test output — the temp dir is
  wiped on cleanup, so the trace is surfaced there rather than left
  to be grabbed from a path that's about to vanish.
- **`daft-sandboxd` user-facing launches:** placed under a
  per-sandbox state dir, surfaced via a future
  `daft sandbox observer-tail <name>` subcommand. Path lookup is
  recorded on the sandbox object so an operator can `cat` it
  directly.
- **`daft sandbox hello`:** uses a tempdir under the user's cache
  dir; surfaces the path on stdout at the end of the run.

**What it does NOT do (yet):**

- No per-protocol or per-flow network detail. The sandboxnet
  byte counters answer "is the guest moving bytes through the
  userspace stack at all" but not "to which destinations" or
  "how many TCP retransmits". Per-flow and per-syscall network
  detail is a Tier 2 gadget (`pktstat`, `tcpstat`).
- No packet counts. Wire-byte counts are enough for the
  idle-vs-busy question; adding frame counters means either
  parsing StdioProtocol headers in the wrapper or coordinating
  with gvisor-tap-vsock's switch. Deferred until a Tier 2 gadget
  asks for it.

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

**`open-snoop` landed first** (roadmap #5b) — the lint cache-miss bug
that motivated this work was an `openat()` returning ENOENT on a path
the test cared about; this gadget shows the failure as one line in a
guest-side log. It attaches to `tracepoint:syscalls:sys_enter_openat`
and pushes one ring-buffer record per open (pid, uid, comm, dirfd,
flags, path). The remaining gadgets follow as they're motivated by a
real symptom.

### Kernel-side prerequisites for Tier 2 (landed — roadmap #5a)

Set in the microVM kernel (`third_party/kernel-config/daft.config`)
and enforced by the `kconfig-verify` gate — a silent `olddefconfig`
demotion fails the kernel build rather than shipping a kernel CO-RE
can't load against.

Already present from the BPF-LSM work:

- `CONFIG_BPF=y`, `CONFIG_BPF_SYSCALL=y`, `CONFIG_BPF_JIT=y`
- `CONFIG_PERF_EVENTS=y`, `CONFIG_FTRACE=y`, `CONFIG_KPROBES=y`
- `CONFIG_BPF_LSM=y`

The CO-RE set added for Tier 2:

- `CONFIG_DEBUG_INFO_DWARF_TOOLCHAIN_DEFAULT=y` — flips the "Debug
  information" choice off its `DEBUG_INFO_NONE` default so the
  choice's `select DEBUG_INFO` fires (BTF needs debug info); lets
  clang pick its own DWARF version.
- `CONFIG_DEBUG_INFO_BTF=y` — the CO-RE prerequisite. `pahole`
  (already in the kernel derivation's `nativeBuildInputs`) renders
  the debug info into the BTF blob embedded in the image. Adds a
  few MB. `RANDSTRUCT_NONE=y` satisfies BTF's
  `!GCC_PLUGIN_RANDSTRUCT` dependency.
- `CONFIG_KALLSYMS_ALL=y` — full symbol table for kprobe attach.
- `CONFIG_FTRACE_SYSCALLS=y` — `sys_enter/sys_exit` tracepoints
  (open-snoop's `openat` probe).

`CONFIG_UPROBES=y` and `CONFIG_BPF_EVENTS=y` were already
`def_bool y` transitively (UPROBES from `ARCH_SUPPORTS_UPROBES`+MMU,
BPF_EVENTS from `KPROBES`+`FTRACE`); they're re-asserted in the
fragment so a future dependency change fails the gate loudly instead
of silently dropping CO-RE attach support.

The `cilium/ebpf` dependency + the first gadget landed in #5b; the
wiring into daft-agent + the in-VM e2e proof landed in #5c.

### Library choice

`cilium/ebpf`. Pure-Go (no cgo), ships `cmd/bpf2go` for embedding
compiled BPF objects via `//go:generate`, mature CO-RE support.
`aquasecurity/libbpfgo` was considered but rejected: requires cgo
+ vendored libbpf at build time, no upside for this project's
scale.

### Library layout

```
internal/observability/
  observer.go              ← Tier 1
  sample_linux.go          ← Tier 1
  ebpf/                    ← Tier 2
    opensnoop/             ← first gadget (roadmap #5b, landed)
      opensnoop.go         ← Event decode, openFlag render, Run drain
                             loop + the privileged load seam (linux)
      opensnoop_other.go   ← non-linux stub (Run → ErrUnsupported)
      opensnoop_bpf.c      ← BPF C source (sys_enter_openat → ringbuf)
      gen.go               ← package doc + //go:generate bpf2go
      opensnoop_bpfel.go   ← bpf2go-generated loader, committed
      opensnoop_bpfel.o    ← compiled BPF ELF, committed
      opensnoop_test.go    ← decode/render/drain-loop unit tests
    biolatency/
      ...
```

Each gadget is one subpackage, owning its own ring-buffer reader and
output format, with a documented "why this was written" block at the
top tying it to a specific recurring symptom.

**Run model: in-process in daft-agent.** A gadget is a library, not a
standalone binary. daft-agent imports it and calls `Run(ctx, w)` as a
goroutine in the guest's privileged init userns — symmetric with
Tier 1's in-sandboxd observer goroutine — writing its trace to a
guest-side log. (A thin `cmd/` main per gadget for standalone host
debugging is allowed but not required; the agent is the production
caller.) The wiring lives in `internal/agent/opensnoop.go`
(`startOpensnoopFromCmdline`, driven by the `daft.observe.opensnoop=`
cmdline token the host emits from `sandbox.Config.OpensnoopLogPath`);
landed in roadmap #5c.

**Testability under the coverage gate.** Loading BPF needs CAP_BPF on
a BPF-capable kernel, which `make check` doesn't have
(`unprivileged_bpf_disabled`). So each gadget keeps its logic — record
decode, output rendering, the ring-buffer drain loop — pure and
behind a small reader interface (`*ringbuf.Reader` satisfies it,
tests fake it), and isolates the privileged load/attach behind an
injected `bpfDeps` seam whose error paths the tests still exercise
(the pattern `internal/agent/tap_linux.go` uses for `realTunSetIff`).
The bpf2go-generated `*_bpfel.go` loader is excluded from the coverage
gate (`scripts/cover-check.sh` `SKIP_PATHS`), like `internal/genpb`;
the hand-written wiring is **not** excluded.

### Regenerating a gadget's BPF object

`make gen-bpf` (Nix dev shell only). For each gadget it dumps the host
kernel BTF to a transient `vmlinux.h`, runs bpf2go (clang → a
committed `*_bpfel.o` + `*_bpfel.go`), then removes `vmlinux.h`. The
committed objects mean a normal build / `make check` needs no clang,
bpftool, or BTF. `NIX_HARDENING_ENABLE` is cleared for the compile
because the Nix cc-wrapper otherwise injects `-fzero-call-used-regs` /
`-fstack-protector`, which clang rejects for the `bpf` target.

## Roadmap

In rough order:

1. **Tier 1** (landed) — `/proc` sampler + per-VM cgroup wrap
   (CH + virtiofsd) + sandboxnet byte counters in
   `internal/observability`, wired into
   `sandbox.Config.ObserverLogPath`.
2. **`daft-sandboxd` per-sandbox state dir** (landed) — each sandbox
   gets a `$XDG_STATE_HOME/daft/sandboxes/<id>/` dir (falling back to
   `~/.local/state/...`; override the root outright with
   `DAFT_STATE_DIR`), and `ObserverLogPath` defaults to
   `<id>/observer.jsonl` there for every user-facing launch. The
   orchestrator-dispatched path (`daft-sandboxd serve` →
   `sandboxexec.Executor`) keys `<id>` on the Sandbox uid, so the log
   is discoverable from the uid alone; the debug `hello`/`run` paths,
   which have no uid, mint a sortable `<UTC-timestamp>-<hex>` id and
   print the chosen path. The dir persists past VM exit for
   post-mortem inspection. Resolution helpers live in
   `internal/sandboxdcli/state.go`; the filename is
   `observability.DefaultLogFilename` (one source of truth shared with
   item 3).
3. **`daft sandbox observer-tail <workspace>` CLI surface** (landed) —
   streams a running sandbox's observer log to the user-host over the
   orchestrator, brokered exactly like `daft workspace exec` (attach):
   the client opens `WorkspaceCatalogService.ObserverTail`; the
   orchestrator resolves the workspace to its `current_sandbox`
   (node fingerprint + uid) and rendezvous-brokers a
   `NodeControlService.ObserverStream`; the node (`daft-sandboxd`)
   resolves the uid through a per-dispatch `sandboxgateway.ObserverRegistry`
   and tails the on-disk log (`internal/observertail`, which follows
   across the rename-based rotation) back to the client. `--follow`
   (default) stays open like `tail -f`; `--follow=false` dumps the
   current log and exits. Read-only and mTLS-authenticated end to end
   (same peer-fingerprint model as attach).
4. **`testdaemons.Cluster` integration** (landed) — `NewCluster`
   points the sandboxd subprocess's per-sandbox state root at a
   `t.TempDir()` subdir via `DAFT_STATE_DIR` (`Cluster.ObserverDir`),
   so every orchestrator-dispatched VM's Tier-1 trace lands at
   `<ObserverDir>/<uid>/observer.jsonl` — host-side observability was
   dark for testdaemons VMs before this. A failure-only Cleanup dumps
   the bounded tail of each log into the test output (the temp dir is
   wiped on cleanup, so the trace is surfaced inline, not left at a
   vanishing path). `TestClusterWorkspaceLaunchAgainstHelloBundle`
   asserts a real booted VM produces a sample. The in-guest
   `attachWorkspaceProfiler` stays for now: its retirement is gated on
   *both* this wiring **and** Tier 2 gadgets covering the guest-internal
   detail (PSI, per-process wchan, store-path counts) the host-side
   sampler can't see — so only the first half of that gate is met.
5. **Tier 2 kickoff** — sliced:
   - **5a (landed)** — kernel-side kconfig prerequisites in
     `daft.config` (`DEBUG_INFO_DWARF_TOOLCHAIN_DEFAULT` +
     `DEBUG_INFO_BTF`, `KALLSYMS_ALL`, `FTRACE_SYSCALLS`;
     `UPROBES`/`BPF_EVENTS` were already transitively on and are
     re-asserted), gated by the existing `kconfig-verify` script.
     See "Kernel-side prerequisites for Tier 2".
   - **5b (landed)** — `cilium/ebpf` dependency + `bpf2go` toolchain
     (dev-shell clang/llvm/bpftool/libbpf, `make gen-bpf`) + the first
     gadget `internal/observability/ebpf/opensnoop` itself: BPF C for
     `sys_enter_openat` → ring buffer, the committed generated loader,
     and the Go decode/render/drain-loop with the privileged load
     behind an injected seam (unit-tested to 96%). The generated
     `*_bpfel.go` is excluded from the coverage gate; daft-lint now
     skips generated files (`ast.IsGenerated`). Not yet wired into the
     agent — the package is built + tested but has no caller.
   - **5c (landed)** — `opensnoop.Run` is wired into daft-agent: when
     the host sets `sandbox.Config.OpensnoopLogPath`, the cmdline carries
     `daft.observe.opensnoop=<guest-path>` and the agent
     (`startOpensnoopFromCmdline`) mounts tracefs (the minimal u-root
     guest mounts neither tracefs nor debugfs, which cilium/ebpf needs to
     resolve the tracepoint id — `ensureTracefs`, symmetric with
     `ensureCgroupV2`), opens that file, and spawns the gadget as a
     lifetime goroutine in the guest's init userns, attaching the
     tracepoint *before* the container so it catches the workload's
     openat storm. The privileged load sits behind the same injected
     `opensnoopDeps` seam (zero value disables it, so `make check`'s
     CAP_BPF-less unit tests skip the BPF path). The in-VM e2e
     (`TestHelloIntegrationOpensnoopCapturesOpenat`) points the trace at
     a writable virtiofs share, boots a real container VM, and asserts a
     decoded openat record made it back to the host file.
6. **Live build-progress summary + CLI** — the reason the gadgets exist.
   When a long workload runs inside a sandbox (the canonical case: a
   `nix build` / `make verify` self-build), the always-on gadgets must
   roll their raw event streams up into a small, continuously-refreshed
   **summary** — counts, top-N paths, ENOENT/cache-miss tallies,
   per-process breakdown, per-window rates, and a per-gadget
   *last-activity age* — folded together with the Tier-1 host sampler
   into one "what is this build doing right now" view. A user or agent
   prints it from the CLI on demand and re-polls to answer "is this
   build making forward progress, stuck, or just pathologically slow,
   and on what?". The raw `opensnoop.log` (roadmap #5c) becomes the
   *drill-down* layer beneath the summary, not the primary surface.

   Shape (confirmed): each gadget keeps an in-memory rolling aggregate;
   the agent serializes all enabled gadgets' aggregates to a small JSON
   summary file on a per-sandbox *observe* virtiofs share (mapped to the
   state dir, so the node reads it host-side) and atomically rewrites it
   ~1 Hz; a `daft sandbox observe <workspace>` CLI surfaces the latest
   snapshot (and `--follow`) through the **same orchestrator→node broker
   chain as `observer-tail`** (roadmap #3) — no new guest-side vsock
   listener, and the continuously-rewritten file means the latest
   snapshot is always on the host even across an abrupt VM teardown.
   Stuck/slow detection lives in the summary itself (e.g. "⚠ no openat
   activity for 47s while CPU idle"). This settles the guest→host
   transport question the gadget review left open: a refreshed summary
   file, not a raw firehose and not a bespoke per-gadget channel.

   Slices:
   - **6a — open-snoop aggregator** (*landed*). A pure, clock-free
     `opensnoop.Aggregator` rolls the decoded `Event` stream into a
     JSON-serialisable `Summary`: an **exact** `Total` (the primary
     liveness signal — a stalled build has a frozen `Total`) plus
     bounded approximate top-N rankings of processes, paths, and
     directories. The rankings use a Space-Saving heavy-hitters counter
     (`topK`, O(cap) memory) because a real `nix build` opens 100k+
     distinct files and an exact map would grow without bound. See
     `internal/observability/ebpf/opensnoop/aggregate.go`.
   - **6b — summary writer + observe share.** The gadget drain loop
     feeds the aggregator; the agent atomically rewrites the JSON
     summary ~1 Hz onto the per-sandbox *observe* share. The
     wall-clock enters here, so the **time-derived** signals
     (last-activity age, per-window rates) are computed at this layer,
     not in the clock-free aggregator.
   - **6c — `daft sandbox observe` CLI + broker.** Mirror the
     `observer-tail` RPC chain to serve the latest snapshot (and
     `--follow`); render a human/agent-readable summary with drill-down
     hints into the raw `opensnoop.log`.
   - **6d — stuck/slow heuristics + in-sandbox build e2e.** Flag stalls
     and pathological churn in the summary; an e2e runs a real build and
     asserts the summary reflects forward progress and then a stall.

   Deferred (needs a gadget enhancement, not in #6): ENOENT /
   cache-miss tallies require hooking `sys_exit_openat` for the return
   value — the current gadget only hooks `sys_enter_openat`, so the
   decoded `Event` carries no errno.
7. **More gadgets** — `biolatency`, `tcpstat`, `runqlat`, `pktstat`,
   each motivated by a real recurring symptom. Gated behind #6: one
   gadget (open-snoop) is enough to build and prove the summary pipeline;
   the rest plug into it.

## Related

- `internal/observability/` — the Tier 1 package.
- `internal/observability/ebpf/opensnoop/` — the first Tier-2 CO-RE
  gadget (open-snoop). Regenerate its BPF object with `make gen-bpf`.
  `aggregate.go` holds the roadmap-#6a `Aggregator`/`Summary` that rolls
  the event stream into the build-progress summary.
- `internal/agent/opensnoop.go` — wires the gadget into daft-agent
  (`startOpensnoopFromCmdline` + the `opensnoopDeps` seam), driven by the
  `daft.observe.opensnoop=` cmdline token (`internal/agent/cmdline.go`).
- `internal/sandbox/sandbox.go` — `Config.OpensnoopLogPath` →
  `buildKernelCmdline` emits the `daft.observe.opensnoop=` token;
  `internal/sandbox/integration_test.go` holds the in-VM e2e
  (`TestHelloIntegrationOpensnoopCapturesOpenat`).
- `scripts/cover-check.sh` — excludes bpf2go-generated `*_bpfel.go`
  loaders from the coverage gate (`SKIP_PATHS`).
- `internal/lint/analyzers/tagcatalog/` — skips generated files so
  bpf2go's GOARCH build tags don't trip the tag-catalog rule.
- `internal/sandbox/sandbox.go` — wiring (`cloudHypervisorRunner.run`).
- `internal/sandboxdcli/state.go` — per-sandbox state-dir resolution
  (`$XDG_STATE_HOME/daft/sandboxes`, `DAFT_STATE_DIR` override) and the
  default `ObserverLogPath` for the `hello`/`run` launch paths.
- `internal/sandboxexec/exec.go` — `Config.ObserverDir` defaults each
  orchestrator-dispatched VM's `ObserverLogPath` to `<uid>/observer.jsonl`
  and `registerObserverRoute` publishes uid→path into the per-node
  `sandboxgateway.ObserverRegistry`.
- `internal/observertail/` — the read-only tail/follow consumer of an
  observer log (handles rename-based rotation).
- `internal/orchestrator/observer.go` + `internal/nodectrlclient/observer.go`
  — the `observer-tail` cross-machine stream (client→orchestrator
  `ObserverTail`, orchestrator↔node `ObserverStream` rendezvous).
- `internal/cli/sandbox_observer_tail.go` — the `daft sandbox
  observer-tail` command.
- `internal/testdaemons/cluster.go` — `NewCluster` wires the sandboxd
  subprocess's state root to `Cluster.ObserverDir` (a `t.TempDir()`
  subdir) via `DAFT_STATE_DIR`, and registers the failure-only
  `dumpObserverLogs` Cleanup (`readTail` bounds each dump).
- `internal/testdaemons/workspace_profiler_test.go` — the in-guest
  `attachWorkspaceProfiler` is **transitional**. Tier 1 is now wired
  through `testdaemons.Cluster` (roadmap #4), meeting the first half
  of its retirement gate; roadmap #5c proved the Tier-2 mechanism
  end-to-end (open-snoop loads/attaches/decodes in a real guest), but
  the second half of the gate stays open until gadgets cover the
  specific guest-internal detail (PSI, per-process wchan, store-path
  counts) the host-side sampler can't see — open-snoop covers openat,
  not those.
- `CONTRIBUTING.md` "Slowness is a bug" — the policy this
  infrastructure exists to support.
