# Sandbox

A Sandbox is one execution of one Workspace declaration generation. It has a
fresh UID, is bound to one Node, and contains one VM and one workload
container. The VM is the security boundary; the container is an execution and
filesystem convenience boundary.

## State ownership

The Orchestrator owns the accepted binding and last observation. The Node owns
VM, route, and local process observations. The guest owns container execution
and guest-side filesystem activity. A terminal Workspace transition therefore
requires evidence crossing all three boundaries.

## Properties

- UID and Workspace generation are immutable after dispatch.
- Boot preconditions fail before untrusted workload execution.
- Resource limits and configured isolation are required unless disabled.
- Attach, observer, secret, and port sessions are bound to the exact Sandbox.
- Terminal publication follows route closure and writable-state preservation.
- A lost Sandbox is unknown, not equivalent to a clean exit.

## Implementation map

- Wire resource: [`proto/sandbox/v1/sandbox.proto`](../../proto/sandbox/v1/sandbox.proto)
- VM construction: [`internal/sandbox`](../../internal/sandbox/)
- Boot path: [`internal/sandboxboot`](../../internal/sandboxboot/)
- Node executor: [`internal/sandboxexec`](../../internal/sandboxexec/)
- Guest agent: [`internal/agent`](../../internal/agent/)
- Control bridges: [`internal/sandboxgateway`](../../internal/sandboxgateway/)

Parent: [Workspace](workspace.md). Lifecycle: [Sandbox lifecycle](sandbox-lifecycle.md).
