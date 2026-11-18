# Architecture

vmjail is being reduced to a local Linux microVM sandbox. The host launches
Cloud Hypervisor and virtiofsd; the guest helper mounts the supplied directories
and runs an OCI container with crun. The VM is the security boundary. The
container supplies the workload's process and filesystem environment.

## Runtime components

- [Sandbox](docs/architecture/sandbox.md) and its
  [lifecycle](docs/architecture/sandbox-lifecycle.md): host preparation, guest
  configuration, workload execution and process cleanup.
- `internal/imagestore` and `internal/sandboxboot`: OCI image preparation,
  per-run bundles and host-directory share assembly.
- `internal/sandbox`, `internal/cgroupwrap` and `internal/userns`: VMM/virtiofsd
  supervision, resource accounting and namespace setup.
- `internal/agent`: guest mounts, crun execution, vsock control and guest
  instrumentation. This helper is part of the VM runtime.
- [Observability](OBSERVABILITY.md): host resource logs, guest eBPF IO and
  process summaries, and retained local diagnostic files.
- [Storage paths](docs/usage/storage-paths.md): host image caches, observation
  files and temporary sockets.

## Reduction status

The distributed control plane, web UI and workspace CAS implementation have
been deleted. Some retained launchers and tests still reference those packages,
so the complete application does not yet build. This page describes the local
runtime being retained, not a claim that its replacement CLI is finished.

The next change must separate local launch and observation setup from the old
remote dispatch paths. Direct host shares already exist, but the launcher must
preserve bundle ownership, cgroup/user-namespace setup and guest observation
configuration. See [PLAN.md](PLAN.md) for remaining work and validation status.

## Boundaries

Host paths are explicitly shared through virtiofsd, not copied into a workspace
CAS. Workload writes to a writable direct share affect that host directory.
Optional guest overlays have different persistence semantics and must be chosen
explicitly. Teardown must reap the VMM and sharing processes and retain
observation files. Guest control still requires vsock even without a remote
management service.
