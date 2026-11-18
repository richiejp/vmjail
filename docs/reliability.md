# Local sandbox reliability

Keep the failure-handling properties of the VM runtime while removing the
remote control plane.

| Failure class | Local safeguard to preserve |
| --- | --- |
| Launch succeeds before preparation is complete | Validate required artifacts, shares, devices and resource configuration before workload execution. |
| Resource accounting misses a child | Place both the VMM and sharing processes in the per-VM cgroup at creation. |
| One stuck process blocks all cleanup | Close listeners first; reap children concurrently and report watchdog diagnostics. |
| A timeout kills legitimate work | Use explicit workload lifetime semantics; the smoke-test deadline is not a general command timeout. |
| Process exit races output draining | Finish consuming output before declaring completion; keep cancellation paths bounded. |
| Early workload activity outruns instrumentation | Wait for gadget attachment readiness before starting the workload. |
| Diagnostics disappear with the guest | Write guest observation data to a host-backed share and retain host resource logs after teardown. |

The core implementations are described in [Architecture](../ARCHITECTURE.md)
and [Observability](../OBSERVABILITY.md). The replacement local launcher still
needs to preserve their wiring and tests; removing distributed features does
not establish that this integration work is complete.
