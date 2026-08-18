# Node

A Node is one `daft-sandboxd` process and its durable local state. Its canonical
identity is its mTLS certificate fingerprint; labels and hostnames are display
metadata.

## State ownership

The Orchestrator records authorisation, connection state, registration time,
last-seen time, advertised resources, and live Sandbox reports. The Node owns
the physical truth about local VMs, routes, retained disks, and snapshot
progress. Neither view alone proves the complete Node lifecycle. Connection,
authorisation, heartbeat freshness, admission, and local inventory are
orthogonal dimensions; a single `connected` boolean cannot represent them all.

## Properties

- Registration is authenticated before the Node may receive Sandbox work.
- A connection is not authorisation, and authorisation is not current liveness.
- Missing heartbeats eventually make bindings unknown or unreachable, never
  successfully stopped.
- Reconnection resynchronises full bounded inventories before destructive
  recovery.
- Node-local cleanup revalidates current references immediately before unlink.

## Implementation map

- Public Node status: [`proto/operator/v1/operator.proto`](../../proto/operator/v1/operator.proto)
- Registration and heartbeat protocol: [`proto/nodectrl/v1/nodectrl.proto`](../../proto/nodectrl/v1/nodectrl.proto)
- Orchestrator registry: [`internal/nodectrl`](../../internal/nodectrl/)
- Node client and event bridge: [`internal/nodectrlclient`](../../internal/nodectrlclient/)
- Sandbox execution: [`internal/sandboxexec`](../../internal/sandboxexec/)
- Liveness reconciliation: [`internal/reconciler`](../../internal/reconciler/)

Parent: [Cluster](cluster.md). Lifecycle: [Node lifecycle](node-lifecycle.md).
Related: [Workspace lifecycle](workspace-lifecycle.md).
