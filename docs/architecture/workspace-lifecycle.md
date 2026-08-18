# Workspace lifecycle

This is the first end-to-end state-machine slice. It describes the abstract lifecycle a
caller may rely on; it is not yet a claim of formal implementation refinement.
The [Graphviz source](diagrams/workspace-lifecycle.dot) is the canonical visual
projection.

```sh
dot -Tsvg docs/architecture/diagrams/workspace-lifecycle.dot \
  -o /tmp/workspace-lifecycle.svg
```

## Abstract transitions

| Action | Required evidence | Result |
|---|---|---|
| Create | Owner/name is available | Durable declaration exists |
| Launch | Declaration is launchable; no unresolved prior binding | Fresh Sandbox UID binds one Workspace generation |
| ObserveRunning | Matching Node reports the bound UID running | Execution is positively known running |
| Stop | Matching binding exists | Teardown requested; mutations remain blocked |
| Preserve | Node verifies chunks and publishes the exact manifest | Writable state becomes durably retained |
| ObserveTerminal | Routes are closed and preservation outcome is recorded | Binding may be cleared |
| Recover | Binding is absent or fenced and retained state is inventoried | Safe stopped state is reconstructed |
| Reject | Exact protected generation selected | Sandbox changes become discardable; Client manifest remains authoritative |
| Adopt | Exact digest and generation materialised and acknowledged | Snapshot becomes recoverable rather than protected |
| Delete | No unfenced execution can mutate the disk | Catalogue and retained state are removed boundedly |

Node loss from Launching, Running, Stopping, or Preserving enters `Unknown`,
not `Stopped`. Recovery must reconcile catalogue binding, Node inventory,
Sandbox phase, broker/vsock routes, retained disk, and manifests. It may fence
an old generation, but must not overwrite Client files or protected writes.

## Initial invariants to formalise

- `SafeStopped => noCurrentBinding /\ noOpenControlRoute`
- `Protected(manifest) => not CleanupEligible(manifest)`
- `Adopted(manifest) => acknowledgedDigest = manifest.digest /\
  acknowledgedGeneration = manifest.generation`
- `Bound(sandbox) => sandbox.workspaceGeneration <= workspace.generation`
- A terminal observation for one Sandbox UID cannot clear a newer binding.

## Implementation map

- Launch/stop/delete handlers: [`internal/orchestrator/workspacecatalog.go`](../../internal/orchestrator/workspacecatalog.go)
- Terminal-event ordering: [`internal/sandboxexec/exec.go`](../../internal/sandboxexec/exec.go)
- Missing heartbeat and Node loss: [`internal/reconciler/reconciler.go`](../../internal/reconciler/reconciler.go)
- Lifecycle end-to-end tests: [`internal/testdaemons`](../../internal/testdaemons/)

Parent: [Workspace](workspace.md). Children: [Sandbox lifecycle](sandbox-lifecycle.md)
and [Storage lifecycle](storage-lifecycle.md). Terms: [Glossary](glossary.md).
