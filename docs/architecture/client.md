# Client

A Client is trusted software on the user host: the `daft` CLI, trusted MCP
server, or dashboard client. It holds local source, local mount mappings, and an
identity permitted to request control-plane operations. It never delegates its
ambient host credentials to a Sandbox.

## State ownership

The Client is authoritative for its local files and whether an exact pulled
manifest was successfully materialised. It is not authoritative for Node-local
disk contents, Sandbox liveness, or whether a timed-out remote operation took
effect.

## Properties

- Identity and trusted Orchestrator configuration are explicit local state.
- Local configuration updates either complete or restore the prior mapping.
- Push excludes control trees and cannot overwrite protected remote changes.
- Pull acknowledges only after exact materialisation succeeds.
- Retry preserves generation preconditions and request identity.
- Command completion is bounded and reports the last known remote phase.

## Implementation map

- CLI commands: [`internal/cli`](../../internal/cli/)
- Typed client operations: [`internal/daftclient`](../../internal/daftclient/)
- Identity and Orchestrator trust: [`internal/identity`](../../internal/identity/)
- Local configuration: [`internal/clientconfig`](../../internal/clientconfig/)
- Trusted MCP projection: [`internal/mcpserver`](../../internal/mcpserver/)

Parent: [Cluster](cluster.md). Lifecycle: [Client lifecycle](client-lifecycle.md).
