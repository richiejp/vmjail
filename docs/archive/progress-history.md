# Scope reduction progress history

Archived on 2026-11-19 after commit `243623e`. This is a chronological
record: intermediate claims and validation results describe their stage of
the reduction, not necessarily the current repository. See [archive index](README.md).

## Completed reduction

- Deleted the 112 files in the approved [deletion manifest](deletion-candidates.tsv).
- Removed 29 obsolete documentation/diagram files, including the old distributed
  architecture model, dashboard usage and historical project plan.
- Removed 28 obsolete tests covering the deleted orchestrator, cluster
  registration and CAS transfer flows. Kept tests of retained VM, observation,
  exec and networking behavior.
- Preserved shared filesystem helpers for surviving direct-mount tests and the
  test version needed by the node-control handshake test.
- Replaced README, architecture/sandbox lifecycle, observation and storage
  guidance with the local runtime scope. Updated blackbox goals and reliability
  guidance. The architecture checker now validates the retained sandbox pages
  and diagram with the same link, lifecycle and word-limit checks.

The [scope report](scope-reduction.md) and its function/test inventories are
pre-deletion audit snapshots. [Deletion results](deletion-results.md) and
the [remaining import index](deletion-import-errors.tsv) describe follow-up
work. Go module paths and retained executable names remain unchanged until after the cuts.

- Removed the distributed client CLI, authenticated client/config packages and
  remote MCP facade, plus their tests (61 files; see
  [client deletion manifest](client-deletions.txt)).
- Extracted terminal handling, port argument validation and observation rendering
  with their tests into `internal/cli`. These helpers have no RPC/protobuf
  dependency; local trace reading remains in `internal/observertail`. Renamed
  the extracted package from `localcli` to `cli` at the user’s request.
- Removed client/control-plane Make targets and Nix packages. The default Nix
  package and portable archive now select sandboxd. Its daemon-to-foreground
  conversion remains the next step.

- Removed `internal/mitmproxy`, `internal/httpcache`, the standalone proxy
  command, sandboxd proxy/cache construction, five proxy-specific integration
  tests and obsolete egress documentation. Kept shared registry helpers needed
  by devcontainer tests. See [proxy deletion manifest](proxy-deletions.txt).
- Moved the launch Spec/Mount/EnvVar/PortMapping types into
  `internal/devcontainerspec`, updated executor callers to `ToSpec`, and removed
  distributed build-job/node-selection fields. Workspace mount projection now
  preserves the resolved host source; image/build/overlay tests cover this.
- Removed the agent-run tests' controlstore dependency by opening Badger directly,
  retaining persistence/reopen, corruption and transition tests. No Go imports of
  workspacecatalog or controlstore remain.

- Removed all remaining Go imports of workspace, CAS, CAS index, brokerclient
  and daftfuse. Removed remote `serve`/`workspace` commands and their tests;
  retained reusable boot/config helpers in sandboxdcli/runtime.go.
- Executor mounts now require absolute host sources. Devcontainer metadata and
  builds use those directories directly; CAS materialization, refresh/snapshot
  routes and preservation/teardown callbacks are gone. Optional scratch disks,
  VM lifecycle, guest control and observation code remain.
- Adapted devcontainer/executor tests to real temporary host directories. Added
  registration lifecycle tests for the surviving protocol client and fixed a
  synchronization race in its secret-delivery test fixture.

- Removed obsolete cluster-dependent integration tests and their harness (see
  docs/archive/cluster-test-deletions.txt). Their runtime builds referenced deleted
  client/orchestrator commands. Kept standalone sandbox VM tests. Removed the unused SSH fixture too: it
  depended on the deleted harness and had no remaining callers.
- Simplified integration-build to local VM artifacts and the hello bundle;
  removed cluster image fixtures and deleted egress-cache command invocations
  from the integration targets.

- Removed the obsolete cluster/dashboard blackbox runner and its shell test
  from check-agent-tools, which the pre-commit hook invokes via make verify.
  Retained Go navigation checks and reusable blackbox workload fixtures.

- Replaced embedded asset cache files with sealed Linux memfds, inherited via
  child descriptor tables. Hashes are checked after sealing to reject any
  contents changed during population. Descriptors are retained until VM teardown; external
  development artifact paths remain supported. No writable-cache verification
  gap remains on the embedded path. Requires memfd executable/noexec sealing
  support; unsupported kernels fail rather than falling back to mutable files.
- Added seal/descriptor-lifetime tests and a real VM/container boot test using
  all four sealed assets; wired the latter into make integration.

- Removed unused mTLS, node registration/client, agent-run catalog/profile
  packages, plus the agentrun/operator/secretdelivery/workspace/workspacebroker
  proto sources and generated APIs. See docs/archive/unused-package-deletions.txt.
- Moved the CPU/memory probe and its tests into internal/hostresources so the
  retained executor no longer depends on the distributed client or registry.
  Kept local runtime helpers, build-image support and generic test tooling.

- Removed the remaining nodectrl and health proto sources and generated APIs
  (docs/archive/proto-deletions.txt). Executor lifecycle events/status now use local
  structs; trace interceptor tests use a generic unary RPC fixture. Sandbox
  launch data still uses the sandbox schema; agent and trace protocols remain
  for guest communication and observability.

- Removed the obsolete sandboxd auth commands, identity/peer trust package
  and unused mTLS hosted-registry transport constructor, plus their tests
  (docs/archive/auth-deletions.txt). Retained image-store transport coverage using a
  standard HTTP transport. Release smoke checks now target the surviving
  local binary, without deleted client/orchestrator services or auth commands.

- Removed private workspace QCOW2 disks, snapshot/seed transfers, disk-pressure
  control, guest XFS tools and resize tracing. Removed the unused Cloud
  Hypervisor disk-control client and guest-only mount option. Host virtiofs
  mounts and container rootfs cloning remain. Proto fields are reserved to
  prevent accidental reuse. See docs/archive/private-disk-deletions.txt.

## Current validation

Private-disk removal passes full `make verify`: lint, unit tests, coverage
(94.80%, unchanged 94.43% baseline; all files meet 80%), real VM tests with
the rebuilt initramfs, benchmarks and all five fuzz targets. Guest protobufs
were regenerated with `make proto`. Log: `/tmp/vmjail-disk-verify.log`.

Auth/identity removal passes full `make verify`: lint, unit tests, coverage
(94.86%, unchanged 94.43% baseline), real VM tests, benchmarks and all five
remaining fuzz targets. The removed sixth target tested identity fingerprints.
Log: `/tmp/vmjail-auth-verify.log`. Release smoke script passes shell syntax
checks; no release archive was built for this cleanup.

The nodectrl/health protobuf cleanup also passes full `make verify`: lint,
default tests, coverage (94.81%), real VM tests, benchmarks and all six fuzz
targets. Log: `/tmp/vmjail-proto-verify.log`.

The unused-package cleanup passes lint, default tests and coverage: 94.81%
against the unchanged 94.43% baseline, with every file meeting 80%. Real VM
integration tests pass, including sealed-asset container boot. Benchmark checks
and all six fuzz targets also pass; full `make verify` completed successfully.
The run is recorded in `/tmp/vmjail-unused-verify.log`.


For the sealed-asset change, `make check` passes in the Nix dev shell: formatting,
repository checks, lint, default tests and coverage. Coverage is 94.64% against
the unchanged 94.43% baseline; every gated file meets 80%. Focused race tests
pass for bundled, sandbox, sandboxboot and sandboxdcli. Tests cover sealed write,
size and executable-bit protection, content mismatch rejection, inherited
execution, partial-allocation/write cleanup and launcher descriptor lifetimes.

`make integration` passes all eight existing real VM tests plus the new sealed
asset/container test. After adding post-seal verification, the new container
boot test passed again. External development artifact paths remain supported.
Logs: `/tmp/vmjail-memfd-check.log`, `/tmp/vmjail-memfd-integration.log`,
`/tmp/vmjail-memfd-boot.log`, `/tmp/vmjail-memfd-race.log`.

The sealed-asset commit also passed the complete pre-commit `make verify`
pipeline in the Nix shell. Pre-commit and pre-push hooks are installed. All
verification thresholds remain unchanged.

