# nsjail-style local command experiment

The new `vmjail` CLI runs host executables inside a fresh Linux microVM. It
constructs an OCI specification for crun inside that VM; no container image is
pulled, and the executable and its libraries come from the selected host files.
The existing `vmjail-sandboxd` entry point remains available for its retained
image/bundle workflows.

This is an implemented subset of the examples in `~/c/nsjail/README.md`, not an
nsjail-compatible security policy parser. Unsupported flags fail instead of
being ignored. GUI applications, nested/multilayer sandboxes and arbitrary
nsjail protobuf configurations are outside this experiment.

## Running it

The Nix package builds both host entry points with embedded VM artifacts:

```sh
nix build
./result/bin/vmjail -Mo --chroot / --user 99999 --group 99999 -- /bin/sh
```

For a development build:

```sh
make dev
export VMJAIL_KERNEL="$PWD/bin/kernel/vmlinux"
export VMJAIL_INITRAMFS="$PWD/bin/initramfs.cpio"
export VMJAIL_CLOUD_HYPERVISOR="$PWD/bin/cloud-hypervisor/bin/cloud-hypervisor"
export VMJAIL_VIRTIOFSD="$PWD/bin/virtiofsd/bin/virtiofsd"
bin/vmjail -Mo -c / -- /bin/sh -c 'echo hello'
```

The host needs KVM access, unprivileged user/mount namespaces, util-linux
`unshare`, and delegated cgroup v2. The launcher obtains delegation through a
user `systemd-run` scope when necessary, following the existing host launcher.
The packaged executable embeds the VMM, kernel, initramfs and virtiofsd; it still
uses these host facilities and tools.

`--chroot /` exposes readable ordinary host files, including personal files.
Use a smaller root or explicit read-only mounts when that is not intended.
There is no automatic copying of executables or discovery of shared libraries.
Absolute symlinks and dynamic-loader paths must be available inside the chosen
root. On NixOS that commonly includes `/nix/store`.

## README example coverage

| nsjail example | Result in vmjail |
| --- | --- |
| Basic isolated shell, `-Mo -c / -u 99999 -g 99999` | Implemented and tested with the host `/bin/sh`, numeric identity, guest hostname and guest devices. Interactive input uses a guest PTY. |
| Inetd shell, `-Ml --port 9000 -c /chroot` | Implemented. Each connection gets a new VM and its socket becomes stdin/stdout/stderr. The chosen root must contain the shell and dependencies. Connections are serviced sequentially and the listener binds host loopback only. |
| Re-running process, `-Mr -c / -- /bin/echo test` | Implemented; each iteration gets a fresh VM. Nonzero workload exits are rerun, infrastructure failures stop the loop. Ctrl-C stops the launcher. Use an executable that exists on the host; this NixOS host has no `/bin/echo`. |
| CTF challenge hosting | The shown CLI primitives are implemented: listener, chroot, numeric identity, wall time, address-space and CPU-time limits. Command paths are interpreted inside the selected root: after `-c /srv/ctf`, a host file `/srv/ctf/challenge` is normally `/challenge`. The actual challenge binary was not provided. |
| Fuzzing | Rerunning, identity and limits work. The example's `--seccomp_string` is rejected: Kafel policy compilation is not implemented. `@@` is passed literally, as a workload argument. |
| Minimal environment / `find` | Empty-root execution and individual file mounts work; tested with a static host executable. Dynamic `find` additionally needs the host's actual loader and libraries. Omit `-R /dev/urandom`: host devices are rejected and a guest-owned `/dev/urandom` is supplied already. |
| Userland networking (`--use_pasta`) | Implemented using vmjail's existing userspace network stack, not the pasta executable. Tested with host curl reaching an HTTP server on the host through the VM's virtual host address. The guest gets a resolver configuration for its gateway. |
| MACVLAN networking | Not implemented; its flags are rejected. |
| Seccomp-bpf filtering example | Not implemented; the Kafel flags are rejected. VM isolation does not implement the example's syscall allowlist semantics. |
| Cgroup resource-control example | The nsjail `--cgroup_*` flags are not implemented. `--memory` controls guest RAM, `--max_cpus` virtual CPUs, and `--rlimit_*` controls the workload. These are distinct limits, not aliases for nsjail cgroup policies. |
| GUI/configuration-file/multilayer examples | Excluded. |

Run `vmjail --help` for the exact supported flags. Other supported primitives
include `--cwd`, `--hostname`, `-R SRC[:DST]`, `-B SRC[:DST]`, `-T DEST`,
`-E NAME=VALUE`, `--keep_env`, and `--rlimit_nofile`. Workload arguments after
`--` are passed unchanged. UID/GID options take numbers, and implemented rlimit
options take numbers rather than nsjail's symbolic `max`/`soft` values.

## Filesystem and observability behavior

The launcher makes a private mount namespace, clones selected host mount trees,
examines their actual mount types and masks pseudo-filesystems wherever they
are mounted. This includes alternate mounts of procfs, sysfs, devtmpfs, devpts,
cgroupfs and other kernel interfaces. If a protected submount is behind an
inaccessible directory, the nearest accessible ancestor is masked instead.
New host mounts do not propagate into the export. The temporary bootstrap
staging directory is also hidden from a host-root export.

Exports are recursively `nodev,nosuid`; read-only exports are recursively
read-only on the host. Explicit special-file mounts are rejected. The workload
gets guest-owned `/proc`, `/sys`, `/dev`, `/dev/pts`, `/dev/shm` and `/tmp`.
A small guest overlay lets crun create mount destinations without modifying the
host root. The resulting workload root is read-only; `-B` selects persistent
host writes and `-T` creates disposable guest storage. Bind destinations cannot
replace the guest root or its device/pseudo-filesystem paths.

Explicit bind exports translate the selected guest UID/GID to the invoking
host user. This allows a non-root guest workload to write an explicitly shared
host directory without changing its host ownership. Host permissions still
bound what virtiofsd can access. A VM reset discards guest scratch state, but
cannot undo writes already made through `-B`.

Host resource accounting is enabled for every run. Console logs and associated
resource logs are retained under the host temporary directory; infrastructure
errors include their console path. To also collect guest file/process/IO
observations and keep everything beneath a known directory:

```sh
vmjail -c / --observe-dir ./traces -- /bin/sh -c 'echo hello'
```

Each VM gets a separate `traces/run-*` directory containing console/resource
logs, syscall inventory and the enabled guest gadget outputs. These observations
are diagnostic summaries, not a complete filesystem audit. Very short workloads
can end before a periodic aggregate is emitted. Guest tracing remains outside
the workload's container.

## Unexpected difficulties found in real runs

1. **The inherited namespace re-exec did not provide usable mount privileges.**
   The initial Go/userns re-exec approach failed with `EPERM` while creating the
   private mount namespace. The first implementation worked around this with
   util-linux `unshare`. The consolidation replaces that dependency with Go
   pre-exec namespace setup: install the caller-to-root UID/GID maps and make
   mount propagation private before executing the launcher. The older
   subordinate-ID helper has a different mapping policy and is not a drop-in
   replacement.
2. **A non-recursive bind of host `/` failed with `EINVAL`.** In a rootless user
   namespace, inherited locked submounts prevent simply stripping all child
   mounts away. The working approach recursively clones the tree, then masks
   forbidden mount types before exporting it. Looking only at `/dev` and `/sys`
   would have missed `/run/docker/netns/...` on this host.
3. **Some forbidden mounts cannot be traversed even as namespace root.**
   Masking `/run/docker/netns/...` returned `EACCES`. Masking an accessible
   ancestor is required, and can hide more host content than just that mount.
4. **virtiofs submount announcements broke the guest overlay.** crun reported
   `cannot open proc`; probing the guest found `EREMOTE` when opening the
   overlay's `proc` directory, although the underlying virtiofs path worked.
   Disabling virtiofsd submount announcements for overlay-backed shares fixed
   traversal across the masked mounts and ordinary host submounts.
5. **Unix-socket half-close was not a usable stdin-EOF protocol over vsock.**
   Closing the write half after empty/piped stdin caused the command output
   channel to end prematurely. Stdin data and EOF are now explicit frames;
   stdout, stderr and exit status have their own frames.
6. **Interactive lifetime differs from ordinary process pipes.** Linux PTYs
   report `EIO` when their last slave closes; that must be treated as terminal
   EOF. Host stdin also needs cancellable polling so a finished VM does not
   leave a reader stealing input from the next rerun. Initial terminal size is
   forwarded, but live resize forwarding is not implemented yet.
7. **Host binaries have host-specific dependencies.** `/bin/echo` does not
   exist here, and `/bin/sh` points into the Nix store. Mounting just the shell
   file is insufficient for dynamic execution. Guest kernel interfaces and
   devices must be supplied by the VM even when all ordinary files come from
   the host.
8. **The old DNS helper assumed a container bundle's `rootfs/etc`.** The new
   root is an independent share, so that helper wrote to the wrong location.
   The generated OCI spec now mounts a gateway resolver file into the workload.
   Host loopback resolver settings are not reused.
9. **The full suite exposed an existing accounting-test race.** Reading an ARP
   reply can finish before its sender increments the transmitted-byte counter.
   The test now waits for the counter with a fixed bound, retaining its assertion.

10. **A shared delegated cgroup is not necessarily usable for a new launcher.**
    The full integration harness runs Go tests in a delegated scope. Moving just
    the CLI into a supervisor leaf left other test processes in the scope root,
    so enabling controllers failed with `EBUSY`. The CLI now obtains a fresh
    systemd scope on that specific failure, retaining the hard requirement for
    working cgroup accounting.

## Validation

Automated real-VM tests cover separate stdout/stderr and exit status, stdin EOF,
non-root execution, guest devices/hostname, writable/read-only host mounts,
working directory/environment, file limits, minimal-root static execution,
wall-time cancellation, repeated fresh VMs, two TCP connections, host curl over
the userspace network, retained observations and a real interactive PTY.

Unit tests cover argument rejection, mount classification and failure handling,
OCI specifications, framed binary IO, lifecycle failures, listener cancellation,
embedded artifact selection and input cancellation. `make integration` includes
the new CLI tests alongside the existing VM and bundled-asset tests.

Verification completed with `make verify` (including real KVM tests, benchmark
checks and all five fuzz targets), plus the final non-root minimal-root and
resolver checks. Total coverage is 94.60% against the unchanged 94.43% baseline;
every gated source file meets 80%. `nix build .#vmjail` and the portable archive
build passed. A packaged executable with all artifact environment overrides
removed booted a VM, ran a non-root host shell and returned its exit status 7;
`scripts/smoke-release.sh` passed for the archive.

## Startup and resource measurements (2026-11-22)

Measured `vmjail -Mo --chroot / -- /bin/sh -c 'echo ready'` on the development
host, with a fresh VM each time, default one vCPU/512 MiB guest RAM, no guest
networking and no optional guest eBPF gadgets. These are repeated runs with warm
host caches, not cold-cache measurements.

A temporary copy of the current launcher source used the same embedded VM
assets as the packaged executable, host-side stage markers, console markers,
10 ms observer sampling and external 5 ms cgroup sampling. Production code was
not changed. Five alternating baseline/profile pairs gave a packaged median of
759 ms end-to-end; five subsequent detailed profile runs had a 750 ms median
(705–757 ms), with first output at a median 719 ms. Treat these as approximately
0.75-second runs rather than claiming a precise profiling overhead.

| Stage | Median elapsed time |
| --- | ---: |
| Host startup and systemd cgroup delegation | 88 ms |
| User/mount namespace setup and CLI re-exec | 6 ms |
| Embedded assets loaded into sealed memfds | 26 ms |
| Filtered host mount view and OCI specification | 0.9 ms |
| Host listeners, VM cgroup, two virtiofsd processes and VMM spawn | 43 ms |
| VMM launched through guest `/init` appearing on the console | 530 ms |
| Guest init/agent/share setup through command-channel readiness | 3.6 ms |
| crun and shell launch through first output | 23 ms |
| Command exit and complete launcher teardown | 31 ms |

These are independent medians and need not add exactly to the median total.
Console boundaries include delivery latency. The guest kernel reports roughly
126–130 ms from its own timestamp origin to `/init`; the rest of the 530 ms
boot interval is not yet split between VMM initialization and early guest boot.
Do not attribute that remainder solely to Cloud Hypervisor without further
instrumentation.

Resource medians for the detailed runs:

- Total process-tree CPU time: **0.63 CPU-seconds** (roughly 0.54 s user and
  0.09 s system), averaging **84% of one logical CPU** across the full launch.
- VMM + virtiofsd cgroup CPU time: approximately **0.56 CPU-seconds**. Boot
  dominates CPU consumption; quota throttling was only about **0.6 ms**.
- Peak memory charged to the entire launch's delegated cgroup: **131 MiB**.
- Peak memory charged to the VMM + virtiofsd cgroup: **88 MiB**. VMM peak RSS was
  also about **88 MiB**. These overlapping views must not be added together.
- Guest RAM configuration: **512 MiB**; VMM/sidecar cgroup hard limit:
  **896 MiB**. Configured/addressable memory differs from resident memory.
- Peak task counts: **16 threads** in the VM cgroup, **38 threads** across the
  whole launch scope; these are not process counts.
- VMM read-like syscall byte count: approximately **11.2 MiB**, mostly artifact
  reads. Recorded VMM physical read/write bytes were zero and cgroup block-IO
  counters were empty in these warm-cache runs. This does not mean there were
  no filesystem operations, or predict cold-cache disk traffic.
- Guest workload networking was disabled.

The default one-second observer cadence produced only its initial sample for
this short command, before significant VMM allocation. Such logs substantially
understate short-run peaks; they cannot support this breakdown without faster
sampling or a final snapshot. Raw measurements and the temporary profiling
scripts are retained in `/tmp/vmjail-profile/` on this host.

## Kernel boot breakdown (2026-11-23)

Further profiling used the same Linux 7.0.10 kernel, one vCPU, 512 MiB guest RAM
and identical initramfs contents. A temporary launcher accepted diagnostic
kernel arguments. No production code, kernel configuration or shipped assets
were changed. Guest `dmesg`, `initcall_debug`, scheduler switches and workqueue
start/end events supplied the evidence. Raw logs and reproduction scripts are
in `/tmp/vmjail-kernel-profile/` on the development host.

Three-run medians measured from the kernel's timestamp origin to `Run /init`:

| Diagnostic condition | Kernel to `/init` |
| --- | ---: |
| Current gzip initramfs | 128.8 ms |
| Same, quieter console (`loglevel=4`) | 126.0 ms |
| Same, `initcall_debug` with quiet console | 127.4 ms |
| Same, synchronous initramfs unpacking | 126.5 ms |
| Same contents encoded with Zstandard level 3 | 95.6 ms |
| Same contents, uncompressed cpio | 86.1 ms |
| Gzip, skipping only `trace_eval_init` and `trace_eval_sync` | 89.0 ms |

The last row is a diagnostic attribution experiment, not a proposed production
configuration: tracing-format metadata supports the retained observability
facilities. The Zstandard decoder is already enabled in the tested kernel.
The compressed sizes were nearly identical: gzip 5,440,037 bytes; Zstandard
5,431,586 bytes. Uncompressed cpio was 13,871,332 bytes. Its dominant files are
the guest agent (9,789,672 bytes), u-root busybox (3,137,662 bytes) and crun
(942,888 bytes).

Scheduler/workqueue tracing adds overhead: two repeat traced boots took
137.9–139.6 ms rather than the approximately 129 ms control. Intersecting each
workqueue callback with the intervals during which its worker was actually
scheduled gives this breakdown. With one vCPU these buckets partition the
observed boot timeline; they are scheduled time, not hardware-cycle counts.

| Work in the traced boots | Time |
| --- | ---: |
| Early boot before the first scheduler event | 16.9–17.2 ms |
| Initramfs worker: gzip decompression and cpio extraction | 53.2–54.3 ms |
| `eval_map_work_func`: tracing-format metadata initialization | 36.6–37.4 ms |
| PID 1's remaining kernel initialization | 29.1 ms |
| Other threads and tracefs setup | approximately 1.8 ms |

About 4.1 ms inside the PID 1 bucket is `event_trace_enable_again`, caused by
turning on the diagnostic scheduler/workqueue events. Other measured PID 1
costs include CUBIC registration (about 5.1 ms), PCI enumeration (3.5–3.6 ms),
virtio PCI driver initialization (3.4–3.7 ms), slab sysfs setup (2.1 ms), PTY
initialization (1.6 ms), virtio console (1.3–1.4 ms) and virtio vsock (0.9 ms).
The CUBIC initcall includes BPF kfunc registration; its total is not proof that
TCP congestion-control calculations themselves take 5 ms.

The tracing-format worker is initialized by `trace_eval_init` in
`kernel/trace/trace.c`, which queues `eval_map_work_func` on `trace_init_wq`.
It calls `trace_event_update_with_eval_map`; `trace_eval_sync` waits for the
workqueue at late init. This work runs even when the CLI's optional guest eBPF
gadgets are disabled. Skipping these initcalls in the diagnostic boot reduced
the control by approximately 40 ms, independently corroborating the scheduler
attribution.

An additional uncompressed-archive scheduler trace measured only **10.9 ms**
in the initramfs worker, versus 53–54 ms with gzip; tracing metadata still took
36.8 ms. That approximately 42 ms difference closely matches the uninstrumented
kernel-to-init improvement from 128.8 to 86.1 ms. Switching the same contents to
Zstandard retained compression and saved approximately 33 ms in the controls.

### Why raw initcall rankings were misleading

The initramfs worker and tracing metadata worker share the single vCPU with
PID 1. An initcall's wall time includes periods when PID 1 is descheduled. In
one traced run:

| Initcall | Elapsed wall time | Time PID 1 was scheduled |
| --- | ---: | ---: |
| OOM reaper initialization | 6.37 ms | approximately 0.004 ms |
| VGA arbiter initialization | 7.95 ms | approximately 0.020 ms |
| PTY initialization | 18.14 ms | 1.65 ms |
| XFS initialization | 14.34 ms | 0.129 ms |

Consequently, removing a driver whose initcall appears to take 8–14 ms would
not necessarily save that time: most of the interval belongs to other kernel
work. Early `initcall_debug` durations also use `ktime_get` before the
high-resolution clocksource switch; this build has `HZ=250`, and early reported
durations are quantized in 4 ms steps. Printed boot timestamps and scheduler
intersections are more informative than those early duration fields alone.

The main demonstrated opportunities inside the measured kernel interval are
initramfs decoding and tracing-format metadata initialization. Quieter console
logging changed little. This investigation does not yet attribute the separate
approximately 400 ms before the kernel timestamp origin in the earlier
host-side VMM-to-init measurement.


## Default initramfs encoding (2026-11-23)

Following the measurements above, development and Nix builds now emit an
uncompressed `initramfs.cpio`, including the embedded runtime asset and the
nested-VM ISO fixture's initramfs. This trades approximately 8 MiB of artifact
size for the measured approximately 42 ms reduction in kernel startup. The
comparison tables above describe the gzip default at the time of profiling.
Compressed initramfs files supplied explicitly through `VMJAIL_INITRAMFS` remain
supported by the kernel. Release tarball compression is independent of the
initramfs encoding.

The measured tracing-format worker is separate from BTF database loading. It
walks compiled-in trace events, substitutes enum/sizeof constants into format
expressions and array dimensions, and sanitizes field type strings for the
tracefs `events/<system>/<event>/format` interface. There is a BTF connection:
`sanitize_field_type` removes compiler attributes, including BTF type-tag
annotations, from these textual descriptions. The worker does not parse the
kernel's BTF type database. The separately measured `cubictcp_register` initcall
includes BPF kfunc/BTF registration; its roughly 5 ms was not broken down into
individual operations. Neither tracing metadata nor BTF is disabled by the
initramfs encoding change.

## Guest userspace startup breakdown (2026-11-23)

Tracing-metadata optimization is deferred. Five fresh-VM diagnostic runs with
one vCPU, 512 MiB RAM, uncompressed initramfs, host root, networking disabled,
and optional guest gadgets disabled measured **22.7 ms median from the kernel's
successful `/init` exec to the shell's first `write(1, "ready\\n", ...)`**
(range 18.5–28.2 ms). These measurements exclude VMM startup, kernel boot,
output delivery to the host, and teardown. Host caches were warm; each guest
started with fresh filesystem caches.

| Guest userspace stage | Median wall time |
| --- | ---: |
| `/init` exec to u-root `main` (Go runtime/package initialization) | 0.67 ms |
| u-root setup through preparing child commands | 0.96 ms |
| Launch agent from u-root | 0.32 ms |
| Agent exec through entering `agent.Run` | 0.96 ms |
| Fetch configuration over control vsock | 0.45 ms |
| Mount shares and prepare overlay | 0.34 ms |
| Other agent setup, listeners, normalization and disabled gadgets | about 0.05 ms |
| Command vsock connection and hello | 0.23 ms |
| Start crun, including flushing diagnostic markers | 0.41 ms |
| crun exec through successful shell exec | 2.33 ms |
| Shell exec through first output syscall | 15.90 ms |

Medians are computed independently and need not sum exactly. The u-root setup
includes about 0.59 ms creating the root filesystem/mounts, 0.10 ms for its
network initialization, and 0.09 ms preparing OS-specific startup commands.
crun spends approximately 0.39 ms reaching the end of its two self-reexecs,
then 1.93 ms on the remaining container setup and shell exec. These timings
include time blocked on host filesystem requests, not just guest CPU time.

### The largest userspace delay is a filesystem lookup

The shell was scheduled for only **1.47 ms** of its 15.90 ms median startup
interval; **14.45 ms** was off-CPU. Its single longest off-CPU interval was
12.65 ms median (7.46–17.31 ms across those five runs). The rest of the shell
startup interval was about 4.27 ms median, including other filesystem waits.
This is not 16 ms of shell computation or crun setup.

Three follow-up runs with all syscall boundaries plus decoded `newfstatat`
arguments identified **`newfstatat(AT_FDCWD, "/etc/nsswitch.conf", ..., 0)`**
as the slow operation: **17.59, 18.84 and 20.41 ms**. The surrounding sequence
contains libc's account lookup: an attempted socket connection, reading NSS
configuration, then `/etc/passwd`. On this host `/etc/nsswitch.conf` ultimately
resolves to a file in `/nix/store`. This is specific to the current host
filesystem and shell, not a fixed cost every workload must pay.

A separate scheduler-triggered kernel stack capture confirmed the long wait:

```text
__x64_sys_newfstatat -> vfs_fstatat -> vfs_statx
  -> filename_lookup -> ... -> ovl_lookup -> ovl_lookup_layer
  -> fuse_lookup -> fuse_lookup_name -> __fuse_simple_request -> schedule
```

The guest is waiting for a virtiofs lookup through the root overlay. This
establishes the guest-side blocking point; it does **not** yet distinguish
host lookup work, virtiofsd scheduling/locking, or request/response delivery.
The next focused measurement would correlate that lookup with virtiofsd and
host filesystem activity. Removing crun would not eliminate this lookup by
the host shell.

### Method and limits

Temporary copies of u-root and the agent recorded `CLOCK_MONOTONIC` timestamps
at stage boundaries, then flushed them in batches to `/dev/kmsg`. Kernel exec,
selected syscall and scheduler tracepoints established process transitions,
first output, and scheduled versus off-CPU time. Trace clock `mono` matches the
explicit timestamps; the dmesg prefix uses a different clock and was not
subtracted from them. The interval starts at successful `/init` exec, so the
small kernel transition before that event is not included.

The diagnostic workload first executed `echo ready`, then mounted/read tracefs
and printed dmesg. Collection commands after the first write are excluded.
Only the temporary diagnostic launcher granted the capability needed to read
tracefs from the workload. All nine traces used for the table and follow-up
attribution had no ring-buffer overwrite. Additional tracepoints and marker
writes perturb timing; these are approximate diagnostic costs, not a new
production benchmark. Detailed syscall/stack runs are reported separately
from the five-run table.

Scripts, temporary source, raw traces and computed results are retained in
`/tmp/vmjail-userland/` on this host. Production binaries, kernel configuration,
and observability behavior are unchanged by this investigation.

### Comparison with other host files

A follow-up used a statically linked Go probe as the container command, without
starting a shell or calling libc's account lookup. Each of eleven paths was
measured first in three fresh VMs (33 boots), followed immediately by a repeat
stat and then the other paths. These runs used the production packaged CLI and
uncompressed initramfs, without the diagnostic guest or kernel tracing. Host
caches remained warm. The probe ran from the repository's ignored `bin/`
directory through the existing host-root share; an initial attempt using an
additional `/probe` share timed out and was not included.

| Path | First stat median | Immediate repeat median |
| --- | ---: | ---: |
| `/etc/nsswitch.conf` | 0.216 ms | 0.058 ms |
| `/etc/hosts` | 0.208 ms | 0.058 ms |
| `/etc/os-release` | 0.212 ms | 0.064 ms |
| `/etc/resolv.conf` | 0.320 ms | 0.057 ms |
| `/etc/passwd` (regular file) | 0.041 ms | 0.002 ms |
| `/etc/static` | 0.094 ms | 0.018 ms |
| `/bin/sh` | 0.163 ms | 0.039 ms |
| Resolved `/nix/store/...-etc-nsswitch.conf` target | 0.050 ms | 0.002 ms |
| Resolved `/nix/store/...-hosts` target | 0.054 ms | 0.002 ms |
| `/nix/store` directory | 0.042 ms | 0.002 ms |
| Nonexistent `/etc/vmjail-nonexistent-probe` | 0.040 ms | 0.002 ms |

The last row measures the expected ENOENT result. Successful first stats across
these cases took at most 0.330 ms. On this NixOS host, several `/etc` paths
traverse `/etc/static` to store files; resolving the target directly avoids
those extra lookups. Immediate repeats measure guest cache reuse, not a second
cold lookup. The probe's own startup necessarily touches some shared ancestor
directories, so "first" means first explicit stat of that path, not globally
cold host or guest caches.

Re-running the earlier instrumented shell scenario afterwards still produced
**5.91, 15.26 and 18.57 ms** stats of `/etc/nsswitch.conf`. Thus the earlier wait
is reproducible in that startup scenario, but **ordinary stats of that file or
other sampled files do not generally cost tens of milliseconds**. The isolated
probe changes the process startup/access sequence and removes instrumentation;
these results do not yet separate those effects. The blocked guest stack
identifies a pending FUSE lookup, but is insufficient to conclude that the file
itself is intrinsically expensive. Next attribution should compare the shell
and probe under matching instrumentation and correlate the host-side request.

The probe source, runner and all 33 results are in
`/tmp/vmjail-userland/statprobe.go`, `statcompare.py`, and `statcompare.json`;
three shell rechecks are `recheck-*.txt` in the same directory.

Seven subsequent production CLI runs timed the complete static-probe workload
(all eleven paths, each with first stat, repeat stat, and lstat): **715 ms median
end-to-end** (656–743 ms), including host setup, VM boot, command execution and
teardown. First output reached the host after **686 ms median** (625–709 ms).
The sum of the 33 timed metadata operations was only **1.01 ms median**
(0.66–1.45 ms); this sum excludes probe runtime initialization, JSON formatting
and output. These were fresh VMs with warm host caches and no diagnostic
instrumentation. All seven exited successfully and returned all 33 records.
The runner and results are `/tmp/vmjail-userland/statwall.py` and `statwall.json`.

## End-to-end checkpoint breakdown (2026-11-23)

Five fresh-VM runs now partition the entire static-probe launch. The VMM was
rebuilt temporarily from the pinned Cloud Hypervisor source with checkpoints
at `main`, VM allocation, memory-manager readiness, payload loading, system
configuration, and immediately before the first `KVM_RUN`. Guest checkpoints
cover u-root, agent setup, crun, probe main/work completion, and crun reaping.
The kernel's `sched_process_exec` event marks successful `/init` and probe exec.
The production observer sampling interval was retained.

| Stage | Median | Boundary |
| --- | ---: | --- |
| Host preparation | 207.9 ms | CLI invocation to VMM spawn request |
| VMM exec and VM creation | 12.3 ms | Spawn request to first `KVM_RUN` |
| Linux boot, including early compressed-image startup | 488.0 ms | First `KVM_RUN` to successful `/init` exec |
| Init and agent | 3.77 ms | `/init` exec to agent starting crun |
| crun launch/container setup | 1.92 ms | Start crun to successful probe exec |
| Probe Go runtime/package initialization | 1.52 ms | Probe exec to `main` |
| Probe workload, including JSON formatting/output | 1.39 ms | After diagnostic synchronization to final probe marker |
| Probe exit and crun reap | 0.28 ms | Final probe marker to `cmd.Run` returning |
| Exit notification | 0.14 ms | End of diagnostic collection to host cleanup starting |
| VM/sidecar teardown | 23.79 ms | Host cancellation to VM/sidecars reaped |
| Remaining host cleanup | 4.31 ms | VM cleanup finished to CLI process exit |

Medians are independent and do not sum exactly. Host preparation is variable
(118.5–218.3 ms here): its median includes approximately 128 ms of CLI/cgroup/
namespace startup, 37 ms extracting embedded assets into memfds, and 41 ms
starting share sidecars. This cost precedes VMM creation.

The instrumented total was **763.8 ms median** (670.7–774.7 ms). Subtracting each
run's explicitly measured synchronization and diagnostic collection intervals
gives **745.0 ms median** (654.6–756.2 ms). That adjustment does not remove all
instrumentation overhead and is not itself a production benchmark. Five fresh
uninstrumented production runs taken during the experiment measured **736.8 ms
median** (646.9–741.9 ms), consistent with the earlier seven-run median of
715 ms given the host-setup variation. All runs completed with 33 metadata
results and exit status zero.

### Clock alignment and diagnostic overhead

Host checkpoints and VMM checkpoints use host UNIX time. Guest checkpoints use
`CLOCK_MONOTONIC`, as does the kernel's configured trace clock. The probe sends
a guest timestamp, waits for a host acknowledgment, and then records a second
guest timestamp. For a host acknowledgment time H and guest send/receive times
G1/G2, the host-minus-guest offset is bounded by `[H-G2, H-G1]`. Intersecting
three exchanges gives an offset interval; analysis uses its midpoint.

The final bzImage runs bound alignment error to **±27–74 microseconds** (median
±45 microseconds), before small timestamping/tracepoint instruction overhead.
This avoids interpreting delayed console receipt as the event time. In fact,
some console lines arrived several milliseconds after the guest timestamp.
Kernel trace timestamps have microsecond display precision. The first-KVM
checkpoint precedes the actual ioctl by a short timestamp/logging operation.

The synchronization exchanges were explicitly excluded from the workload
interval. At command completion, the agent dumps the exec trace and pauses for
10 ms to let console data drain before host teardown; the measured whole
collection/pause interval (13.5–13.9 ms) is also excluded from the adjusted
total. The host closes stdin after the final acknowledgment. Leaving it open
in an earlier diagnostic run introduced an artificial input-drain delay; those
runs are excluded from the final table.

### The missing early time belongs to the guest boot path

The first-KVM checkpoint disproves the earlier suspicion that the unexplained
roughly 400 ms might be VMM initialization. With the production XZ-compressed
`bzImage`, the interval from first KVM entry to the guest monotonic-clock origin
is **393.9 ms median**; another **93.6 ms** elapses to successful `/init` exec.
The earlier kernel-log measurements covered approximately that latter interval,
not all execution of the Linux boot image.

As a diagnostic control, `scripts/extract-vmlinux` extracted the exact same
kernel's uncompressed ELF (about 30 MiB, versus the 6.1 MiB bzImage). Cloud
Hypervisor supports this kernel's PVH entry point. Three runs with that ELF,
without changing the kernel configuration or initramfs, gave:

| Interval | XZ bzImage | Extracted ELF/PVH |
| --- | ---: | ---: |
| VMM spawn to first KVM entry | 12.3 ms | 17.3 ms |
| First KVM entry to guest monotonic origin | 393.9 ms | 20.1 ms |
| Guest monotonic origin to `/init` exec | 93.6 ms | 97.7 ms |
| Total guest kernel boot | 488.0 ms | 117.8 ms |
| Adjusted end-to-end diagnostic total | 745.0 ms | 365.6 ms |

Loading the larger ELF costs the VMM a few milliseconds but bypasses roughly
374 ms of early guest execution. This implicates the compressed-image boot
path. It does **not** isolate XZ decoding from the other differences between
bzImage startup and direct ELF/PVH entry. A dummy kernel was unnecessary for
separating VMM time because the first `KVM_RUN` checkpoint provides that boundary
on the real workload. Production still uses bzImage; this experiment changes
no shipped boot settings.

Temporary source, the VMM patch/build expression, synchronization runner,
per-run records, analyzer, and extracted ELF are in `/tmp/vmjail-stages/`.
`analysis.json` and `analysis.log` contain the final stage results; `run-*.json`
are the five bzImage runs and `elf-*.json` the three ELF/PVH controls.

### Compression and size-optimization audit

The current kernel is **6,345,728 bytes (6.05 MiB)** as bzImage and
**31,185,192 bytes (29.74 MiB)** as the extracted bootable ELF. The development
initramfs is **13,924,512 bytes (13.28 MiB)**. Together the uncompressed kernel
and initramfs are approximately **43 MiB**; artifact size is not the same as
runtime guest memory consumption.

Two additional explicit size-oriented compiler settings remain:

- Linux's upstream `kernel/configs/tiny.config` selects both `CONFIG_KERNEL_XZ=y`
  and `CONFIG_CC_OPTIMIZE_FOR_SIZE=y`. Our fragments do not override those;
  the resolved config confirms size optimization rather than performance
  optimization. Thus using tinyconfig also inherited optimization policy,
  not merely a smaller feature set.
- The pinned Cloud Hypervisor `Cargo.toml` release profile specifies
  `opt-level = "s"`, alongside LTO and one codegen unit. The Nix derivation
  does not override that profile setting. Its effect on this workload has
  not been benchmarked; size optimization does not guarantee slower execution.

The pinned virtiofsd release profile enables LTO but does not override Cargo's
normal release optimization level. The crun derivation has no explicit
size-optimization flag. Go binaries use `-s -w` to omit symbols/debug data;
this is not executable compression or an instruction-optimization-for-size
setting. Embedded runtime assets are copied into memfds without an additional
compression layer. Initramfs is already uncompressed. Release `.tar.gz` files
and compressed source downloads are unpacked outside the per-VM launch path.
The kernel's enabled `CONFIG_RD_*` decoders only support optional compressed
initramfs inputs; they do not compress or decode the current raw archive.

Other inherited settings worth treating separately are periodic 250 Hz ticks
and disabled high-resolution timers. Those are timing/scheduling policy,
not compression, and should be evaluated against actual latency requirements.
SLUB_TINY has already been disabled by the hardening fragment; zram and zswap
are disabled. No changes to compiler optimization or boot defaults were made
by this audit.

## PVH and performance optimization default (2026-11-23)

Development and packaged launches now select the kernel's uncompressed
`vmlinux` ELF, entering Linux directly through PVH. The kernel config explicitly
sets `CONFIG_CC_OPTIMIZE_FOR_PERFORMANCE=y` and disables
`CONFIG_CC_OPTIMIZE_FOR_SIZE`, overriding tinyconfig's optimization policy.
Cloud Hypervisor's compiler profile is unchanged.

The kernel derivation emits a stripped ELF using the kernel boot image's own
objcopy flags (`-R .comment -S`), with build checks for the Xen physical entry
note and the `.BTF` section. This removes debug/symbol tables without removing
boot metadata or guest BPF type information. The integration harness and
embedded memfd asset now use `vmlinux`. Explicit bzImage overrides remain
supported, and the isolinux-based ISO fixture still uses the retained bzImage
output because its boot loader expects that format.

Validation passed: `make verify` (94.60% coverage, all gated files at least
80%, real-VM tests, benchmark checks, five fuzz targets), Nix CLI/ISO builds,
and a packaged embedded-assets shell command with verified output and exit
status. The resolved config confirms performance optimization, PVH, and BTF;
the final stripped ELF is **30,705,288 bytes (29.28 MiB)**.

Seven fresh production CLI runs of the same 33-operation static probe measured
**375.2 ms median end-to-end** (297.4–397.8 ms), with first output at **343.1 ms
median** (276.7–375.4 ms). The metadata calls themselves totaled 1.00 ms median.
Earlier production medians were 715–737 ms. These runs use warm host caches
and retain the variable cgroup/host preparation cost. The comparison combines
the new boot path and compiler optimization; it does not establish an isolated
speedup from `CONFIG_CC_OPTIMIZE_FOR_PERFORMANCE`. Results and runner are in
`/tmp/vmjail-userland/pvh-statwall.json` and `pvh-statwall.py`.
