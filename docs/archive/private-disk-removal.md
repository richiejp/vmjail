# Removed QCOW2 and workspace storage handling

Removed on 2026-11-19 in commit `243623e`. The
[file manifest](private-disk-deletions.txt) lists deleted files; the commit also
contains edits to retained callers, build definitions and protobufs.

## Decision

vmjail uses explicitly mounted host directories through virtiofs. Persistence
and file access happen on the host. A separate persistent VM disk introduced
allocation, formatting, resizing, pressure control and file-transfer machinery
without a requirement for a second storage model. The implementation can be
recovered from Git if isolated private volumes become a concrete requirement.

## Removed behavior

- `internal/workspacedisk`: atomic creation and reopening of sparse QCOW2
  images keyed by workspace ID, virtual-size checks and growth, capacity
  reservations, host-space pressure checks and legacy mount migration.
- `internal/workspacesnapshot`: tar-based directory export and seed import over
  vsock, plus filesystem usage, capacity, flush and grow requests. These were
  directory transfers, not VM memory or execution-state snapshots.
- Executor disk attachment and lifecycle control: polling guest usage, pausing
  VMs near their disk allowance or under host storage pressure, coordinated
  virtual-disk/filesystem growth and explicit unfreezing.
- Guest XFS formatting, repair, mounting and bind-mount mappings; dedicated
  seed/snapshot listeners and bounded XFS-growth trace profiles.
- The Cloud Hypervisor disk-control client, guest-only mount option and private
  disk fields in host/guest configuration, together with their associated tests.
- XFS utilities and their shared libraries from both Make and Nix initramfs
  assembly. The obsolete disk-default environment resolver was also removed.

Before removal, no production caller opened the disk store or supplied it to
the executor. Guest support was conditional on receiving a disk configuration;
external callers of snapshot Pull and Flush had already disappeared.

GuestConfig field 10 (`workspace_disk`) and SandboxSpec field 17 (`disk_bytes`)
are reserved in the protobuf schemas to prevent accidental reuse. Generated
Go bindings were regenerated.

## Preserved behavior and validation

Direct host-directory mounts, the container root filesystem and its writable
layer, image/bundle cloning, guest IO/process observation and host resource
accounting remain. The general VMM API socket configuration remains as well.

Full `make verify` passed with a rebuilt initramfs: lint, unit tests, real VM
tests, benchmarks and all five remaining fuzz targets. Coverage was 94.80%
against the unchanged 94.43% baseline, with every gated file at least 80%.
