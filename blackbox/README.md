# Local sandbox blackbox fixtures

This directory retains workload fixtures and the existing source-blind scenario
format. The old distributed product and browser workflows are no longer the
supported scope. The CLI/harness is still being reduced and is not currently a
working end-to-end local test interface.

The fixtures can be reused by local VM tests:

- `workspaces/networked-go`: requests to a configurable HTTP endpoint.
- `workspaces/bun-package`: dependency installation and test execution.
- `workspaces/http-service`: a service with a health endpoint.
- `workspaces/mutable-tree`: host files for direct-mount read/write tests.
- `workspaces/disk-writer`: a bounded storage-growth workload.

[GOALS.md](GOALS.md) lists local behavior worth testing. Existing scenarios,
issue records and run history describe the earlier product; review them before
using them as acceptance criteria. In particular, remote catalog, transfer,
cluster and browser scenarios need retirement, not a replacement fake service.

When the local command is ready, run blackbox checks against its installed
binary without source access. Record the public operation, bounded output,
expected result and reproducible fixture inputs. Keep implementation diagnosis
separate from evidence of the observable failure.
