# Local sandbox lifecycle

The diagram shows the retained local runtime's responsibilities, not a remote
state machine or a completed replacement CLI. See the
[Graphviz source](diagrams/sandbox-lifecycle.dot).

![Local sandbox lifecycle](diagrams/sandbox-lifecycle.svg)

## Transitions

| Stage | Work |
| --- | --- |
| Prepare | Validate required artifacts, shares, devices and resource configuration; assemble the image bundle and prepare namespaces and cgroups. |
| Start | Start virtiofsd sidecars, host control listeners and the VMM. |
| Configure guest | Deliver configuration over vsock; mount shares, start requested listeners and wait for instrumentation readiness. |
| Execute | Run the OCI workload with crun. |
| Finish or cancel | Choose completion-driven or persistent-VM behavior; close listeners and reap children. |
| Retain diagnostics | Remove the empty cgroup and temporary resources; leave observation files for inspection. |

`Hello` is a smoke-test runner with a 30-second deadline. `RunToCompletion`
waits for the guest completion event without that fixed deadline. `Serve`
supervises a persistent VM until cancellation or VMM exit. The local CLI must
choose its intended behavior explicitly.

## Initial invariants to formalise

Validate launch prerequisites before workload execution. A failed preparation
step must not leave unmanaged child processes. Guest attach and port-forward
listeners must be ready before a long-running container can block startup.

On completion or cancellation, close listeners first and reap children
concurrently so one stuck child cannot prevent cleanup of its siblings. Retain
watchdog diagnostics for stalled teardown, and remove the cgroup only after
child-process cleanup. Finish draining process output before reporting
completion, while keeping cancellation paths bounded.

The replacement local launcher must preserve this ordering and its tests.
[Observability](../../OBSERVABILITY.md) describes cgroup membership,
instrumentation readiness and diagnostic retention.

## Implementation map

- Host lifecycle: [`internal/sandbox/sandbox.go`](../../internal/sandbox/sandbox.go)
- Boot configuration: [`internal/sandboxboot/sandboxboot.go`](../../internal/sandboxboot/sandboxboot.go)
- Guest execution: [`internal/agent/agent.go`](../../internal/agent/agent.go)
- Cleanup tests: [`internal/sandbox/teardown_test.go`](../../internal/sandbox/teardown_test.go)
- Direct-share overlay test: [`internal/sandbox/integration_workspace_overlay_test.go`](../../internal/sandbox/integration_workspace_overlay_test.go)

Parent: [Sandbox](sandbox.md).
