# Workspace

A Workspace combines a durable desired declaration with recoverable writable
state. Each launch derives a fresh Sandbox from one immutable declaration
generation. The Workspace outlives that Sandbox.

## State dimensions

Workspace state is deliberately multi-dimensional rather than one enum:

- **Declaration:** incomplete, launchable, or changed since launch.
- **Binding:** no Sandbox, dispatch pending, or bound to a Sandbox UID and Node.
- **Execution observation:** starting, running, terminal, lost, or unknown.
- **Storage:** absent, retained, protected, recoverable, frozen, or deleting.
- **Client synchronisation:** unpublished, published, materialised, adopted, or
  rejected at an exact manifest generation.

A presentation state such as “stopped” is valid only when its required
dimensions agree. `Ready` currently means that required declaration fields are
present; it does not mean that execution has stopped.

## Properties

- Specification mutations advance `metadata.generation`.
- Every Sandbox records the Workspace generation it materialised.
- Stale generation acknowledgements cannot adopt newer writes.
- Stop preserves writable changes before the terminal binding is cleared.
- Push cannot overwrite unadopted Sandbox changes.
- Delete and reject operate on the exact protected manifests reported by the
  Node, not only the Client's local mount map.

## Implementation map

- Wire declaration and status: [`proto/workspace/v1/catalog.proto`](../../proto/workspace/v1/catalog.proto)
- Durable catalogue: [`internal/workspacecatalog`](../../internal/workspacecatalog/)
- RPC transitions: [`internal/orchestrator/workspacecatalog.go`](../../internal/orchestrator/workspacecatalog.go)
- Client synchronisation: [`internal/workspacesync`](../../internal/workspacesync/)
- Retained disk: [`internal/workspacedisk`](../../internal/workspacedisk/)
- Snapshot and manifest publication: [`internal/workspacesnapshot`](../../internal/workspacesnapshot/)
- CAS provenance and protection: [`internal/casindex`](../../internal/casindex/)

Parent: [Cluster](cluster.md). Child: [Workspace lifecycle](workspace-lifecycle.md).
