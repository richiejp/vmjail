# Cluster lifecycle

Cluster lifecycle is a derived view over the Orchestrator and its admitted
components. There is no persisted Cluster resource or current wire enum. The
[Graphviz source](diagrams/cluster-lifecycle.dot) is the visual projection.

## Abstract transitions

| Action | Required evidence | Result |
|---|---|---|
| Start | Configuration and durable stores validate | Control plane starts but admits no work |
| Listen | Required listeners and controllers are live | Authenticated connections may be evaluated |
| Accept | Identity, authorisation, and operation-specific dependencies pass | Request is admitted |
| Degrade | A dependency becomes unavailable | Affected operations fail closed; unrelated ones may continue |
| Drain | Shutdown or maintenance intent is recorded | New mutating work is refused |
| Stop | In-flight operations reach bounded outcomes and stores close | Process is terminal |
| Fail | A required startup or runtime invariant fails | Admission stops and bounded diagnostics are retained |

`Accepting` is operation-relative. A Cluster can list durable Workspaces while
refusing launches because no authorised Node has sufficient capacity. The
statechart therefore shows a presentation view, not one global availability
bit.

## Initial invariants to formalise

- `Accept(operation) => Preconditions(operation)`
- `Draining => not Accept(NewMutatingWork)`
- Losing all Nodes does not destroy Workspace declarations or protected data.
- Restart reconstructs durable authority before accepting mutations.
- Shutdown produces bounded outcomes for every admitted operation.

## Implementation map

- Local composition: [`internal/localcluster`](../../internal/localcluster/)
- Orchestrator startup: [`internal/orchestratorcli`](../../internal/orchestratorcli/)
- Health service: [`internal/orchestrator`](../../internal/orchestrator/)
- Node readiness: [`internal/nodectrl`](../../internal/nodectrl/)

Parent: [Cluster](cluster.md). Children: [Orchestrator lifecycle](orchestrator-lifecycle.md)
and [Node lifecycle](node-lifecycle.md).
