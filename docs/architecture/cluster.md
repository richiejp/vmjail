# Cluster

The Cluster is a composition of independently progressing components. It does
not currently have one persisted lifecycle enum. “Accepting requests” means
the Orchestrator listener is live and the requested operation's own
preconditions can be proved.

```text
trusted user host                  sandbox host
┌────────────┐   mutual TLS   ┌──────────────┐   mutual TLS   ┌──────────┐
│ Client     │───────────────▶│ Orchestrator │◀───────────────│ Node     │
└────────────┘                └──────────────┘                └────┬─────┘
                                      │                           │ vsock
                               durable catalogue             ┌────▼─────┐
                               and control state              │ Sandbox  │
                                                              └──────────┘
```

## State ownership

| Component | Authoritative for | Not authoritative for |
|---|---|---|
| Client | Local source and exact materialisation acknowledgement | Node-local writes or Sandbox liveness |
| Orchestrator | Workspace declaration, binding, identity, and last accepted observations | Physical VM or disk existence |
| Node | Local Sandbox, VM, route, disk, and snapshot observations | Client materialisation |
| Sandbox guest | Workload/container execution and guest filesystem activity | Durable catalogue state |

The global state is the product of these views plus messages in flight. Node
heartbeat delay, RPC cancellation, and process failure create **unknown**
states; they must not be collapsed into successful destruction.

## Required invariants

- At most one current Sandbox generation may accept mutating Workspace control
  operations.
- A Sandbox binding names its Node, Sandbox UID, and Workspace generation.
- An Orchestrator observation cannot make protected data eligible for deletion
  without exact Client or operator intent.
- Replayed and duplicate control messages are harmless.
- A Node reconnect cannot silently attach an old Sandbox to a newer Workspace
  generation.

## Implementation map

- Orchestrator assembly: [`internal/orchestrator`](../../internal/orchestrator/)
- Node control protocol: [`proto/nodectrl/v1`](../../proto/nodectrl/v1/)
- Node registry and heartbeat state: [`internal/nodectrl`](../../internal/nodectrl/)
- Level-triggered reconciliation: [`internal/reconciler`](../../internal/reconciler/)
- Local development composition: [`internal/localcluster`](../../internal/localcluster/)

Children: [Node](node.md), [Workspace](workspace.md). Parent:
[Architecture](../../ARCHITECTURE.md).
