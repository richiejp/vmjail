# vmjail local sandbox scope reduction

The smallest useful foundation is the existing microVM and guest-container stack, with direct virtiofs host mounts and local observation files. Remove the distributed control plane, workspace replication and web UI. Extract the useful boot and observation wiring from their current owners before deleting those owners.

This is a first-pass deletion plan against commit `d785afbedca3` (the copied darkaifactory history). It describes the pre-deletion source. The approved file deletion pass has since been applied; see [deletion results](deletion-results.md) for the current build failures and remaining work. Go module paths remain unchanged until the reduction lands. “Remove CAS” here means workspace chunk storage and transfer; the OCI image/layer cache is still needed for image-based container launches unless image pulling is explicitly dropped too.

## Evidence and inventories

There is one root Go module. The inventories below distinguish its packages from individual function declarations and from external modules.

| Entry point | Repository packages in production import closure | Reached production function declarations | Detailed inventory |
| --- | ---: | ---: | --- |
| `cmd/daft-sandboxd.main` | 62 | 971 | [Sandboxd functions and test callers](scope-analysis/sandboxd.md) |
| `cmd/daft.main` | 57 | 926 | [Client CLI functions and test callers](scope-analysis/client.md) |
| `cmd/daft-agent.main` | 18 | 341 | [Guest functions and test callers](scope-analysis/guest.md) |

Counts include generated code and conservative callback/interface references; they are not executable-size measurements. Packages imported for types/constants can have zero reached functions. The guest is listed separately because starting its binary is a process boundary, not a Go call from sandboxd.

- [Package matrix](scope-analysis/packages.tsv): every loaded internal package, each component's import membership and function count.
- [Recursive reference edges](scope-analysis/reference-edges.tsv): declaration-to-declaration edges, including globals, callback fields, interface dispatch and test helpers. Follow the `to` declaration as another row's `from` to recurse.
- [Test association index](scope-analysis/tests.tsv): 3,740 component/test-root associations, with the production declarations each test references directly or through test helpers. This includes unit tests, integration-tag tests, benchmarks, fuzz targets and examples; it is not 3,740 unique tests or a coverage result.

## Sandboxd entry points and recursive paths

### Process setup

[`cmd/daft-sandboxd/main.go`](../cmd/daft-sandboxd/main.go) `main` → `isHostSetupCommand`, then, for VM commands:

1. `cgroupReexec` → `cgroupwrap.EnsureDelegated` for a delegated cgroup v2 scope.
2. `prepareCgroupSubtree` → `observability.PrepareDelegatedSubtree`; `sandbox.SetVMCgroupParent` records the parent.
3. `usernsReexec` → `userns.Reexec` for subordinate UID/GID mappings.
4. `run` → `sandboxdcli.Execute` → `NewRootCmd` → Cobra command callbacks.

Keep this startup behavior for the new local command. The thin `daft` client entry point does not supply equivalent cgroup/user-namespace preparation. Renaming the client alone would lose this setup.

The sandboxd root currently registers `version`, `hello`, `pull`, `images`, `debug run`, `auth`, `workspace`, and `serve`. Preserve version/image operations and VM launch mechanics; remove identity bootstrap, workspace transfer and remote registration commands after their shared helpers have been separated.

### Existing local launch

[`internal/sandboxdcli/run.go`](../internal/sandboxdcli/run.go):

```text
newDebugCmd → newRunCmd → RunE → runRun
  → runOptions.validate
  → resolveWorkspaceDir
      local --workspace: host directory
      remote options: workspace RPC / broker / CAS / lazy FUSE
  → assembleBootConfig
      → openStore → imagestore.New
      → Store.Pull → Store.Bundle → Store.Lock
      → bundled.Extract
      → sandbox.NewStandaloneVsock
      → sandbox.Config with bundle + optional host-directory shares
  → applyObserverLog → defaultObserverLog
  → deps.boot = sandbox.Hello
```

The local `--workspace` branch already avoids workspace copying and CAS. Delete the `--workspace-from`, broker, lazy-attach and pull-back branches rather than reimplementing direct mounting.

However, do not simply rename `debug run` and call it finished:

- `sandbox.Hello` applies a 30-second timeout. `sandbox.RunToCompletion` uses the same lower-level completion path without that fixed deadline; `sandbox.Serve` supports a persistent VM until cancellation/exit. Choose lifecycle behavior explicitly, and verify workload failure/exit-code propagation.
- `assembleBootConfig` does not configure the guest observation share or full network/attach policy.
- The more developed `sandboxboot.BuildBundleConfig` and `buildShares` handle per-run bundle cloning and non-root mount UID/GID translation. Extract these capabilities rather than losing them by keeping only the older debug assembly.

### VM lifecycle and host mounts

[`internal/sandbox/sandbox.go`](../internal/sandbox/sandbox.go):

```text
Hello → helloWith → helloWithTimeout
RunToCompletion → helloWithTimeout with timeout 0
Serve → serveWith
  → configuration validation/defaults
  → createVMCgroup
  → startShares → virtiofsdSidecar.start → startOneVirtiofsd
      → translateArgs / hardenSpawn / placeInCgroup / waitForSocket
  → vsock configuration, trace and optional network listeners
  → cloudHypervisorRunner.run
      → buildCloudHypervisorArgs
      → hardenSpawn / placeInCgroup
      → startObserverIfConfigured → observability observer
  → completion event or long-running lifecycle wait
  → vmTeardown.run / process-group kill / reapWithWatchdog / cgroup removal
```

The three runners share these responsibilities but differ in ordering and completion behavior; the edge inventory records the individual implementations. Preserve cgroup placement of both virtiofsd and the VMM, resource limits, sidecar cleanup and cancellation. Host mounts use `ShareConfig.HostDir`, virtiofs tags and guest mountpoints. They are independent of workspace manifests.

The guest boot configuration and events travel through vsock; these remain necessary even without any host-to-host networking. `internal/cloudhypervisor` contains VMM control API support, distinct from the subprocess runner in `sandbox`.

### Distributed daemon path

[`internal/sandboxdcli/serve.go`](../internal/sandboxdcli/serve.go):

```text
newServeCmd → runServe
  → sandboxd identity / peer store / node resource detection
  → buildNodeSync → CAS + index + WorkspaceService + broker
  → gateway attach/job/observer/summary registries
  → workspacedisk.OpenStore
  → buildDispatcher → sandboxexec.New
      → sandboxboot.BuildBundleConfig → imagestore / bundled / shares
      → sandbox.Serve
  → newJobManager → nodejob.Manager
  → sandboxsecret.New
  → newServeNodeClient → nodectrlclient
  → runNodeServices → registration / heartbeats / workspace service
```

Remove the daemon registration, sync and relay structure. Before dropping `sandboxexec`, extract its `applyObserverLog`, `applyObserveSummary`, and the local portions of vsock/network/attach setup. Its devcontainer builds, disk lifecycle, remote dispatch and reconciliation are not prerequisites for a local mounted-directory sandbox.

### Guest process boundary

[`cmd/daft-agent/main.go`](../cmd/daft-agent/main.go) `main` → `agent.Main` → `agent.Run` → `runWith` builds production dependencies, obtains host configuration, prepares shares and starts the requested facilities. `applyShares` mounts virtiofs exports (and optional overlays); `runContainer` → `RunContainer` → `execContainerLinux` runs crun against the OCI bundle. Guest attach/exec, port forwarding, network bridging, tracing and observation have their own callbacks.

Keep this guest runtime. Its name “agent” refers to the VM init/helper, not an AI agent orchestration service. Remove workspace disk/snapshot and credential-delivery features only after narrowing its config and protocol. `agentprofile` and `agentruncatalog` serve a different purpose and are deletion candidates.

## Client CLI paths and reusable parts

[`cmd/daft/main.go`](../cmd/daft/main.go) `main` → `run` → `cli.Execute` → `cli.NewRootCmd`. The root registers version, sandbox, node, auth, ping, image, workspace, cluster, globals and blackbox groups.

| Path | Current recursive path | Local-sandbox disposition |
| --- | --- | --- |
| Sandbox hello | `newSandboxHelloCmd` → `helloConfigFromEnv` → `sandbox.ConfigFromEnv`, `NewStandaloneVsock` → `sandbox.Hello` | Keep as a smoke test; use sandboxd's process setup for the new executable. |
| Guest observation | `newSandboxObserveCmd` → `runObserve` → `runObserveWith` → orchestrator catalog dial and summary RPC → `renderObserveSummary` → `writeIosnoop`, `writeOpensnoop`, `writeExecsnoop`, `writeSleepsnoop` | Keep rendering and follow behavior. Replace workspace lookup/RPC with the local summary file. |
| Host observer tail | `newSandboxObserverTailCmd` → `runObserverTail` → `runObserverTailWith` → catalog `ObserverTail` → `pumpObserverTail` | Keep UX; tail the local rotating JSONL through `observertail`, without RPC. |
| Interactive exec | `newWorkspaceExecCmd` → `runWorkspaceExecWith` → `runExecSession` → `pumpExecSession`, stdin/stdout/resize pumps; catalog `AttachSandbox` | Salvage raw-terminal setup, resize, cancellation, output handling and silent-exec diagnostics. Replace Connect stream and workspace resolution with local guest attach. |
| Port forwarding | `newWorkspaceForwardCmd` → `runWorkspaceForwardWith` → `forwardOnePort` → accept loop → `runForwardTunnel` → stream pumps | Keep parsing/listener/lifecycle ideas if forwarding is retained; replace catalog tunnel transport with direct local VM/vsock wiring. |
| Node, cluster, auth, globals, workspace push/pull/serve/attach, hosted image push | Command callbacks → client config/identity/catalog clients and/or localcluster subprocess supervisor | Remove these distributed workflows. Localcluster is a local deployment of the distributed system, not the desired local sandbox. |
| Version/help/completion | Cobra tree and `version` | Keep, rename later. |

Useful source files are `internal/cli/sandbox_observe.go`, `sandbox_observer_tail.go`, `workspace_exec.go`, `workspace_exec_terminal_unix.go`, and `workspace_forward.go`. Most of `internal/cli` should be rewritten around the small local command surface, not retained wholesale. The function inventory lists all currently registered paths, including commands recommended for removal.

## Storage IO observability after CAS removal

| Surface | Existing implementation | Retain or change |
| --- | --- | --- |
| VM and sidecar resource totals | `sandbox.startObserverIfConfigured`; `observability` reads VMM `/proc` and per-VM cgroup accounting, including virtiofsd | Keep. Produces CPU/memory/IO/network time series and rotating JSONL. Cgroup IO accounting is not a per-file audit and is not equivalent to syscall byte counts. |
| Opened paths | Guest `startOpensnoop` → `observability/ebpf/opensnoop` | Keep. Independent of CAS; captures guest workload opens. |
| Read/write activity | Guest `startIosnoop` → `iosnoop.RunAggregating`; BPF syscall hooks aggregate per PID/FD and resolve paths through guest `/proc` | Keep. Summarizes file/socket/pipe IO and byte counts. It is sampled aggregation, not a complete ordered filesystem history; short-lived FDs/path resolution and operations outside the instrumented syscalls limit attribution. |
| Build/process activity | execsnoop, sleepsnoop, `observability/progress`, guest summary writer | Keep with local JSON summary rendering. |
| Observation export | `sandboxexec.applyObserveSummary` creates an observation virtiofs share and sets all gadget log/summary paths | Extract into the local launcher. The guest already writes files directly into a host-backed share; remove gateway summary registries and remote RPC access. |
| CAS read decisions | `daftfuse.ReadAt` → `readFileSpans` → `Policy.OnRead(ReadEvent{Path, Offset, Size, Digest})` | Remove with CAS. This is a chunk-read policy hook, defaulting to AllowAll, not a general read/write log. Direct virtiofs mounts bypass it. |
| XFS resize tracing | `agent.runXFSgrowTraced` and kernel trace profiles | Optional if virtual workspace disks are removed. Preserve general diagnostic patterns if useful, but host-directory mounts do not need XFS grow operations. |

Recommended first-pass contract: retain host resource logs plus guest path/IO summaries, and make both easy to inspect locally after VM exit. A complete per-operation host-directory audit or read interception/freeze policy would be additional work at the virtiofs or host filesystem boundary; the current gadgets should not be described as providing that guarantee. Neither snapshots nor CAS manifests are needed to keep the existing guest summaries.

## Package disposition

“Remove” means a target after callers/build entries are cut, not that the package can be deleted from the current tree without compilation failures.

| Decision | Packages | Reason or required extraction |
| --- | --- | --- |
| Keep core | `sandbox`, `bundled`, `imagestore`, `userns`, `cgroupwrap`, `cloudhypervisor` | VMM artifacts, OCI bundles, namespace/cgroup setup, VM control and cleanup. Trim hosted registry support from imagestore. |
| Keep guest core | `agent`, `genpb/agent/v1` | Guest configuration, shares, crun, control events. Narrow unrelated feature handlers and fields. |
| Keep observation | `observability` and its eBPF/progress subpackages, `observertail`, core `trace`, `component`, `genpb/trace/v1` | Host/guest diagnostics and trace framing. Strip Connect exporters/interceptors if only local/vsock traces remain. |
| Keep support | `oops`, `assert`, `version`; optionally `banner` | Small runtime helpers and build metadata. |
| Extract then shrink | `sandboxboot`, `sandboxexec`, `sandboxdcli`, `cli` | Boot assembly, non-root mounts, observation wiring and selected local CLI behavior are mixed with remote features. |
| Keep local portions if interactive execution is retained | `sandboxattach`, `sandboxjob`, guest attach/job code | Guest protocol and exec plumbing are useful locally; remove node job orchestration. |
| Keep local networking; decide optional proxy features | `sandboxnet`, guest net/port-forward code; `mitmproxy`, `httpcache`, `egresscache` | Isolation/forwarding remain useful. HTTPS MITM, replay and caching are distinct optional product features, not required by local mounting. Remove proxy/cache packages only after choosing the local egress policy. |
| Remove distributed control | `orchestrator`, `orchestratorcli`, `localcluster`, `nodectrl`, `nodectrlclient`, `reconciler`, `controlstore`, `workspacecatalog`, `workspacebroker`, `brokerclient`, `workspacesync` | Fleet state, catalogs, dispatch, relays and synchronization. |
| Remove workspace CAS | `cas`, `casindex`, `daftfuse`, remote `workspace` service | No manifests/chunks/replication needed for direct mounts. Remove remaining consumers such as snapshot/build paths before deletion. |
| Remove remote client/auth infrastructure | `daftclient`, current `clientconfig`, `identity`, `mtls`, remote `genpb` Connect services | No host-to-host trust or operator enrollment in the proposed local interface. Keep only any small local settings facility actually required. |
| Remove agent orchestration | `agentruncatalog`, `agentprofile`, `mcpserver`, `nodejob` | Separate automated-workflow/MCP/node job functionality. Not the guest VM agent. |
| Remove or defer explicit optional features | `devcontainer`, `devcontainerspec`, `buildimage`, `workspacedisk`, `workspacesnapshot`, `agentcredential`, `sandboxsecret` | Image-building/devcontainer support, private workspace disks/snapshots and provider-specific secret injection exceed the minimal image + host mounts goal. Extract credential-independent mount code first. |
| Remove after callers are replaced | `sandboxgateway`, `ociregistry` hosted server | Gateway registries/bridging and orchestrator registry are unnecessary, but boot/CLI code and `imagestore/pull.go` still refer to them. |
| Adapt testing/tooling | `testdaemons`, `blackbox`, `testtmux`, `kconfig`, `lint/*`, `sandboxnet/sandboxnettest` | Retain low-level tests and kernel validation; replace cluster fixtures. Runtime import reachability does not decide build/test tool usefulness. |

Generated `workspace`, `workspacebroker`, `nodectrl`, `agentrun`, `operator`, `health`, and `secretdelivery` protocols are deletion candidates with their remote consumers. `genpb/sandbox/v1` currently supplies boot/egress specification types; replace those with local config types before removing it. Keep guest/vsock and trace schemas as needed; “remove RPC” does not imply “remove all protobuf.”

External module candidates after pruning include Badger (catalog/CAS index), go-fuse (lazy CAS mount), WebAuthn (web login), MCP SDK (MCP server), and JSON schema validation (devcontainers). Connect and its HTTP dependencies can go only after tracing and any retained generated services stop importing them. Keep cilium/ebpf, container registry libraries, namespace/network libraries and Cobra as their retained features require. Let `go mod tidy` establish the final module set after edits; do not delete dependencies merely because their largest consumer is removed.

## Tests to preserve and port

The inventories identify exact test callers; the following groups deserve attention before deleting the containing higher-level package.

| Behavior | Existing tests | Action |
| --- | --- | --- |
| Startup setup | `cmd/daft-sandboxd/main_test.go`, `internal/cgroupwrap/*_test.go`, `internal/userns/*_test.go` | Keep delegation/reexec and setup-command checks in the new entry point. |
| Direct mount/container assembly | `internal/sandboxdcli/run_test.go`: `TestRunWiresBundleShareAndBundlePath`, `TestRunAddsWorkspaceShareAndMount`, `TestRunWorkspaceOverlayPopulatesConfig`, `TestRunResolveWorkspaceAbsoluteOnRelativeInput` | Port local cases; drop broker/CAS cases with those features. |
| Boot assembly and ownership | `internal/sandboxboot/sandboxboot_test.go`, `internal/imagestore/*_test.go` | Preserve mount validation, bundle locking/cloning and non-root ownership cases. |
| Host observation defaults | `internal/sandboxdcli/state_test.go`; `internal/sandboxexec/exec_test.go`: `TestHandleSandboxEventDefaultsObserverLog`, `TestHandleSandboxEventObserverLogBaselineWins` | Port semantics into local launcher tests. |
| Guest observation export | `internal/sandboxexec/exec_test.go`: `TestHandleSandboxEventRegistersSummaryRoute`, `TestHandleSandboxEventNoSummaryRouteWhenOptedOut` | Keep share/path/enablement assertions; replace registry/RPC assertions with local output paths. |
| VM cleanup and limits | `internal/sandbox/{teardown,cgroup_limits,cgroupfd_linux,trace,wait_boot,vsock_config}_test.go`, observability sampler/cgroup tests | Keep; these are local invariants. |
| Guest execution and observation | `internal/agent/{container,share,opensnoop,iosnoop,execsnoop,sleepsnoop,observe_summary}_test.go`, `internal/observability/ebpf/*/*_test.go`, progress tests | Keep and narrow config/test fixtures as remote features disappear. |
| Human-facing diagnostics | `internal/cli/sandbox_observe_test.go` (`TestRenderObserveSummary*`), observer-tail tests, exec terminal tests | Preserve renderer/terminal semantics; rewrite dial/RPC-specific tests. |
| Real VM smoke tests | `internal/sandbox/integration_test.go`: `TestHelloIntegrationBootsVM`, `TestHelloIntegrationBootsVMWithShare`, `TestHelloIntegrationBootsVMWithContainer`, `TestHelloIntegrationOpensnoopCapturesOpenat` | Retain as core local end-to-end tests; add the new launcher's long-running command and IO-summary wiring cases when implementing it. |
| Cluster-based useful scenarios | `internal/testdaemons/workspace_nonroot_mount_test.go`, `workspace_profiler_test.go`, `workspace_forward_test.go`, `workspace_virtiofs_latency_test.go` | Port useful VM/IO assertions to a local harness before deleting the multi-process cluster fixtures. |

Integration-tag test associations are included in `tests.tsv`. Tests that launch a binary by filename rather than call a Go function are not fully recoverable from Go reference analysis; the manual scenario list above covers important examples. Broad helper references such as `oops` associate tests with multiple components and are not a recommendation to keep those tests' whole packages.

## Non Go components and executables

| Component | Disposition |
| --- | --- |
| `internal/orchestrator/web/`, dashboard HTTP/template/auth Go files, `web/terminal/`, `scripts/update-terminal-assets.sh`, browser-driver scenarios | Remove web UI, xterm assets and Bun terminal dependency workflow. Embedded assets are registered in `internal/orchestrator/dashboard_assets.go`. |
| `cmd/daft-orchestrator`, `cmd/daft-mcp` | Remove with control-plane/MCP packages. |
| `cmd/daft-mitm-proxy`, `cmd/daft-egress-cache` | Remove if optional MITM/replay/cache features are excluded. They are not essential VM components. |
| `cmd/daft-agent`, current sandboxd entry point | Keep guest helper and adapt sandboxd into the local executable. Merge selected client commands into it rather than requiring two local management programs. |
| `cmd/kconfig-verify`, `cmd/daft-lint`, Go navigation and test scripts | Build/quality tooling, not cluster runtime dependencies. Keep initially and prune rules tied to removed architecture separately. |
| `cmd/daft-hello-container`, `cmd/daft-hello-server`, `cmd/daft-http-fetch`, `cmd/daft-iso-fixture`, `contrib/hello-bundle` | Test fixtures; retain those used by surviving boot/network/nested-VM tests. |
| `third_party/cloud-hypervisor`, `third_party/virtiofsd` | Keep required Rust VMM and sharing daemons, including their build pins. |
| `third_party/crun` | Keep C OCI container runtime and required libraries/build inputs. |
| `third_party/u-root` | Keep current initramfs/init build dependency (Go, but a separate upstream module). |
| Linux kernel configuration/build | Keep KVM guest, virtiofs/vsock and BPF/BTF support. `flake.nix` currently fetches a kernel tarball rather than using the `third_party/linux` checkout; that submodule may be removable after checking remaining build/script references and regenerating submodule metadata. |
| `internal/observability/ebpf/*/*_bpf.c`, generated Go and `*_bpfel.o` | Keep. These C/ELF assets implement the requested guest observability; normal builds consume committed objects. Keep clang/bpf2go generation and kernel BTF tooling for maintenance. |
| `nix/orchestrator.nix`, `nix/node.nix` | Remove distributed service deployments; extract any useful local sandbox packaging separately. |
| `flake.nix`, `flake.lock`, Makefile, release scripts and CI | Trim orchestrator/UI/cache artifacts and cluster tests, but preserve VMM/kernel/virtiofsd/crun/initramfs builds, embedded assets, local release and verification. |
| `blackbox/`, browser tooling, agent-run history/scenarios, deployment skills | Remove obsolete distributed/UI workflows; salvage small filesystem/network workload fixtures for the local test harness. |
| Architecture diagrams/docs and their validation scripts | Replace cluster topology/lifecycle assertions and distributed operational docs. Keep local VM, mount and observation documentation. |

## Suggested cut order

1. Extract a local launcher using sandboxd's setup, direct shares and image bundles. Reuse the modern bundle/mount ownership handling. Define one-shot versus persistent execution explicitly.
2. Move observer JSONL defaults and the guest observation share out of `sandboxexec`. Read/tail these files locally using the retained CLI rendering. Port the relevant tests in the same step.
3. Wire only the required local execution/networking features. Retain guest vsock control, choose egress behavior, and remove dependencies on gateway registries/catalog lookup.
4. Remove web UI and distributed command registration, then orchestrator/cluster/broker/node-control packages and their deployment artifacts.
5. Remove workspace CAS/FUSE/sync/catalog, snapshot/disk and remote-build paths once no retained launcher or guest handler imports them. Remove remote identity and generated services as their final consumers disappear.
6. Trim build outputs, fixtures and docs; run the remaining unit and real-VM checks. Then rename Go imports, executable names, cache paths, flags and environment variables to vmjail and tidy modules.

Open product choices are persistent versus one-shot VMs, interactive exec/forwarding, optional network MITM/cache, devcontainer builds, private scratch disks and credential injection. None needs to block removing the UI or remote cluster design. The default proposed here is an image-based local sandbox with host mounts, local logs/summaries, and no distributed state.

## Method and limits

The analysis used `gopls` call hierarchy at sandboxd `main`, `runRun` and `assembleBootConfig`, plus references to `sandboxboot.BuildBundleConfig` to verify callers/tests. LSP confirmed the store interface boundary and exposed why following only direct callees misses dependency-injected functions.

[`scope-analysis/analyze.go`](scope-analysis/analyze.go) uses `golang.org/x/tools/go/packages`, `go/types` and Go ASTs to resolve function/method references to definitions and recurse through the graph. It loads all command/internal packages with tests in the default Linux configuration and again with `integration`. Both loads completed with zero package/type errors. Production traversal is restricted to each executable's production import closure; test-only imports are excluded from those closures. Globals/init, inline callbacks, function-valued struct fields and compatible internal interface implementations are represented explicitly. Anonymous function bodies are folded into their enclosing declaration; duplicate test variants are unified by source position.

This is a conservative typed reference analysis, not SSA pointer analysis or a proven complete dynamic call graph. References include registration and values that may never execute. Interface candidates can over-approximate implementations. Function reassignment/dataflow, reflection, callbacks invoked solely through external interfaces (including library-managed handlers), external-module internals and cross-process execution are not comprehensively modeled. Other platforms and special build tags are not analyzed. Imports, source inspection, the separate guest traversal and build/runtime asset review complement the graph. **Absence from a function list is not sufficient evidence to delete code.**

Test mapping follows test helpers up to their production-reference boundary; it does not pretend that a mocked invocation executes the real dependency. Build-selection `integration` means that declaration is absent from the default load, not necessarily that every test in that file boots a VM. No tests or VMs were executed for this report; package/type loading and navigation are the validation performed.

Reproduce from the repository root using existing cached modules:

```sh
GOCACHE=/tmp/vmjail-go-cache GOPROXY=off GOSUMDB=off \
  go run docs/scope-analysis/analyze.go > /tmp/vmjail-graph.json
GOCACHE=/tmp/vmjail-go-cache GOPROXY=off GOSUMDB=off \
  go run docs/scope-analysis/analyze.go integration > /tmp/vmjail-graph-integration.json
python3 docs/scope-analysis/render.py \
  /tmp/vmjail-graph.json /tmp/vmjail-graph-integration.json
```

The offline settings avoid module resolution/network changes during this audit. A fresh environment needs the module dependencies downloaded first. Source line numbers in the inventories refer to the audited revision and will move during the cuts.
