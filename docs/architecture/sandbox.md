# Local sandbox

A sandbox consists of a host-supervised microVM and a workload container inside
it. Cloud Hypervisor runs the VM. A guest helper mounts virtiofs shares, obtains
configuration over vsock and invokes crun against an OCI bundle.

## State ownership

The host owns VM and virtiofsd processes, cgroup limits and accounting, image
bundles, explicit shared directories and retained diagnostic files. The guest
owns its mounts, container execution and eBPF instrumentation. Direct share
writes remain in the host directory; there is no remote synchronization step.

## Properties to preserve

- Required devices, artifacts and resource settings are checked before launch.
- The VMM and virtiofsd sidecars belong to the per-VM resource accounting group.
- Each host directory is exported through an explicitly configured share.
- Non-root container ownership is handled by bundle and share configuration.
- Cancellation tears down listeners and child processes and then the cgroup.
- Host resource logs and guest summaries remain inspectable after VM exit.

## Implementation map

- Process setup: [`cmd/vmjail-sandboxd`](../../cmd/vmjail-sandboxd/)
- VM supervision: [`internal/sandbox`](../../internal/sandbox/)
- Bundle and share assembly: [`internal/sandboxboot`](../../internal/sandboxboot/)
- Image storage: [`internal/imagestore`](../../internal/imagestore/)
- Guest runtime: [`internal/agent`](../../internal/agent/)
- Observation: [`OBSERVABILITY.md`](../../OBSERVABILITY.md)

These local pieces are retained, but the old command wiring still has imports
of deleted components. The new local launcher is not yet complete.

Parent: [Architecture](../../ARCHITECTURE.md).
Lifecycle: [Sandbox lifecycle](sandbox-lifecycle.md).
