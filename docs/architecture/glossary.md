# Architecture glossary

Terms are singular and capitalised when they name a darkaifactory resource.

| Term | Meaning |
|---|---|
| Client | Trusted CLI, MCP, or UI caller acting with an authenticated identity. |
| Cluster | One Orchestrator and the Nodes, Clients, Workspaces, and Sandboxes it coordinates. |
| Orchestrator | Control-plane authority for identity, Node registration, Workspace declarations, and Sandbox bindings. |
| Node | One `daft-sandboxd` instance, identified by its mTLS certificate fingerprint. |
| Workspace | Durable, owner-scoped declaration and recoverable writable state reused across launches. |
| Sandbox | One launch of a Workspace: a fresh identity, VM, and workload container. |
| declaration generation | Monotonic Workspace specification version. |
| Sandbox generation | Immutable declaration generation materialised by a particular Sandbox. |
| observed generation | Latest declaration generation for which controller status has evidence. |
| retained disk | Node-local qcow2/XFS Workspace disk kept after Sandbox termination. |
| manifest | Generation-numbered description of Workspace files stored through the CAS. |
| adopted | Exact manifest digest and generation successfully materialised and acknowledged by the Client. |
| protected | Data that cleanup must not remove because no exact adoption, rejection, or deletion permits it. |
| terminal observation | Durable record that a particular Sandbox exited, was killed, or was lost. |
| condition | One independently observed status dimension with status, reason, message, and transition time. |
| reconciliation | Repeated comparison of desired state and observations, followed by an idempotent correction. |
| fencing | Preventing an old or unreachable actor from being mistaken for the current generation. |

`Ready` is intentionally not a glossary synonym for “stopped”, “healthy”, or
“safe to mutate”. A command must name and verify the precise conditions it
requires.

Parent: [Architecture](../../ARCHITECTURE.md).
