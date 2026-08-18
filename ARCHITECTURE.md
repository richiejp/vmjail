# Architecture

darkaifactory is a distributed isolation system. A trusted Client declares a
long-lived Workspace through an Orchestrator. The Orchestrator selects an
authorised Node, and that Node runs a short-lived Sandbox consisting of a VM
and one workload container. The VM, not the container, is the security
boundary.

The implementation is asynchronous: catalogue state, Node observations,
Sandbox execution, control streams, and retained storage can change
independently. A user-visible lifecycle state is therefore a derived claim,
not a field owned by any one component. The architecture model exists to make
those claims and their required evidence explicit.

## Global properties

- Untrusted workloads never receive the Client's ambient credentials.
- Only authenticated and authorised identities cross the control-plane trust
  boundary.
- A Workspace generation identifies the declaration used to create a Sandbox.
- Sandbox termination is not complete until writable state is preserved or a
  bounded, recoverable failure is recorded.
- Unadopted Sandbox writes are not discarded by cleanup or overwritten by a
  newer Client push.
- Loss of a Node changes what is known; it does not prove that its Sandboxes or
  retained disks were destroyed.
- Every wait and recovery action is bounded, idempotent, and diagnosable.

## Map

- [Cluster](docs/architecture/cluster.md) — component boundaries, concurrent
  state, and trust relationships.
- [Node](docs/architecture/node.md) — registration, liveness, execution, and
  disconnection.
- [Workspace](docs/architecture/workspace.md) — durable declaration,
  generations, and recoverability.
- [Workspace lifecycle](docs/architecture/workspace-lifecycle.md) — the first
  state-machine slice and its current implementation mapping.
- [Glossary](docs/architecture/glossary.md) — canonical terms used throughout
  the model.
- [Detailed resource-model reference](docs/architecture/resource-model-reference.md)
  — older, implementation-level material being split into the layers above.

The diagrams are explanatory projections. They do not yet constitute a proof
that the Go implementation refines a formal model. The next modelling step is
an executable specification of the Workspace lifecycle, followed by trace
mapping from implementation events to model actions.
