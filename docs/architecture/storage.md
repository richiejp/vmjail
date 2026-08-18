# Workspace storage

Workspace storage spans the Client filesystem, immutable CAS chunks and
manifests, and a retained Node-local qcow2/XFS disk. No single copy is always
authoritative: authority depends on the exact manifest generation and whether
Sandbox changes have been adopted, rejected, or remain protected.

## State ownership

- The Client owns its local source and exact materialisation result.
- The CAS index owns chunk provenance, references, recoverability, and cleanup
  eligibility.
- The Node owns physical retained-disk and snapshot observations.
- The Orchestrator exposes accepted bounded summaries but does not infer disk
  destruction from Node loss.

## Properties

- Manifest publication is transactional after chunk verification.
- Existing unindexed chunks begin protected.
- A stale acknowledgement cannot adopt a newer manifest.
- Cleanup revalidates current references immediately before unlink.
- Failed snapshot leaves the retained disk as the protected authority.
- Resize is complete only after qcow2 growth and guest XFS growth succeed.

## Implementation map

- CAS bytes: [`internal/cas`](../../internal/cas/)
- Provenance index: [`internal/casindex`](../../internal/casindex/)
- Retained qcow2 disk: [`internal/workspacedisk`](../../internal/workspacedisk/)
- Snapshot publication: [`internal/workspacesnapshot`](../../internal/workspacesnapshot/)
- Client transfer: [`internal/workspacesync`](../../internal/workspacesync/)

Parent: [Workspace](workspace.md). Lifecycle: [Storage lifecycle](storage-lifecycle.md).
