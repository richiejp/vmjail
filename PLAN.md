# vmjail living plan

The original protected project goal below was mistakenly removed in commit
`cf1f895` and is restored verbatim. It records the inherited darkaifactory
goal; the separately labelled current vmjail scope reflects the agreed fork
direction.

## Project goal

**AGENTS DO NOT EDIT THIS SECTION** (Yes that includes you GPT Astra)

Primary Goal: Make running a single process inside a VM sandbox as easy as 
              running it inside a namespace sandox.
Secondary Goal: Expose advanced observability features of the guest kernel and 
                allow configuration of the guest kernel per the application 
                requirements.



## Current vmjail scope

Reduce the inherited project to a local Linux microVM sandbox containing an OCI
container. Keep direct host-directory mounts, resource limits, reliable process
cleanup, host resource accounting and guest IO/process observations. Remove the
distributed control plane, web UI and workspace CAS/replication.

[Architecture](ARCHITECTURE.md) describes the retained runtime.
[Observability](OBSERVABILITY.md) describes the diagnostic surfaces to preserve.
Git history retains the removed project plans and distributed architecture.

## Completed work

The distributed control plane, web/client interfaces, CAS, authentication and
private workspace disks have been removed. Host-directory mounts, container
rootfs cloning, VM/guest observability and sealed embedded assets remain.

Historical scope reports, function/test inventories, deletion manifests and
validation records are in [docs/archive](docs/archive/README.md). The archive
includes the rationale and removed capabilities for QCOW2/XFS workspace storage.
Active architecture, observability and storage-path guidance stays outside it.
The standalone reliability checklist has been folded into lifecycle guidance;
accounting, instrumentation readiness and diagnostic retention were already
covered in OBSERVABILITY.md.

The obsolete remote-node deployment and AgentRun skills, their supporting
files, the MCP agent configuration and the deployment-skill Make check have
been removed. A local-run skill is deferred until the CLI workflow is defined.

## Current validation

The local CLI change passes `make verify`: formatting/lint, unit tests, real VM
integration tests, benchmark checks and all five fuzz targets. Coverage is
94.79% against the unchanged 94.43% baseline; every gated file meets 80%.
Additional non-root minimal-root and resolver tests pass after the final
permission checks. The Nix package builds, its embedded-assets executable
successfully boots a VM and preserves exit status, and the portable archive
passes its smoke test.

## Local CLI experiment

The nsjail-style `vmjail` entry point runs host executables in fresh VMs with
once/rerun/listener modes, filtered direct mounts, numeric identity, process
limits, optional userspace networking and retained observation files. No image
is required. Guest-owned devices and pseudo-filesystems replace host kernel
interfaces. The original image/bundle launcher remains available.

[Experiment report](docs/nsjail-experiment.md) records the supported examples,
unsupported flags, actual VM tests and unexpected namespace/virtiofs/PTY/vsock
problems. All eight new CLI end-to-end tests pass alongside the retained
VM tests. The packaged CLI and release archive include the new entry point.

The 2026-11-22 [startup/resource measurement](docs/nsjail-experiment.md#startup-and-resource-measurements-2026-11-22)
found approximately 0.75 s end-to-end latency, dominated by a 0.53 s VM boot
interval, with about 0.63 CPU-seconds and 131 MiB peak launch-cgroup memory.
The default 1 Hz host sampler misses short-run peaks; review faster sampling or
a final snapshot. The later checkpoint experiment below resolves VMM initialization versus early
guest boot.

The 2026-11-23 [kernel breakdown](docs/nsjail-experiment.md#kernel-boot-breakdown-2026-11-23)
identified roughly 53–54 ms of initramfs-worker time and 37 ms of tracing-format
metadata work in scheduler-instrumented boots. Diagnostic controls reduced
kernel-to-init from 129 ms to 96 ms with Zstandard or 86 ms with uncompressed
cpio; skipping tracing metadata alone yielded 89 ms. These were diagnostic
comparisons; tracing and BTF settings remain unchanged. Raw initcall wall times
overstate driver costs because workers share one vCPU. Further tracing metadata
optimization is deferred. The [guest userspace breakdown](docs/nsjail-experiment.md#guest-userspace-startup-breakdown-2026-11-23)
measured 22.7 ms median from successful `/init` exec to first shell output,
including 2.3 ms in crun and 15.9 ms in shell startup. Follow-up syscall and
stack traces identify a slow `/etc/nsswitch.conf` stat waiting for virtiofs
lookup through the overlay. A 33-boot static-probe comparison found ordinary
first stats of eleven paths take only 0.04–0.32 ms, including 0.22 ms for
`/etc/nsswitch.conf`; the slow shell-startup lookup still reproduces separately.
The startup/instrumentation dependency and host-side cause remain open. Seven
production CLI runs of the full static probe took 715 ms median end-to-end,
686 ms to first output, with only 1.01 ms spent in its 33 metadata operations.
The [end-to-end checkpoint experiment](docs/nsjail-experiment.md#end-to-end-checkpoint-breakdown-2026-11-23)
measured 12 ms VMM creation, 488 ms complete Linux boot, 4 ms init/agent,
about 5 ms probe launch/execution, and about 28 ms teardown, plus variable host
preparation. Clock synchronization bounds alignment to ±74 microseconds. The
previously unexplained roughly 394 ms is guest execution before its monotonic
clock origin. Booting the same extracted ELF via PVH reduced total kernel boot
to 118 ms. That motivated the PVH/performance default below. The build-policy
audit found inherited Linux size optimization and Cloud Hypervisor
`opt-level="s"`; only the kernel policy has been changed so far. The diagnostic
size-optimized ELF was 29.74 MiB and the raw development initramfs 13.28 MiB.

## Uncompressed initramfs default

Development and Nix builds now produce uncompressed `initramfs.cpio` artifacts,
including embedded assets and the ISO fixture. Explicit compressed initramfs
overrides remain supported. This applies the measured roughly 42 ms kernel-boot
improvement at the cost of a larger artifact. `make verify` passes, including
real-VM tests, with 94.60% coverage. Nix builds of the CLI, initramfs, and ISO
fixture pass; the packaged CLI also boots successfully using its embedded
uncompressed archive and preserves command output and exit status.

## Performance kernel and PVH default

The kernel fragment now explicitly selects performance optimization instead of
inheriting tinyconfig's size optimization. Development and embedded VM launches
use the uncompressed `vmlinux` ELF with its PVH entry point. The kernel output
retains bzImage only for the isolinux-based ISO fixture. Build-time checks
require the stripped ELF to retain the PVH physical entry note and BTF.
`make verify` passes, including real-VM tests, benchmark checks and five fuzz
targets; coverage remains 94.60%. Nix CLI and ISO builds pass, and a packaged
embedded-assets VM preserves output and exit status. The performance-optimized
ELF is 29.28 MiB. Seven production stat-probe runs measured 375 ms median
end-to-end (297–398 ms), versus the earlier 715–737 ms medians, with first
output at 343 ms. This comparison combines PVH and compiler changes; it does
not isolate the benefit of performance optimization. Cloud Hypervisor
optimization is unchanged.

## CLI consolidation review

Three Sol reviews covered the CLI-added source/test files and supporting
changes against existing namespace, stream/session, OCI, artifact, and
observability code. The refactor removes external `unshare` and test-only
`script` dependencies, shares native PTY allocation, stream decoding, OCI wire
types, artifact defaults and version handling, and groups setup/execution/cleanup
for readability. Distinct namespace mappings, session EOF semantics and mount
filtering policies remain explicit. See the [file-by-file review](docs/cli-refactor-review.md).

Full `make verify` passes, including real KVM tests, benchmark gates and all
five fuzz targets. Coverage is 94.79%, above the previous 94.60% measurement;
the baseline remains unchanged. Both Nix outputs build, the portable archive
passes its smoke test, and the packaged executable boots with embedded assets
and preserves stdin, stdout, stderr and workload exit status.

## Next implementation work

1. Decide which further nsjail features merit support: Kafel policies, cgroup
   option compatibility, configuration files and concurrent listener clients.
2. Add terminal resize forwarding and review complete signal semantics.
3. Review configured guest gadget failure propagation and final aggregation
   for short commands; preserve the existing observability facilities.
4. Reintroduce useful devcontainer/nested-VM assertions from history against
   the local launcher when those workflows are selected.

## Open scope choices

- Initial interface: a daemonless foreground CLI; defer detached/background VMs.
- Interactive execution is implemented; detached operation and additional port
  forwarding CLI controls remain open.
- MITM interception and HTTP caching are removed. Generic network/proxy hooks
  remain to be reviewed with the launcher.
  Sandboxd no longer creates a proxy or injects its CA; the existing no-proxy
  network path permits direct egress when networking is enabled.
- Devcontainer builds and credential injection. Private scratch disks are
  removed; host-directory mounts are the storage model.
- Diagnostic IO summaries versus additional instrumentation for a complete
  filesystem audit. Existing guest summaries do not provide the latter.
