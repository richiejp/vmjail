# darkaifactory — living plan

This file contains current direction, state, and open work. Detailed decisions
and completed slice notes through 2026-07-21 are preserved in
[`PLAN_HISTORY.md`](PLAN_HISTORY.md); consult that archive only for a specific
historical question. Update this file in the same change whenever scope,
status, or an open question changes.

## Project goal

**DO NOT EDIT THIS SECTION**

The purpose of this project: create the harness required for a dark AI factory.

The premise is that fully automated coding-agents will never be trusted therefor in order to run them safely we need to have a strong sandbox.
Meanwhile a sandbox naturally degrades the thing that makes coding agents powerfull. They need access to data and services to do their job.
So we create a sandbox that provides fine-grained and convenient control of the data we move in and out of it.

Secondly a developer running dark AI software factory will not be using a single host. They won't be running automated agents on the host they
physically log into. They will have several large machines, running one or more sandboxes. The AI software factory will be a cluster.
The client machine which the user physically interacts with is ideally not a sandbox-host and they connect over the physical network. This client machine
contains the secrets and code that the user wants to selectively and seamlessly share with fully automated sandboxed agents.

This way the coding-agents are virtually and physically isolated from the system with the user's credentials. Data is duplicated between machines with barriers
where data diffing and inspection can take place.

The sandbox is run in a virtual network where the user has control of DNS and port forwarding. Potentially the agent can run in a shadow universe where its
requests are sent to a fake service where they trigger the VM to be frozen while the user is notified or a third part agent scans the request.

Preliminary Scope:
 - Isolation with VMs and containers
  - Creating Linux KVM VMs with QEMU
  - Creating microvm Linux images with minimal userspace to create containers using an existing container runtime
  - Creating an init system TBD
  - Creating an agent inside the VM to create the container and perform I/O via various virtio interfaces, mount file systems etc.
  - Setting CPU, Strorage and RAM usage limits
  - GPU/PCI passthrough or virtio GPU
  - API for freezing VMs when certain I/O is performed by the sandbox VM
 - Synchronising workspace files and state between the users-host (client), the sandbox host (if different to the user's host) and the sandbox VM
  - Copying or streaming user-host workspace directories to the sandbox host
  - Injecting environment variables into the sandbox VM (specifically the container running in the VM)
  - Intercepting file system requests to allow file streaming, diffing and detect malicious activity
 - Network isolation
  - Putting the VM in a virtual network that may or may not be able to access the host network
  - Forwarding sandbox VM ports to the user host
  - Forwarding user-host ports to the sandbox
  - Allow lists for internet connectivity
  - Bandwidth and connection limits
  - DNS overrides
 - Orchestration UI
  - view clients and hosts, create, delete, start and stop hosts
  - view sandbox-hosts agent logs
  - view sandbox-hosts terminal
  - view and configure various isolations and file synchronisation
  - view what data is being shared with the sandbox
  - authentication for clients
 - Client CLI on user-host
  - share workspace (files, env vars, sanbox config) with the sandbox-host
  - access sandbox-host's terminal
  - forward ports to sanbox-host
  - manage sanbox-hosts in the same way as the orchestration UI
Out of Scope:
  - storing secrets and other state for the sandboxes, always sent from the client on the user-host
  - on sandbox host
    - Integration with cloud providers or K8s
    - Operating Systems other than Linux
    - Old Linux kernels
    - Old versions of QEMU (we may even vendor it)
    - Old Go or GCC/Clang versions
    - Linux kernels without the necessary hypervisor features configured
    - Old hardware
    - Running on Windows or MacOSX unless nested VMs work
    - Making nested VMs work (up to user), we target baremetal for sandbox-host
  - on user-host
    - Windows support
  - Coding agent: The user runs what they want inside the sandbox
  - An API for getting the user's attention; however port forwarding and built in features of agents can be used
  - Shadown services that mock real third party services and allow testing what an agent would do with them

## Current architecture

The trusted user-host runs clients and the overseer. The orchestrator stores
workspace, node, authorization, and AgentRun state. `daft-sandboxd` runs on each
Node and creates image-backed microVMs containing workload containers. Mutual
TLS identities and server-side roles separate operators, clients, and Nodes.

The CPU workspace path works end to end: Node registration, workspace
configuration and file push, image-backed VM launch, PTY execution, port
forwarding, file pull, liveness, and reconciliation. `daft cluster up` provides
a local development cluster.

The in-process egress proxy and DNS path support default-deny networking,
record/replay, and guest CA injection. Host `/proc`/cgroup sampling and guest
eBPF gadgets provide bounded resource and build-progress observations; see
[`OBSERVABILITY.md`](OBSERVABILITY.md).

The orchestrator now has an optional, separate read-only HTTP dashboard
listener. It is intended to bind to loopback or a private interface and sit
behind a trusted private HTTPS route; the mTLS control-plane listener remains unchanged. The dashboard
lists connected and disconnected Nodes and groups active and inactive
Workspaces by their live Sandbox Node or declared target, with unassigned
Workspaces retained in a separate section. Its bounded JSON status document is
available alongside the HTML page and health endpoint.

`nix/orchestrator.nix` exports a parameterised NixOS service module. It contains
no host-specific paths, addresses, identities, or service configuration.

## Active milestone: overseer-controlled agent runs

The target interaction is: “develop `/project/path` in darkaifactory on a
registered Node.” A trusted overseer selects an already deployed, authorized,
healthy Node, applies and pushes a Workspace, starts a sandboxed coding agent
with one-shot OAuth delivery, monitors durable events and bounded
observability, and tears down or reports a stuck run. Node deployment is an
operations concern and is not an implicit part of project development.

Landed:

- Durable operator authorization and revocation.
- Durable AgentRun records and events.
- Workspace apply-by-generation, including atomic create-by-owner-scoped-name,
  concurrent create convergence, and retry-safe `created`/`changed` results.
- Idempotent, per-workspace-serialized launch and stop operations, exposed
  through the trusted MCP facade with bounded workspace results.
- Project-root-confined workspace push through a reusable client API, the CLI,
  and trusted MCP, with bounded declarations/results and explicit host paths.
- Detached agent execution with one-shot OAuth delivery.
- Retry-safe AgentRun creation keyed by a caller-generated request UUID. Exact
  retries converge on the durable run, conflicting reuse is rejected, and
  credentials are not redelivered after the run leaves its awaiting state.
- Trusted stdio MCP facade with owner-scoped workspace and agent operations.
- Devcontainer reliability fixes: overseer workflows push workspace content
  before launch, OCI hard links resolve from the layer archive root, and the
  documented `workspace image --clear` operation is implemented. Image-based
  devcontainers also honor the default `overrideCommand` keepalive (with an
  explicit false opt-out), so lifecycle hooks run even when an image's own
  default command exits immediately. Their `workspaceFolder` becomes the OCI
  and exec working directory, including post-start and detached commands.
  Entrypoint-dependent integration fixtures opt out explicitly, keeping the
  default semantics covered without conflating them with image-command tests.
- The integration gate keeps its concurrency lock in the parent but closes the
  descriptor in test subprocesses, preventing leaked tmux servers from holding
  the repository lock after a timed-out test run. Checkout-specific `.daft`
  state generated by the devcontainer fixtures is ignored.
- Real daemon/MCP lifecycle tests for workspace and AgentRun paths.
- Real-VM MCP restart/resume proof: a fresh `daft-mcp` process retries the
  caller request UUID to the same durable AgentRun and resumes event polling
  from the previously observed cursor through result and teardown.
- Real-VM cancellation proof: MCP stops an indefinitely running Node job,
  repeated cancellation converges, durable state/result become `CANCELED`, and
  terminal reconciliation tears down the exact Sandbox.
- Real-Node-loss proof: terminating the registered sandboxd while its job is
  running leaves MCP/orchestrator available, reports the Node disconnected,
  and durably resolves the affected AgentRun and result as `LOST`.
- Deterministic partial-fleet deployment proof: successful Nodes retain one
  rollback generation, a later Node's failed post-stage validation restores
  its old release, and rolling back only the changed subset reconverges the
  fleet.
- Real multi-Node concurrency proof: one trusted overseer drives two MCP
  sessions, pushes distinct projects to Workspaces pinned to independently
  identified Nodes, overlaps their sandboxed AgentRuns, and verifies each run
  remains bound to its selected Node and returns only its own project result.
- MCP discovery lists use stable lexical cursors, a concise 50-item default,
  a hard 200-item maximum, and explicit `truncated`/`next_cursor`
  continuation. Agent events already use durable numeric cursors and are
  structurally limited to five 16 KiB messages per run; terminal results are
  capped at 1 MiB.
- `sandbox_observer_summary` exposes the existing ownership-checked,
  orchestrator-capped (1 MiB) guest build-progress snapshot as structured MCP
  output. This is the planned progress-get surface as well as the observer
  summary; a second alias would expose identical data without adding a
  capability.
- `sandbox_observer_tail` drains the existing ownership-checked one-shot
  observer stream into a rolling tail buffer on the trusted MCP host. Defaults
  are 64 KiB/50 lines, hard maxima are 256 KiB/200 lines, partial leading
  records are discarded after byte rollover, and only the bounded final
  snapshot enters MCP output. Real-MCP/KVM coverage exercises both
  observability tools against a live AgentRun.
- The future direct-takeover contract is specified below. It deliberately
  reuses the existing authenticated PTY frame transport where possible but
  does not pretend the current detached, output-only AgentRun job is
  attachable.
- Strict-SSH/KVM proof of the full overseer recipe: inspect and checksum the
  static artifact, probe the host, atomically stage and retry the release,
  establish mutual trust, register the Node, survive a daemon restart with its
  identity intact, apply and push a Node-pinned Workspace, start and retry the
  same AgentRun across an MCP restart, resume from its durable event cursor,
  collect its result, and verify teardown. The test uses a temporary `nohup`
  supervisor because it is an isolated fixture and not production deployment
  guidance.

Core runtime implementation is complete. Documentation and agent integration
still need cleanup: the canonical supported workflow belongs in the visible
`docs/usage/agentic-development.md`, while `.agents/skills` may contain only a
thin harness adapter that points to that guide. The current adaptive binary
deployment skill and its stage/rollback scripts encode environment-specific
operations policy and must be removed. The project-development adapter must stop
deploying Nodes implicitly and require an already registered healthy Node.
Durable handoff state should ultimately be product/Ordino state rather than an
ad-hoc file maintained by an agent. Direct interactive takeover remains
intentionally deferred under the contract below.

The Nix build keeps its dependency boundaries explicit: `kconfig-verify` uses
a deliberately narrowed source tree and therefore owns a
separate `buildGoModule` vendor hash rather than sharing the full application
hash. Kernel builds use the fixed-hash upstream release tarball, avoiding both
a remote default-branch lookup and a full Git-history clone.

Host filesystem defaults are being audited by lifetime and ownership. Runtime
Unix sockets prefer a service runtime directory, while configuration and
identity, durable state, and rebuildable caches remain separate and keep their
existing explicit overrides.

## Deferred direct AgentRun takeover contract

Direct terminal/browser takeover remains out of the current milestone. The
existing `WorkspaceCatalogService.AttachSandbox` path already provides an
ownership-checked, mTLS-authenticated bidirectional PTY frame stream with
input, output, resize, and terminal exit propagation through the orchestrator
and Node. It is not an AgentRun takeover primitive: every attach starts a new
arbitrary process, while AgentRuns use the separate detached Job protocol,
which is non-PTY and output-only after its Hello frame. Closing that job stream
is cancellation, so it cannot be upgraded safely into an interactive session.

A future implementation must satisfy all of the following:

- Launch an explicitly takeover-capable AgentProfile under a retained PTY (or
  equivalent terminal multiplexer) from the beginning. The durable AgentRun
  records its terminal-session identifier; no API may claim an existing
  output-only job can become interactive.
- Address takeover by AgentRun UUID. The orchestrator must resolve and verify
  the durable owner, Workspace UUID, current Sandbox UID, Node fingerprint,
  and Node job/session identifier as one binding before opening a stream.
  Terminal, replaced-Sandbox, wrong-Node, revoked-owner, and stale-session
  requests fail closed.
- Keep terminal traffic out of MCP tool results and model context. MCP may
  expose bounded status or mint a short-lived, single-purpose attachment
  capability, but terminal bytes use a dedicated bidirectional RPC or web
  transport derived from the existing `AttachFrame` semantics.
- Separate detach from cancel. Browser/terminal disconnect leaves the
  AgentRun and retained PTY alive for a bounded reconnect window; explicit
  cancellation continues through `agent_cancel`. Natural process exit writes
  the same durable terminal state/result exactly once.
- Define one input-writer lease with expiry and explicit transfer/revocation.
  Optional additional viewers are read-only. Enforce bounded replay,
  monotonically increasing output offsets, bounded per-client queues, and
  slow-consumer disconnection so reconnect and fan-out cannot grow memory
  without limit or duplicate user input.
- Preserve resize and exit frames, propagate half-close/cancellation
  correctly, and audit attach, detach, writer-transfer, and cancel decisions
  without recording terminal content or OAuth credentials.
- A browser gateway terminates an authenticated UI session at the
  orchestrator, validates Origin/CSRF, exchanges only a short-lived scoped
  capability, and relays WebSocket frames to the internal attach RPC. It must
  never expose the operator certificate, AgentProfile credential catalog, or
  injected OAuth payload to browser storage.

Acceptance requires unit protocol/state-machine tests plus a real-VM test that
starts a takeover-capable AgentRun, observes output, sends input and resize,
disconnects and resumes from an output offset, rejects a second writer,
survives overseer/UI reconnect, and resolves cancellation, natural exit, Node
loss, and stale Sandbox bindings deterministically.

## Active milestone: XFS workspace disks and recoverability-aware CAS cleanup

Writable workspace virtiofs shares will be replaced by one retained
XFS-on-qcow2 disk per Sandbox. OCI rootfs and read-only shares remain on
virtiofs. The default virtual allowance is 20 GiB; the node CAS hard limit is
100 GiB, with cleanup beginning at 85% and targeting 75%.

Landed:

- A deterministic internal qcow2 v3 encoder creates empty sparse workspace
  images without adding a host utility, preserving single-static-executable
  Node deployment. It validates sector alignment and the format layout's
  maximum addressable size and initializes all metadata refcounts.
- Sandbox VMM configuration accepts one qcow2 workspace disk with a stable
  device ID and requires an absolute Cloud Hypervisor API socket alongside it.
  A bounded internal HTTP-over-Unix client implements pause, resume, and the
  pinned `/vm.resize-disk` request used by the later freeze/live-grow path.
- A durable Node-side workspace-disk store atomically creates or converges on
  one retained image per Workspace, refuses implicit size changes, and performs
  a marker-backed first-start deletion of legacy `mounts/*` trees without
  import. `disk_bytes` now flows generation-safely through Workspace apply,
  `workspace resources --disk`, Sandbox dispatch, and trusted MCP; zero selects
  the fixed 20 GiB default. Production dispatch attaches the retained image and
  a unique VMM API socket while test-only executors remain explicitly diskless.
- The guest kernel now includes built-in XFS, quota, and ACL support, and the
  self-contained initramfs carries relocated `mkfs.xfs`, `xfs_repair`, and
  `xfs_growfs` binaries with their exact libraries. Writable Workspace mounts
  are delivered as read-only first-launch seed shares, copied into a newly
  formatted reflink-enabled XFS filesystem, and bind-mounted into the OCI
  container with the seed root's ownership and mode; retained XFS is recovered
  and reused without reseeding. OCI rootfs, declared read-only mounts, and
  explicit absolute host-control mounts (such as build outputs) remain on
  virtiofs. Both local and embedded initramfs builds relocate the XFS tools and
  their transitive libraries to guest-local `/lib` paths.
- A bounded guest snapshot service streams an allowlisted XFS mount directory
  over the VM's hybrid-vsock socket. Live pulls refresh the Node's host staging
  tree before transactionally building its CAS manifest; the real-KVM
  push/mutate/pull test proves guest writes round-trip from XFS to the client.
- Retained qcow2 images can now grow in place with crash-safe header ordering;
  shrinking is rejected so a resource edit cannot discard protected data.
- The bounded guest workspace service now reports XFS total/used/available
  bytes and runs `xfs_growfs` on request alongside snapshot streaming. It uses
  a dedicated vsock port, avoiding the credential-delivery listener.
- Running workspace disks are tracked on the Node with coherent usage,
  allowance, and frozen state. A bounded poller pauses Cloud Hypervisor at 90%
  usage; live growth durably expands qcow2, resizes virtio-blk, grows XFS, and
  keeps an already-frozen VM paused until a separate validated unfreeze.
- The retained-disk store reserves every qcow2 virtual maximum against the
  durable filesystem capacity observed at Node startup, rejecting new disks
  and live growth that cannot fit. Under host pressure, the bounded disk
  monitor freezes actively growing sandboxes by recent growth rate, breaking
  ties largest-first, while leaving idle sandboxes available.
- `workspace resources --disk` now grows an existing retained disk through an
  authenticated exact-manifest Node RPC before committing the catalog change.
  Running disks execute the qcow2, virtio-blk, and guest `xfs_growfs` sequence;
  stopped disks grow durably in place, and every shrink is rejected.
- A per-CAS Badger index durably records chunk size/access/provenance,
  exact manifest digest/generation references, recoverability, disposition,
  and crash-recoverable deletion queue state. Legacy unindexed chunks migrate
  protected. Exact adoption, rejection with prior-manifest restoration,
  workspace deletion, shared-chunk protection, batched touches, revalidation
  before unlink, and the 50/35/15 cleanup ordering are deterministic and
  tested against the 85/75/100 GiB and 180-day policy.
- The production Node workspace service now publishes manifests and chunks
  transactionally through that index. Incomplete client pushes remain
  invisible, live guest snapshots are protected, successful pulls acknowledge
  only their exact digest/generation, and current manifests restore after a
  restart. A durable five-minute worker performs expiry and pressure cleanup;
  synchronous hard-limit admission reserves pending bytes, reclaims eligible
  recoverable chunks, marks affected manifests `NeedsRepush`, and refuses a
  write when protected data prevents enough reclamation.
- The brokered Node workspace data plane exposes exact-generation manifest
  rejection/deletion plus bounded CAS status and dry-run prune RPCs. The CLI
  now provides `workspace changes reject` and `workspace storage status|prune`;
  a brokered end-to-end test proves rejection restores the prior adopted
  manifest and stale operations fail without weakening protection.
- Brokered exact-manifest-set disk lifecycle controls now expose running and
  retained disk status, explicit unfreeze, durable stopped-qcow2 deletion, CAS
  protection release, and generation-checked catalog deletion. The CLI exposes
  `workspace unfreeze` and `workspace delete`; tests pin stale/extra manifests,
  pending reservations, locked/running workspaces, unreachable nodes, malformed
  bindings, controller/index failures, and never-launched deletion.
- Exact rejection now removes the stopped retained qcow2 before restoring CAS
  provenance, refuses a running disk, and leaves the protected manifest current
  when disk deletion fails. Relaunch therefore seeds only the restored client
  manifest instead of recovering rejected XFS bytes.
- The reusable authenticated client now exposes bounded Workspace disk/CAS
  status plus exact-manifest rejection, unfreeze, and Node-first deletion over
  the broker. This is the shared non-Cobra control path used by trusted API
  surfaces; its target, stale-state, transport, and partial-failure behavior is
  covered with in-process Connect peers.
- Trusted MCP now exposes bounded `workspace_storage_status`, exact
  `workspace_changes_reject`, `workspace_unfreeze`, and `workspace_delete`
  tools with closed-world schemas and accurate read/destructive/idempotent
  annotations. They delegate to the same authenticated client controls rather
  than weakening generation checks or duplicating transport policy.
- Node disk status now carries exact current manifests and deduplicated
  per-Workspace protected bytes alongside usage, allowance, and frozen/running
  state. CLI and trusted MCP project that live status with bounded global CAS
  accounting. The catalog status schema/model includes the corresponding disk,
  protected-byte, manifest digest/generation fields and the `StorageFrozen`
  condition type.
- Authenticated Node heartbeats now carry bounded live workspace-disk reports.
  The orchestrator persists usage, allowance, protected bytes, non-regressing
  manifest identity, and `StorageFrozen` only when the reporting Node still
  owns the Workspace's current Sandbox binding; stale or foreign reports are
  ignored.
- Explicit stop and process-exit teardown snapshot every writable guest XFS
  mapping into a protected durable manifest before unregistering it. The
  real-KVM push/mutate/stop/pull acceptance path now also asserts the writable
  mount is XFS and supports reflink cloning.
- The real-KVM XFS round trip now grows a running 512 MiB disk to 1 GiB through
  `workspace resources --disk`, verifies the mounted filesystem expanded, then
  stops and relaunches to prove the retained disk preserves guest mutations. It
  also verifies the XFS/qcow2 allowance returns `ENOSPC`, observes the Node
  freeze the resulting greater-than-90%-full disk, grows the frozen disk to 2
  GiB, verifies it remains paused, and explicitly unfreezes it before resuming
  guest work.
- First-launch workspace contents now stream directly from durable manifests
  and CAS chunks into guest XFS over bounded per-VM vsock archive endpoints.
  Writable Workspace paths no longer create virtiofs shares or host seed
  trees; retained qcow2 images receive no seed endpoint and therefore cannot
  be overwritten during relaunch. The real-KVM round trip exercises this path.
- Devcontainer launch metadata for writable XFS mounts is read as one bounded
  regular file directly from the durable manifest/CAS, so image-based projects
  do not recreate a host workspace tree. Build-based devcontainers materialise
  only an ephemeral build context and remove it after image assembly.
- Exact pull acknowledgment now removes a stopped retained qcow2 after the CAS
  generation becomes recoverable. A running disk is retained because it may
  already contain writes newer than the acknowledged manifest.
- The real-KVM stop/pull acceptance path asserts the stopped qcow2 exists until
  exact adoption, then verifies both the image and per-Sandbox snapshot staging
  tree disappear without leaking retained protection.
- A live compatibility run against the sibling `localaisrl.com` web app used a
  fresh 512 MiB allowance on the local Node: `/workspace` mounted from
  `/dev/vda` as quota-enabled XFS, reflink cloning succeeded, all nine Bun unit
  tests and the production build passed, and an orchestrator port forward
  returned the rendered site with HTTP 200. Stop made the exact snapshot
  protected; generation-checked deletion then removed the catalog binding,
  qcow2, staging tree, VMM/API socket, cgroup, and reservation without touching
  tracked app files. The app's optional Chromium GPU budget requires browser
  packages and `/dev/shm` that its minimal Alpine devcontainer does not declare;
  that image prerequisite is independent of workspace-disk semantics.

Remaining:

- None for this milestone. Deterministic manifest-control and retained-store
  tests cover rejection/deletion cleanup and failure ordering; real-KVM and the
  live app run cover disk/VMM behavior and teardown leaks.

Protected sandbox writes must never be cleanup candidates until the client
acknowledges the exact manifest digest and generation, rejects that generation,
or deletes the workspace. A stale acknowledgement cannot adopt newer writes.

## Other open product work

- Orchestrator-hosted ingress and complete per-workspace routing policy.
- GPU discovery, scheduling, passthrough, and shared-VM lifecycle.
- Workspace adoption and per-client mount mappings.
- Freeze-on-disconnect semantics.
- Optional Docker API compatibility.
- Cache pinning, correctness, invalidation, and hosted cassette completion.
- DNS and non-HTTP replay policy where real workloads demonstrate a need.

## Engineering efficiency milestone (2026-07-21)

Long agent sessions consumed excessive cached-input tokens. Local session
records showed broad source reads and repeated transcript replay dominated;
observability-specific output was a small fraction of tool output. The project
now treats bounded diagnostic output and targeted code navigation as
maintainability requirements.

- Use `gopls` symbols, workspace-symbol, definition, references, call hierarchy,
  and diagnostics before reading Go files linearly.
- Use `scripts/go-nav.sh` for bounded command-line LSP results. `gopls mcp` is
  available when an agent runtime can register a repository-local MCP server.
- Search for a symbol first, then read the smallest useful region (normally no
  more than 200 lines). Do not dump whole files, generated protobufs, or broad
  directory matches into an agent conversation.
- Successful checks should produce summaries. Store verbose output outside the
  conversation and return only bounded failure excerpts.
- Subagents receive a compact task brief and relevant paths, not the complete
  parent transcript. Long milestones end with a concise handoff into a fresh
  session.
- The repository-local `daft_developer` Codex agent owns the daft MCP schemas
  and raw tool traffic, returning only bounded results to the overseer. Its MCP
  tool set follows the server's advertised surface instead of duplicating an
  allowlist that would drift as tools are added.
- MCP and observability APIs must paginate or cap potentially unbounded output.
- Keep Nix derivation source closures narrow. In particular, the kernel's
  `kconfig-verify` dependency includes only its command/package and module
  metadata, while general Go packages exclude agent skills and this plan, so
  unrelated plan or skill edits do not rebuild Linux or production Go images.

## Open design questions

- What persistence boundary should follow the existing Badger-backed operator
  and AgentRun stores for the rest of the control plane?
- How should cassette keys include image, lockfile, request set, and policy so
  stale replay cannot silently pass?
- Which terminal transport can support future human takeover without widening
  the trusted MCP authority?

## Updating this plan

Keep this document concise and current. Add decisions and active work here;
move superseded explanations and completed implementation diaries to
`PLAN_HISTORY.md`. Do not append command transcripts, test logs, or per-commit
narratives. The current plan should remain readable in one targeted pass.
