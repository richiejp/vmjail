# Deletion results and remaining errors

## Obsolete cluster tests and real VM validation (latest)

Removed 32 files listed in [cluster-test-deletions.txt](cluster-test-deletions.txt).
The cluster tests compiled, but their harness tried to build deleted daft,
daft-mcp and orchestrator executables at runtime. Removed that harness and its
unused registry/Nix/SSH fixtures, including the SSH fixture test that depended
on harness helpers. Useful higher-level assertions remain available in git
history for porting to a local launcher. Standalone VM tests remain intact.

Simplified Makefile integration targets to build the retained VM artifacts and
hello bundle, preserving the integration lock and delegated cgroup scope.
Removed obsolete cluster image builds and egress-cache recording/publishing
steps from those targets.

Validation: current artifacts built successfully with `make dev`; the complete
default and integration-tagged suites pass. All 70 test-bearing packages pass,
including eight real VM tests for boot, shares, containers, imagestore bundles,
guest opensnoop, vsock tracing, exec attachment and overlays. The TUN test's
normal unprivileged skip was resolved by running it separately in an isolated
user/network namespace, where it passed. Default coverage is 94.70%; all
existing coverage gates pass. No tests were removed for failures in retained
VM behavior. Full make verify/benchmarks/active fuzzing were not run.

Logs: `/tmp/vmjail-all-integration-final.jsonl`, `/tmp/vmjail-tests-cleanup.log`,
`/tmp/vmjail-tests-cleanup-coverage.log`, `/tmp/vmjail-tun-test.log`.


## Direct host storage

No surviving Go files import the deleted workspace, CAS, casindex, brokerclient
or daftfuse packages. Removed sandboxd's remote `serve`/`workspace` commands and
CAS fetch/mount flags. Shared runtime utilities and their tests remain in
`internal/sandboxdcli/runtime.go` and `runtime_test.go`.

The executor accepts absolute host mount sources and reads devcontainer metadata
and build contexts from them. Removed materialization, live refresh/snapshot
routes, CAS preservation callbacks and their obsolete tests. Caller-owned host
directories are no longer subject to remote backing-tree cleanup. Scratch disks,
virtiofs sharing and VM observations remain. Existing devcontainer tests now use
real temporary directories; invalid remote mount sources are rejected.

The full default suite passes; total coverage is 94.69% against 94.43%, with all
files meeting the 80% gate. Focused race tests pass, including a corrected
secret-delivery test fixture. Integration-tagged tests compile, but real VM
execution remains unverified. No thresholds were weakened. The debug launcher
still uses the existing Hello runner; foreground workload lifetime and old
cluster fixtures are follow-up work.

Go vet, the custom daft-lint checks, architecture validation and whitespace
checks pass. Golangci-lint reports six gosec G115 integer-conversion warnings
in retained agent/CLI terminal, observer and userns code; none are in the
workspace/CAS changes. Log: `/tmp/vmjail-storage-golangci.log`.

Logs: `/tmp/vmjail-storage-full.log`, `/tmp/vmjail-storage-coverage.log`,
`/tmp/vmjail-storage-race.log`, `/tmp/vmjail-storage-integration-compile.log`.
The sections below record earlier stages and their then-current failures.


## Local devcontainer data types

Moved the needed Spec, Mount, EnvVar and PortMapping types into
`internal/devcontainerspec/spec.go`. Removed unused catalog build-job and node
selection fields, renamed projection to `ToSpec`, and updated executor callers.
Workspace mount sources now retain resolved host paths instead of being emptied
for CAS lookup. Updated the existing projection/overlay tests accordingly.

Agent-run catalog tests now use Badger directly instead of controlstore, keeping
persistence, reopening, corruption and state-transition assertions. Neither
deleted package is imported by surviving Go code.

Race-enabled focused tests pass (devcontainerspec 97.7%, agentruncatalog 94.8%,
devcontainer 96.6% coverage), as does go vet for the changed packages. The full
default suite passes 67 packages; three remain blocked by workspace/CAS imports:
sandboxdcli, sandboxexec and cmd/daft-sandboxd. No runnable tests failed.
Logs: `/tmp/vmjail-local-spec-tests.log`, `/tmp/vmjail-local-spec-full.log`.

## Proxy and HTTP-cache removal

Removed 14 files in [proxy-deletions.txt](proxy-deletions.txt), covering the MITM
proxy, HTTP cache, standalone command, proxy-specific integration tests and
obsolete egress guidance. Removed sandboxd's proxy/cache constructor, environment
options and related unit tests. Removed the proxy binary from the old cluster
build harness; preserved shared registry helpers used by devcontainer tests.

The generic executor/netstack proxy interfaces remain, but sandboxd no longer
constructs an interception proxy or injects its CA. Its existing no-proxy path
allows direct egress when networking is enabled. The old cluster fixtures still
need adaptation for local launch and no longer provide interception/replay.

The full default suite outside the sandbox passed 65 packages, with five setup
failures from earlier deletions. Integration compilation has six setup failures,
including the old cluster harness. No runnable default test failed. Full logs:
`/tmp/vmjail-proxy-removal-tests.log` and
`/tmp/vmjail-proxy-removal-integration.log`.

Devcontainer parsing is independent of workspacecatalog and controlstore.
`devcontainerspec` needs only four catalog data types; moving the needed fields
into a local launch spec will remove that dependency. Its workspace mount
projection also needs to preserve the resolved host source instead of emitting
an empty CAS-backed source. The sole remaining controlstore import is in
agentruncatalog tests. These adaptations are follow-up work.

## Client removal

Removed 61 files listed in [client-deletions.txt](client-deletions.txt):
`cmd/daft`, `internal/cli`, `internal/clientconfig`, `internal/daftclient`,
`cmd/daft-mcp`, `internal/mcpserver`, and three remote MCP/overseer tests.
The MCP facade operated exclusively through the removed remote client.

Preserved in `internal/cli`, with the applicable original tests:

- Terminal detection, dimensions, raw-mode restoration and SIGWINCH subscription.
- Observation summary rendering, stale/stalled detection and byte formatting.
- Host/container port argument validation.

The preserved helpers expose reusable APIs but are not yet wired into sandboxd.
Removed remote RPC pumps and command wrappers; guest exec/forwarding primitives
and the local `internal/observertail` reader remain. Cluster-based tests of useful
VM behavior remain pending porting to local fixtures. No protocol files were
removed in this pass.

Make/Nix no longer build the client or removed orchestrator. Nix's default
package and release archive now select sandboxd. The obsolete orchestrator Nix
module check/export was removed; the node module check remains.

Validation for this pass:

- Race-enabled cli tests pass, including PTYs, with 97.3% statement coverage.
- Race-enabled progress-summary and local trace-reader tests pass.
- Default compile-only check: eight packages still fail at setup due to prior
  deletions. The refreshed import index contains 23 imports across 13 files to
  seven removed packages, with no imports of the removed client/MCP packages.
- Nix syntax parsing, architecture validation and whitespace checks pass.
- Unsandboxed test execution was rejected by session policy: sandbox escalation
  is disabled. Focused tests ran inside the sandbox; no KVM tests ran.
- Full coverage and lint gates remain blocked by existing source errors.

Logs: `/tmp/vmjail-client-tests.log`, `/tmp/vmjail-client-compile.log`.
The following sections record earlier passes and their validation at that time.

## Documentation and test cleanup

After the initial deletion pass, removed **29 obsolete documentation/diagram
files** and **28 obsolete test files**. This includes the distributed architecture
pages and diagrams, dashboard guide, historical project plan, orchestrator tests,
node-control end-to-end tests against that server, and CAS transfer tests.
Shared helpers were preserved for surviving direct-mount tests; the handshake
test now owns its test-version value.

README, local architecture/lifecycle, observability, storage, reliability and
blackbox guidance now describe the retained local runtime. The architecture
checker still enforces links, diagram presence, lifecycle sections and word
limits for the retained component. The earlier audit reports remain historical
snapshots, not current user documentation.

Checks at that stage:

- Architecture checker and `git diff --check` pass.
- Sandbox integration-tag tests compile, as do the surviving node-control tests.
- Full default compile-only check still fails in **14 packages**, all at setup.
- **47 imports across 27 files** refer to nine deleted packages; the refreshed
  [import index](deletion-import-errors.tsv) lists them.
- Twelve retained core packages pass actual unit tests. Agent, sandbox and
  image-store suites hit socket permission failures; the user-namespace test
  still times out waiting for a child. No real VM was run.

The tool shell did not inherit the expected direnv toolchain. `direnv exec`
cannot connect to the Nix daemon socket, but installed Nix-store GCC, Graphviz
and gofumpt work directly. The latest Go checks used GCC with CGO enabled;
missing GCC is no longer a test blocker. Socket restrictions and absent
`/dev/kvm` remain.

Current logs: `/tmp/vmjail-doc-cleanup-compile.log`,
`/tmp/vmjail-doc-cleanup-tests.log`,
`/tmp/vmjail-doc-cleanup-sandbox-integration.log` and
`/tmp/vmjail-doc-cleanup-nodectrlclient-compile.log`.

## Initial 112 file deletion pass

Applied all **112 files** in [the approved manifest](deletion-candidates.tsv), removing approximately 32,000 lines. Each candidate was checked against HEAD before deletion; none contained local edits. No additional runtime files were deleted or rewritten. Go module paths and dependencies remain unchanged.

**The repository does not currently build as a whole.** Retained launchers, clients and tests still reference the removed remote components. This pass applies the requested deletion and identifies its consequences; the local-launcher extraction and consumer cleanup are still required.

## Checks performed

Final Go checks used `CGO_ENABLED=0`, `GOCACHE=/tmp/vmjail-go-cache`, `GOPROXY=off` and `GOSUMDB=off`. The initial environment had CGO enabled without a `gcc` executable; rerunning with CGO disabled removed that unrelated compilation failure.

| Check | Result |
| --- | --- |
| Exact manifest versus Git deletions | All 112 paths deleted; no extra runtime deletions. |
| `go list -e -deps -test -json ./cmd/... ./internal/...` | Identified missing internal packages and affected imports. |
| `go test -run '^$' ./cmd/... ./internal/...` | Failed: 16 packages have setup/build errors. This compiles tests without running them. |
| `go test -tags=integration -run '^$' ./cmd/... ./internal/...` | Failed: 18 packages have setup/build errors, including retained cluster-based VM tests. No integration tests executed. |
| Actual unit tests in 16 selected core packages | 12 packages passed; four encountered socket/namespace-related failures described below. |
| Real microVM execution | Not available: `/dev/kvm` is absent. |

Package-load errors can hide subsequent undefined symbols. The listed failures are a current diagnostic set, not a proof that fixing only the first reported imports will restore the build.

## Missing package imports

A scan of surviving production and test Go files found **65 direct imports across 39 files** referring to ten deleted packages. The initial counts below predate the documentation/test cleanup. The current file/line mapping is [deletion-import-errors.tsv](deletion-import-errors.tsv). It includes all checked-in Go files, including build-tagged tests.

| Deleted package | Remaining import occurrences | Representative affected code |
| --- | ---: | --- |
| `internal/brokerclient` | 7 | Client workspace commands and sandboxd remote workspace paths. |
| `internal/cas` | 13 | Workspace operations, client helpers and sandboxd remote storage setup. |
| `internal/casindex` | 3 | Sandboxd daemon state and tests. |
| `internal/controlstore` | 2 | Retained catalog tests and supporting code. |
| `internal/daftfuse` | 1 | Lazy remote workspace mount branch in `sandboxdcli/run.go`. |
| `internal/localcluster` | 2 | Client configuration and retained cluster fixtures. |
| `internal/workspace` | 16 | Transfer helpers, client/server callers and tests. |
| `internal/workspacebroker` | 1 | Retained orchestrator test. |
| `internal/workspacecatalog` | 16 | CLI, devcontainer, executor and orchestrator/test consumers. |
| `internal/workspacesync` | 4 | Client synchronization and tests. |

Go's diagnostic suggests `go get` for missing imports. These packages were deliberately deleted from this module; downloading them is not the fix. Remove their remote callers or replace the relevant branch with local behavior.

## Other consequences

- `internal/orchestrator/` now contains only retained non-dashboard tests. Consumers report “no non-test Go files”; `internal/nodectrlclient` tests depend on that old server. Port useful test assertions to a local harness, then retire obsolete server tests.
- `internal/cli/cli.go` still registers `newClusterCmd`. `internal/cli/clientconfig.go` uses `discoverLocalCluster`, and `internal/cli/config.go` uses `resolveIdentityDir`; those declarations were removed with the cluster files. These are additional source-confirmed blockers even where missing imports currently stop type checking first.
- `flake.nix` still refers to `nix/orchestrator.nix` in the deployment check and exported NixOS module. Remove those deployment/build entries as part of build cleanup. A Nix build was not run.
- `scripts/blackbox-agent.sh` and `scripts/test-blackbox-agent.sh` still expect `blackbox/browser-driver.js`. Browser portions of the old blackbox workflow must be retired or rewritten.
- Existing docs, release targets and cluster fixtures still describe/build the removed executable. The earlier scope inventories remain a snapshot of the pre-deletion source, not a navigation index for the new tree.

## Retained core test results

These 12 packages passed their actual unit-test runs:

```text
internal/observability
internal/observability/progress
internal/observability/ebpf/topk
internal/observability/ebpf/execsnoop
internal/observability/ebpf/iosnoop
internal/observability/ebpf/opensnoop
internal/observability/ebpf/sleepsnoop
internal/observertail
internal/cgroupwrap
internal/bundled
internal/sandboxboot
internal/cloudhypervisor
```

The remaining selected core packages compiled, but their test runs did not pass:

- `internal/agent`: loopback TCP listener creation returned `operation not permitted`.
- `internal/sandbox`: Unix socket setup returned `operation not permitted`; the fake virtiofsd sidecar also timed out waiting for its socket.
- `internal/imagestore`: the local `httptest` registry could not bind a listening socket.
- `internal/userns`: `TestReexecRealLandsInUserNSMappedFromSubID` timed out waiting for the child process. Its exact cause was not established in this pass.

These core source/test files were not changed. The socket failures are explicit execution-environment restrictions; the namespace timeout remains unresolved. These results do not establish that an actual VM launch works.

## Next cleanup boundary

The first functional follow-up is the retained sandboxd launcher: remove remote workspace/CAS branches, use direct shares, preserve cgroup/user-namespace setup and modern bundle ownership/cloning, and move guest observation-share setup out of `sandboxexec`. Preserve the local observation renderer and log reader while removing their catalog transport dependencies. Then retire the corresponding remote client/executor/test callers and clean build/deployment references.

This is larger than repairing a handful of import declarations. The approved manifest intentionally held these mixed files so their local functionality could be extracted instead of discarded.

Full diagnostics from this run are saved locally in `/tmp/vmjail-delete-compile.log`, `/tmp/vmjail-delete-integration-compile.log`, `/tmp/vmjail-delete-core-tests.log` and `/tmp/vmjail-delete-packages.json`. Coverage/lint/full verification cannot be claimed while repository-wide package loading fails. No coverage baseline or verification rule was weakened.
