# Conservative file deletion report

The first deletion target is dedicated web, cluster, remote catalog/broker and workspace CAS code. Keep the microVM, virtiofsd, guest container runtime, direct-mount assembly and both observation tiers. This report deliberately excludes mixed launchers, protocols and potentially reusable integration tests from the deletion manifest.

**Every listed file is a conditional deletion candidate, not an instruction to unlink it immediately.** The current commands still reference these components. There is no claim that removing this list without accompanying edits leaves a compiling or working project. All 112 listed files have since been deleted at the user's request. See [deletion results and remaining errors](deletion-results.md); the repository currently has compilation failures.

The audit uses the copied `d785afbedca3` source, the earlier [scope analysis](scope-reduction.md), its typed Go reference/test inventories, current tracked filenames, source inspection and a further import/reference scan. The exact manifest is [deletion-candidates.tsv](deletion-candidates.tsv). Paths below are individual tracked files, not wildcard deletion rules.

## Summary


| Group | Files | When deletion is appropriate |
| --- | ---: | --- |
| UI: Web interface and its tooling | 23 | Remove dashboard registration, flags and asset references; adapt non-dashboard tests that use dashboard helpers. |
| CLUSTER: Cluster executable and deployment shell | 14 | Replace CLI cluster discovery and remote command wiring; remove build/release/Nix references. Preserve sandboxd process setup. |
| CONTROL: Distributed state and rendezvous | 21 | Remove remote catalog, broker and reconciliation callers from the retained launcher/client; port any useful local behavior first. |
| CAS: Workspace chunk storage and replication | 36 | Switch all retained workspace mounts to direct host shares; remove snapshot/build/transfer consumers; preserve IO observation independently. |
| ORCH: Remaining orchestrator production code | 18 | Extract or replace useful observation/exec/forwarding behavior first; retire the server and its callers together. Non-dashboard orchestrator tests are held for migration. |

Total: **112 exact files**. This is a deliberately incomplete first removal set; files omitted from it are held, not judged permanently necessary. The sections below contain the complete same list.

## Conditions that make this conservative

1. Preserve a working local image + direct-directory-mount launch before removing its remote alternatives. `sandboxdcli/run.go` already contains direct mounts but also CAS branches, so that whole file is held.
2. Extract `sandboxexec.applyObserveSummary` and observer-log defaults, and retain guest gadget configuration/export. Keep local summary rendering and rotating-log tailing before removing remote observer routes.
3. Keep modern bundle cloning and non-root ownership translation from `sandboxboot`; the old debug path alone is not an adequate replacement. Preserve cgroup and user-namespace preparation from sandboxd's entry point.
4. Port useful observation, interactive-exec and forwarding assertions before deleting their old harness. Tests that assert only an intentionally removed feature can be deleted with that feature.
5. Edit command registration, imports, flags, Makefile/Nix/release references and affected tests in the same changes as deletions. Do not treat a file list as an independent patch.
6. Keep the current Go module name during these cuts. Tidy dependencies and rename after the retained system builds and its checks pass.

The manifest's final column lists referring Go files outside the manifest, using package imports and the previous typed reference graph. These are review obligations, including same-package references and tests. It is not a complete dependency proof: type/constant references, string-based subprocess launch, embedded assets, build scripts and reflection require separate checks. An empty cell does not establish that deletion is immediately safe.

## Files held back

This section records the initial conservative decision. The user subsequently
authorized removing documentation and tests of deleted components; those
follow-up removals are recorded in [deletion results](deletion-results.md).

| Files or family | Why they are excluded from deletion |
| --- | --- |
| `cmd/daft-sandboxd/main.go`, its tests; all `internal/sandboxdcli/` | Startup delegation/reexec, local image operations and direct mounting are mixed with remote commands. Edit rather than delete wholesale. |
| `cmd/daft/main.go`, most `internal/cli/` | Retain command shell and selected rendering/terminal behavior. Only dedicated cluster files are listed below. |
| `internal/cli/sandbox_observe.go`, `sandbox_observer_tail.go`, `workspace_exec.go`, `workspace_exec_terminal_unix.go`, `workspace_forward.go`, and their tests | Observation display, terminal setup, diagnostics and forwarding may be reused locally. Replace transport dependencies first. |
| All `internal/sandbox/`, `internal/sandboxboot/`, `internal/sandboxexec/` | VM lifecycle, cleanup, limits, ownership, mount and guest-observation setup. These contain code to extract, not whole-file deletion candidates yet. |
| All `internal/agent/`, `cmd/daft-agent/` | This is the in-VM runtime helper. Do not confuse it with distributed AI-agent management. Narrow disk/snapshot/credential handlers later. |
| `internal/observability/`, including C BPF sources, generated Go, ELF objects and tests; `internal/observertail/` | Required host/guest observability. Removing CAS does not require removing these. |
| `internal/imagestore/`, `internal/bundled/`, `internal/userns/`, `internal/cgroupwrap/`, `internal/cloudhypervisor/` | OCI image cache, embedded runtime artifacts, namespace/cgroup setup and VMM control. OCI layer storage is distinct from the workspace CAS slated for removal. |
| `internal/sandboxnet/`, `sandboxattach/`, `sandboxjob/`, `sandboxgateway/`, `nodejob/`, `nodectrl/`, `nodectrlclient/` | Some are predominantly distributed, but local execution, streaming, resource detection and related tests warrant a separate file review before removal. This report intentionally postpones them. |
| `internal/trace/`, `component/`, all `proto/` and `internal/genpb/` | Guest control/tracing remain. Remote schemas currently also supply types to retained code; do not delete generated files independently of their schemas and consumers. |
| `internal/identity/`, `mtls/`, `clientconfig/`, `daftclient/`, `ociregistry/` | Remote features can eventually be removed, but live imports remain in launchers, trace/client paths or image pulling. Wait for consumer cleanup. |
| `internal/devcontainer/`, `devcontainerspec/`, `buildimage/`, `workspacedisk/`, `workspacesnapshot/` | Build/private-disk choices are not settled. They also depend on some listed deletion candidates, so either retire them in a later reviewed set or replace their dependencies before completing those deletions. |
| `internal/agentprofile/`, `agentruncatalog/`, `mcpserver/`, `agentcredential/`, `sandboxsecret/` and related commands | Likely future cuts, but this conservative pass does not decide all AI-workflow/credential features or their shared config types. |
| Proxy/cache packages and commands | Local networking policy is still a product choice. Removing remote orchestration alone does not justify deleting local egress isolation or instrumentation. |
| Non-dashboard `internal/orchestrator/*_test.go`; all `internal/testdaemons/`; non-UI blackbox fixtures | Preserve as migration references until local observation, exec, network and mount assertions have been ported. They may stop compiling when old production code is deleted; port/retire them in that same change, not by leaving broken tests behind. |
| `nix/node.nix` | Contains useful `Delegate = true`, service filesystem and runtime-directory setup, despite launching a distributed node. Extract relevant setup before retiring this module. |
| `flake.nix`, `flake.lock`, `Makefile`, `.github/workflows/`, release/check scripts | Edit outputs and checks, not whole-file deletion. Nix still imports the listed orchestrator module. |
| `third_party/`, `.gitmodules`, `third_party/submodule-revs.json`, kernel configuration | Keep VMM, virtiofsd, crun, initramfs and kernel/BTF dependencies. Possible unused source checkouts need a separate build audit. |
| `LICENSE`, general contributor docs, `PLAN.md`, current architecture/observation docs, analysis reports, quality tools | Preserve attribution and working guidance; update stale architecture rather than deleting the whole documentation/tooling set. |

## Required edits outside the manifest

- `internal/cli/cli.go` registers `newClusterCmd`; remove that registration with the cluster command files. Other CLI helpers use `discoverLocalCluster` and `resolveIdentityDir` from `clientcluster.go`; replace those uses before removing it.
- `internal/orchestratorcli/cli.go` currently configures the dashboard. If removing the UI first, edit those calls while retaining the rest of the orchestrator until its later cut. Shared dashboard test helpers must be removed/replaced in remaining tests.
- `internal/sandboxdcli/run.go` and `serve.go` use workspace/broker/CAS services. Keep the direct mount branch, and extract the local pieces from daemon setup before deleting their imports.
- `internal/cli/workspace*.go`, `internal/daftclient/`, `internal/devcontainerspec/` and `internal/sandboxexec/devcontainer.go` contain consumers of the remote catalog/store or transfer layers. They must be rewritten or separately retired before the corresponding candidate groups can be deleted.
- `internal/imagestore/pull.go` still imports hosted-registry logic; that is why the registry package is held even though the orchestrator executable is listed.
- `flake.nix` imports `nix/orchestrator.nix` for modules/checks and builds distributed executables. Makefile and release/CI scripts also reference old outputs. Remove those entries while retaining VM/kernel/virtiofsd/crun/initramfs and BPF builds.
- Remove browser-driver invocation and terminal-assets update instructions from surviving blackbox/build documentation. Preserve shell/Go test fixtures that exercise retained local behavior.

## Exact candidate files

The groups are logical deletion units, not a strict execution order. In particular, CAS and distributed-state packages may remain until their last consumer has been removed. Some candidate groups refer to one another; the manifest only highlights referring Go files outside the entire candidate set.


### UI Web interface and its tooling

Dedicated dashboard HTML/HTTP/auth/terminal code and frontend tooling. Deleting the xterm LICENSE is appropriate only with removal of all copied xterm assets; retain the root project LICENSE.

```text
blackbox/browser-driver.js
internal/orchestrator/dashboard.go
internal/orchestrator/dashboard_assets.go
internal/orchestrator/dashboard_auth.go
internal/orchestrator/dashboard_auth_test.go
internal/orchestrator/dashboard_detail.go
internal/orchestrator/dashboard_security.go
internal/orchestrator/dashboard_terminal.go
internal/orchestrator/dashboard_terminal_test.go
internal/orchestrator/dashboard_test.go
internal/orchestrator/dashboard_webauthn.go
internal/orchestrator/dashboard_webauthn_test.go
internal/orchestrator/web/terminal.js
internal/orchestrator/web/thirdparty/xterm/LICENSE
internal/orchestrator/web/thirdparty/xterm/xterm.css
internal/orchestrator/web/thirdparty/xterm/xterm.js
internal/orchestratorcli/dashboard.go
internal/orchestratorcli/dashboard_test.go
scripts/update-terminal-assets.sh
web/terminal/README.md
web/terminal/bun.lock
web/terminal/bunfig.toml
web/terminal/package.json
```

### CLUSTER Cluster executable and deployment shell

Dedicated orchestrator executable, its command layer, local-cluster supervisor/discovery, cluster-only tests and orchestrator service module. Localcluster starts multiple distributed components even on one host; it is not the proposed local sandbox.

```text
cmd/daft-orchestrator/main.go
cmd/daft-orchestrator/main_test.go
internal/cli/clientcluster.go
internal/cli/clientcluster_test.go
internal/cli/cluster.go
internal/cli/cluster_test.go
internal/localcluster/discover_test.go
internal/localcluster/localcluster.go
internal/localcluster/localcluster_test.go
internal/orchestratorcli/auth.go
internal/orchestratorcli/auth_test.go
internal/orchestratorcli/cli.go
internal/orchestratorcli/cli_test.go
nix/orchestrator.nix
```

### CONTROL Distributed state and rendezvous

Remote durable catalogs, reconciliation and workspace rendezvous/relay. Included tests belong to these retired features; inspect the test association inventory before removing any helper shared with held tests.

```text
internal/brokerclient/attach.go
internal/brokerclient/client.go
internal/brokerclient/client_test.go
internal/brokerclient/serve.go
internal/brokerclient/serve_attach_test.go
internal/brokerclient/serve_sandbox_test.go
internal/controlstore/store.go
internal/controlstore/store_test.go
internal/reconciler/reconciler.go
internal/reconciler/reconciler_test.go
internal/workspacebroker/conn.go
internal/workspacebroker/conn_test.go
internal/workspacebroker/matcher.go
internal/workspacebroker/matcher_test.go
internal/workspacebroker/sessions.go
internal/workspacebroker/sessions_test.go
internal/workspacecatalog/catalog.go
internal/workspacecatalog/catalog_test.go
internal/workspacecatalog/persistence.go
internal/workspacecatalog/persistence_test.go
internal/workspacecatalog/watch_test.go
```

### CAS Workspace chunk storage and replication

Workspace chunk storage/index, lazy CAS-backed FUSE, manifest/archive/sync and transfer wrappers. Removing daftfuse also removes its chunk-read Policy.OnRead hook. Preserve guest IO summaries separately; do not claim those provide equivalent interception or a complete filesystem audit.

```text
internal/cas/cas.go
internal/cas/cas_test.go
internal/casindex/index.go
internal/casindex/index_test.go
internal/casindex/model.go
internal/casindex/policy.go
internal/casindex/policy_test.go
internal/casindex/worker.go
internal/casindex/worker_test.go
internal/daftfuse/daftfuse.go
internal/daftfuse/daftfuse_test.go
internal/daftfuse/fetcher.go
internal/daftfuse/fetcher_test.go
internal/daftfuse/fuse_internal_linux_test.go
internal/daftfuse/fuse_linux.go
internal/daftfuse/fuse_linux_test.go
internal/daftfuse/fuse_nonlinux.go
internal/daftfuse/tree.go
internal/workspace/apply.go
internal/workspace/apply_test.go
internal/workspace/archive.go
internal/workspace/archive_test.go
internal/workspace/chunker.go
internal/workspace/diff.go
internal/workspace/diff_test.go
internal/workspace/errors.go
internal/workspace/fastcdc.go
internal/workspace/fastcdc_test.go
internal/workspace/manifest.go
internal/workspace/server.go
internal/workspace/server_test.go
internal/workspace/sync.go
internal/workspace/sync_test.go
internal/workspace/workspace_test.go
internal/workspacesync/push.go
internal/workspacesync/push_test.go
```

### ORCH Remaining orchestrator production code

Dedicated orchestrator production files remaining after the UI group. Observation handlers are remote relays, but read bounds and stream lifecycle behavior may be worth transferring. Their non-dashboard tests are intentionally held outside this list until migration is complete.

```text
internal/orchestrator/agentrun.go
internal/orchestrator/attach.go
internal/orchestrator/attachsandbox.go
internal/orchestrator/broker.go
internal/orchestrator/forwardport.go
internal/orchestrator/health.go
internal/orchestrator/job.go
internal/orchestrator/nodectrl.go
internal/orchestrator/observe_summary.go
internal/orchestrator/observer.go
internal/orchestrator/openport.go
internal/orchestrator/operator.go
internal/orchestrator/peerctx.go
internal/orchestrator/secret.go
internal/orchestrator/secretdelivery.go
internal/orchestrator/server.go
internal/orchestrator/trace.go
internal/orchestrator/workspacecatalog.go
```

## Verification when implementing

For each deletion unit, check remaining references and Go package loading, then run the affected retained unit tests. Test the local launcher with a real VM before removing the final old boot/observation wiring: direct host read/write, a non-root container, a command longer than 30 seconds, cancellation/cleanup, host observer output and guest IO summaries. Port the existing focused tests rather than discarding behavior simply because its test currently uses a cluster harness.

This report itself was checked for tracked-file existence, duplicate entries, agreement between the Markdown list and TSV, and exclusion of the protected VM/guest/observation paths. Those checks describe the original pre-deletion audit. Subsequent deletions, compile checks and unit-test results are recorded in [deletion results](deletion-results.md). The earlier analysis successfully type-loaded default and integration-tag packages; that establishes the baseline, not the state after deletion.
