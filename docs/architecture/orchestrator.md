# Orchestrator

The Orchestrator is the durable control-plane authority. It authenticates
connections, authorises operations, stores Workspace and AgentRun declarations,
records accepted Node observations, binds Sandboxes to Nodes, and bridges
bounded control streams.

## State ownership

The Orchestrator owns declarations and the last observation it accepted. It
does not own physical truth about a VM, route, disk, or Client filesystem.
Consequently, an RPC cancellation or missing heartbeat changes knowledge; it
cannot by itself prove that a remote side effect did not occur.

## Properties

- Durable state is opened and recovered before mutating requests are admitted.
- Caller identity comes from the authenticated transport, not request fields.
- Desired state and observations are stored separately.
- Bindings include Node, Sandbox UID, and Workspace generation.
- Bridge/session identifiers are bounded capabilities, not lifecycle truth.
- Shutdown refuses new work before closing controllers and durable stores.

## Implementation map

- Process configuration and serving: [`internal/orchestratorcli`](../../internal/orchestratorcli/)
- Service assembly: [`internal/orchestrator`](../../internal/orchestrator/)
- Durable Workspace catalogue: [`internal/workspacecatalog`](../../internal/workspacecatalog/)
- Durable control stores: [`internal/controlstore`](../../internal/controlstore/)
- Authentication and roles: [`internal/mtls`](../../internal/mtls/)
- Reconciliation: [`internal/reconciler`](../../internal/reconciler/)

Parent: [Cluster](cluster.md). Lifecycle:
[Orchestrator lifecycle](orchestrator-lifecycle.md).
