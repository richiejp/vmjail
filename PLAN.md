# darkaifactory — PLAN

This is the living plan for the project. It MUST be kept up to date as work
progresses. Whenever scope, direction, milestones, or open questions change,
update this file in the same change that introduces the change.

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

## References

- Prio art: ../Ayup
- Project layout: ../behtree
- Project layout 2: ../VoxInput

## Direction (2026-05-12) — workflows and reorientation

The Project goal (above) is the *what*; this section is the *how* —
two concrete workflows that drive every product decision. Existing
primitives (broker, daft-fuse, overlayfs, mTLS, registry, CAS,
FastCDC, tracing) stay. The control surface above them changes
shape: sandboxd becomes a daemon, the orchestrator owns workspace
state, and the client drives everything from the user-host.

### Two reference workflows

Both are described from the **client on the user-host**. The
orchestrator and sandboxd daemons run on their own schedule and
authenticate separately — the user never has to set them up to use
them, only the client.

**CPU workflow (1 VM per workspace).** A git repo for a web app
on the user's laptop. They want claude-code + the dev server in the
same VM, the dev server reachable in a browser.

```sh
cd ~/src/myapp
daft workspace create                    # default name = basename(pwd)
daft workspace target <sandbox-host>     # CPU; one VM per workspace
daft workspace image ghcr.io/me/claude-dev:latest
daft workspace mount add ~/.config/git --readonly
daft workspace mount add ~/.claude       --readonly
daft workspace mount add .                # cwd, default rw push+pull
daft workspace env add GH_TOKEN --from-env
daft workspace env add NODE_ENV=development
daft workspace port add 3000 --tls --listen orchestrator,user-host
daft workspace launch \
    --terminal --forward-ports --stream-files
# attached; ^C stops the VM (unless --detach)
```

Inside the VM: claude-code is the container PID 1 (or the entrypoint
the image declares), it runs `npm run dev`. The user views the app
at `https://<workspace>.<orchestrator>/` or `https://localhost:3000`
(forwarded). Files in `.` stream on first read; writes land in the
overlay; the user explicitly `daft workspace pull` to surface them
back.

**GPU workflow (N workspaces share one VM).** A GPU-bound workload —
fine-tune, batch inference, training. The orchestrator co-locates
workspaces on a single GPU-bound VM as sibling containers so VM-boot
and model-load costs amortize across jobs.

```sh
daft workspace create
daft workspace target <sandbox-host> --gpu rtx-4090
daft workspace image ghcr.io/me/trainer:latest
daft workspace mount add .
daft workspace launch --terminal
```

Default `--isolation shared` on `--gpu …`; `--isolation strict`
forces 1:1 (full VM-per-task, slow first boot, no sibling exposure).
The GPU-bound VM survives idle for $TTL between workspaces.

### Architectural implications

1. **Sandboxd is a daemon.** `daft-sandboxd serve --orchestrator
   <url>` opens a persistent outbound control channel (reusing the
   broker's outbound-mTLS plumbing), authenticates once, advertises
   its resources (cores, RAM, GPUs by BDF, image cache size, CAS
   size), and waits for work. The orchestrator pushes `RunSandbox`,
   `RunContainer`, `Freeze`, `Thaw`, `Stop` over the control
   channel. No workspaces, images, or paths on the sandboxd CLI.
   The current `daft-sandboxd run …` form survives as
   `daft-sandboxd debug run` for one-shot local testing.

2. **Orchestrator owns workspace state.** A `Workspace` is a
   persistent first-class object stored on the orchestrator: name,
   UUID, owner fingerprint, mounts[], env_vars[], image_ref or
   pending-build, ports[], target_sandbox_host, gpu_request,
   isolation, lock_owner_fp, lock_expires_at, status. Clients
   mutate it incrementally via `WorkspaceService.*`. The
   sandbox-host receives a one-shot `SandboxSpec` derived from
   workspace state at launch time; it stores nothing across runs.

3. **Client-driven incremental assembly.** Each `daft workspace …`
   command returns the workspace's current readiness state
   (`status`, `missing[]`) so the user sees what's left before
   `launch` will accept the workspace. `launch` refuses when
   `missing[]` is non-empty, with an instructive message naming
   each gap. Sensible defaults (cwd as a mount, `~/.config/git`
   and `~/.claude` offered with `--with-dev-defaults`, workspace
   name = `basename($PWD)`) keep the common path one command.

4. **Port forwarding through the orchestrator.** Containers expose
   ports back over the existing broker channel. The orchestrator
   can:
   - serve them on its own LAN/VPN interface under
     `https://<workspace>.<orchestrator>/`, optionally terminating
     TLS with an auto-issued cert (so the workspace dev server
     doesn't need TLS at all),
   - tunnel them to a localhost listener on the user-host
     (`--forward-ports`).
   No direct sandbox-host network exposure required; mTLS continues
   to terminate at the orchestrator on the control side, and the
   port-forward tunnels run inside that mTLS.

5. **Workspace adoption.** A workspace is launched from one
   user-host but isn't bound to it once data has synced. Any
   user-host approved by the orchestrator can:
   ```sh
   daft workspace adopt <name-or-uuid>
   # CLI walks each mount, prompts for a local mapping or --skip
   daft workspace mount remap </src/a> </local/a>
   daft workspace mount flag </local/a> --pull-only
   ```
   Per-user-host overrides (skip / pull-only / push-only / remap)
   live alongside the global workspace spec; the global spec is
   the source of truth for "what's mounted into the VM".

6. **Workspace locks.** State mutations and pushes require a write
   lock, held by one client at a time. `daft workspace lock` and
   `--force` are explicit. Locks auto-release on heartbeat timeout
   (default 60 s after the client's control channel drops).

7. **Freeze-on-disconnect.** With `--stream-files`, daft-fuse
   reaches the user-host (over the broker) on chunk misses. If the
   user-host control channel disconnects and a chunk miss happens
   on the sandbox-host, the controller QMP `stop`s the VM; on
   reconnect the VM thaws and the read resumes. The
   already-scoped Slice 4c is the policy-driven half of this; the
   disconnect-triggered half is Phase G below.

8. **Dockerfile build in VM (Ayup-style).** If the workspace
   declares a Dockerfile instead of an image ref, the
   orchestrator schedules a transient buildkit-VM on a
   sandbox-host (taking the Dockerfile context as a workspace),
   pushes the resulting image into the orchestrator's embedded
   registry, and rewrites the workspace's `image` field. No
   host-side docker required.

9. **Two-way sync is delegated, not built.** The workspace mount
   model is intentionally asymmetric (push, pull, or
   stream-on-miss; writes surface back only on explicit `daft
   workspace pull`). For users who want true bidi sync we expose
   a Docker-API-compatible endpoint per workspace (Phase H) and
   point them at their existing tooling — Mutagen primarily,
   but also VS Code Dev Containers, JetBrains Gateway, plain
   `docker cp`. The user's tool drops its agent into the
   container and runs whatever sync mode it wants. This keeps
   the daft-side filesystem story narrow and lets the
   ecosystem's existing sync UX serve the case where bidi is
   genuinely desired.

### Slice plan

Phase A — **Sandboxd daemon mode**:
- A1 (landed): `daft-sandboxd serve --orchestrator <url>` opens
  an outbound mTLS control channel over a new
  `SandboxControlService` (`proto/sandboxctrl/v1`), issues
  `RegisterSandbox` with a CPU + RAM advertisement, and emits
  `Heartbeat` pings on a 5-second ticker until SIGTERM. The
  orchestrator-side registry (`internal/sandboxctrl.Registry`)
  tracks live registrations keyed by mTLS fingerprint, refreshes
  `LastSeenAt` on every heartbeat, and releases the entry on
  stream close. `ListSandboxes` exposure to clients is deferred
  to A2.
- A2 (landed): `SandboxControlService.ListSandboxes` returns a
  snapshot of the registry (fingerprint, version, resources,
  RegisteredAt, LastSeenAt). Surfaced to operators as
  `daft sandbox list --orchestrator <url>`. Read-only; in-memory
  only — persistence still B5.
- A3 (landed): `SandboxSpec` proto + supporting messages
  (`Mount`, `EnvVar`, `PortMapping`, `Protocol`, `GpuRequest`,
  `Isolation`) in `daft.sandboxctrl.v1`. No Dispatch RPC and no
  executor yet — the proto is reserved for A4 to drive
  orchestrator → sandboxd workload handoff over the existing
  RegisterSandbox stream.
- A4a (landed): wire path for orchestrator → sandboxd dispatch.
  `RegisterEvent` oneof gains a `Dispatch` variant carrying
  `dispatch_id` + `SandboxSpec`. `SandboxControlHandler` now owns a
  per-fingerprint event channel (bounded buffer), exposes
  `Dispatch(fp, id, spec)`, and the RegisterSandbox stream goroutine
  forwards the channel to `stream.Send`. Sandboxd-side: a
  `DispatchHandler` seam on `sandboxctrlclient.Config` is invoked
  per Dispatch event; handler errors tear the registration down.
- A4b (landed, minimal): sandboxd-side `internal/sandboxexec`
  package implements `DispatchHandler`. On every Dispatch event it
  logs the dispatch_id / workspace_uuid / image_ref and runs
  `sandbox.Hello` with the baseline `sandbox.Config` resolved at
  startup via `sandbox.ConfigFromEnv()`. The spec's `image_ref` and
  `mounts[]` are recorded in the trace span but do not yet drive an
  in-VM container — that path lands in Slice A5 alongside the
  `daft-sandboxd run` deprecation.
- A5a (landed): sandbox-host executor consumes `spec.image_ref`.
  New `internal/sandboxboot` package wraps the image-pull + bundle
  + lock + artefact-extract pipeline so both
  `internal/sandboxdcli` (legacy `run` command) and
  `internal/sandboxexec` (dispatch executor) can call into it.
  `Executor` now takes a `Config` struct with an `Assembler` seam;
  when `spec.image_ref != ""` the assembler runs and the bundled
  config is fed to `sandbox.Hello`. Empty `image_ref` continues
  to boot the baseline Hello (smoke-test path).
- A5b (landed): `daft-sandboxd run …` → `daft-sandboxd debug run
  …`. New `debug` parent holds the legacy / offline subcommands.
  Every invocation prints a stderr deprecation banner pointing
  users at orchestrator-driven dispatch.
- A6 (landed): protocol-version handshake on the
  `RegisterSandbox` stream. New `ProtocolVersion{major,minor,patch}`
  message; `RegisterSandboxRequest.protocol_version` carries the
  sandboxd's wire version, `SubscriptionReady.protocol_version`
  echoes the orchestrator's. Both sides validate via
  `sandboxctrl.CheckProtocolCompatibility` (major must match;
  minor/patch informational). Local version pinned at 1.0.0 so a
  peer omitting the field (proto3 zero ⇒ major=0) is rejected.
  Deliberately landed before any field-deployment so future wire
  breaks have a versioned escape hatch from day one.

Phase B — **Workspace as a first-class orchestrator object**:
- B1 (landed): `Workspace` proto +
  orchestrator-side store. New
  `proto/workspace/v1/catalog.proto` defines `Workspace` +
  `WorkspaceStatus` enum (DRAFT/READY/RUNNING/FROZEN/STOPPED/
  ARCHIVED), reusing `daft.sandboxctrl.v1` Mount/EnvVar/
  PortMapping/GpuRequest/Isolation. New
  `internal/workspacecatalog` package owns the Go-native store:
  CRUD (Create/Get/Update/Delete/List), per-owner alias index
  via (owner_fp, name), `Missing[]` recomputed on every mutation,
  and a `CanTransition` adjacency table reserved for the Slice B4
  state machine. Wired into `orchestrator.Config.WorkspaceCatalog`
  with a `Server.WorkspaceCatalog()` accessor, mirroring the
  SandboxRegistry pattern. Still in-memory only — persistence is
  B5. No service surface yet — RPCs land in B2.
- B2 (split for size; see B2a..B2d):
  `WorkspaceCatalogService` — orchestrator-facing wire surface
  over `internal/workspacecatalog`. Named with the "Catalog"
  suffix because `daft.workspace.v1.WorkspaceService` is already
  the chunk/manifest sync service.
- B2a (landed): Create, Get, List, SetImage, SetTarget,
  AddMount, AddEnv, AddPort — the assembly-time happy path.
  `WorkspaceHandle{uuid|name}` oneof lets every addressing RPC
  accept either form, with name-scoped lookups gated on the
  caller's mTLS fingerprint. Catalog errors map cleanly to
  Connect codes (NotFound / AlreadyExists / FailedPrecondition).
  UUIDs are minted server-side as RFC-4122 v4. New
  `internal/workspacecatalog.PortMapping` carries the wire
  protocol enum integer so the catalog stays the single source
  of truth for what's on the wire.
- B2b (landed): RemoveMount, RemoveEnv, RemovePort. The
  symmetric "remove what AddX added" set. Each RPC takes a
  WorkspaceHandle + the key (target / name / port+proto); a
  missing key returns NotFound so clients can distinguish it from
  a missing workspace. RemapMount / FlagMount move to Phase F
  (adoption) because they are per-user-host overrides — not
  edits to the global workspace spec — per architectural
  implication #5.
- B2c (landed): Validate, Launch, Stop. Validate auto-
  transitions DRAFT↔READY based on Missing[]; READY/STOPPED
  workspaces are launchable. Launch mints a fresh dispatch_id,
  composes a SandboxSpec from the workspace, hands it to a new
  `Dispatcher` interface (production: SandboxControlHandler;
  tests: fakeDispatcher), and transitions to RUNNING.
  Workspace.current_dispatch_id stores the correlation token.
  Dispatch error mapping: ErrSandboxNotConnected →
  FailedPrecondition, ErrDispatchBacklog → Unavailable. Stop is
  catalog-only for now (RUNNING/FROZEN → STOPPED, clears
  dispatch_id) — the wire teardown signal to the sandbox-host
  follows in B2c'.
- B2d (landed): Acquire / Release RPCs + lock gating across every
  spec/status mutator (SetImage/SetTarget, Add*, Remove*, Launch,
  Stop). New `workspacecatalog.Lock` helpers: IsHeld / IsExpired /
  HeldBy / HeldByOther; expired locks are treated as unheld so a
  runaway client can't squat a workspace forever. Acquire is also
  the refresh primitive (caller-as-holder extends ExpiresAt).
  Refusal cases map to FailedPrecondition with the prior owner's
  fingerprint in the message. CLI: `daft workspace acquire --ttl
  5m` and `daft workspace release`. Adopt remains a Phase F1
  concern; `force` + audit trail land in F3.
- B3 (landed): Client CLI surface for the catalog. New file
  `internal/cli/workspace_catalog.go` ships
  `daft workspace <create|status|list|image|target|
  mount add|mount rm|env add|env rm|port add|port rm|
  validate|launch|stop>`. Shared `workspaceClientOpts` binds
  `--orchestrator / --config-dir / --timeout / --name / --uuid`;
  workspace name defaults to basename($PWD). The deprecated
  chunk-sync subcommands (serve/pull/attach/diff) coexist at the
  top level and will move under `daft workspace debug` in a
  follow-up. `--with-dev-defaults` and `daft workspace adopt`
  defer to Phase F (per-user-host overrides). E2E test
  `TestWorkspaceEndToEndCPUFlow` drives the full
  create→image→target→mount→env→port→validate→launch→stop
  sequence and asserts the fake dispatcher saw the composed
  SandboxSpec.
- B4: **Resource-model reshape + K8s-style rename.** One pre-
  deployment wire break that introduces the conventions documented
  in [ARCHITECTURE.md](ARCHITECTURE.md). No backwards-compat
  shims — nothing is field-deployed yet, so the break is free.
  Split into mechanical sub-slices to keep each commit reviewable:
  - **B4a**: rename `sandboxctrl` → `nodectrl` end to end. Proto
    package `daft.sandboxctrl.v1` → `daft.nodectrl.v1`; directory
    `proto/sandboxctrl/v1/` → `proto/nodectrl/v1/`; Go packages
    `internal/sandboxctrl/` → `internal/nodectrl/`,
    `internal/sandboxctrlclient/` → `internal/nodectrlclient/`,
    generated code under `internal/genpb/nodectrl/`.
    `SandboxControlService` → `NodeControlService`;
    `RegisterSandbox`/`ListSandboxes` → `RegisterNode`/`ListNodes`;
    `SandboxRegistration` → `Node`. CLI `daft sandbox list` →
    `daft node list`. Find-and-replace + `make proto`. No
    semantic changes.
  - **B4b**: introduce the `Sandbox` resource (the new per-Launch
    execution unit). New `proto/sandbox/v1/` defines `Sandbox`
    with `metadata { uid, workspace_ref, created_at,
    deletion_timestamp }`, `spec { image_ref, mounts, env, ports,
    gpu_request, isolation, node_ref }`, and `status { phase,
    exit_code, conditions[], last_transition_time }`. New
    `internal/sandboxstore/` owns the orchestrator-side map of
    Sandboxes by uid. The `Dispatch` message goes away; the
    `RegisterEvent` oneof becomes `NodeEvent` carrying
    `SubscriptionReady` or `SandboxWatchEvent{EventType, Sandbox}`.
    `Workspace.Launch` now creates a Sandbox resource and emits
    `SandboxWatchEvent(ADDED)` on the target Node's stream;
    `Workspace.current_dispatch_id` becomes
    `Workspace.status.current_sandbox_ref { uid, node_fingerprint }`.
  - **B4c**: reshape `Workspace` into metadata/spec/status with
    conditions and generation. `WorkspaceStatus` enum is
    deleted; `Workspace.status.conditions[]` replaces it
    (`Ready`, `Dispatched`, `Reconciled`, `ImageResolved`).
    `metadata.generation` increments on every spec change;
    `status.observed_generation` tracks the reconciler's last
    pass. Validate becomes "set the Ready condition based on
    missing[]"; Launch/Stop check conditions instead of the
    enum. CLI `status` output renders the condition list and
    the "edit pending" indicator
    (`observed_generation < generation`).
  - **B4d**: extend `Heartbeat` to carry `NodeStatus` with
    `repeated SandboxStatus live_sandboxes`. Orchestrator's
    heartbeat handler diffs the reported set against the
    sandboxstore and updates Sandbox `status.phase` /
    Workspace conditions accordingly. This is the
    reconciliation primitive — pre-action reconcile and
    periodic sweep (B5) build on it. No new RPC; the
    extension is additive on the existing unary call.
- B5: **Reconciler.** Pull the per-RPC diff logic from B4d into
  `internal/reconciler/` and wire it at the four reconciliation
  points described in ARCHITECTURE.md (pre-action, on heartbeat,
  periodic sweep, startup). Phantom-Sandbox cleanup: any Sandbox
  the orchestrator believes RUNNING that no Node reports for
  three consecutive heartbeats is transitioned to `Killed` with
  reason `SandboxLost`. Sweep cadence configurable; default
  30 s.
- B6: **Persistence.** Badger-backed store for the *declarative*
  half of each resource (Workspace.spec, Workspace.metadata,
  Sandbox.spec.node_ref binding, Node.metadata). Runtime
  state (`Sandbox.status`, `Workspace.status`, `Node.status`)
  is **not persisted** — it is always derived by reconciliation
  on startup. Schema versioned by an on-disk `schema_version`
  key; migrations land alongside future wire breaks. Catalog
  Create/Update/Patch become write-through; reads stay served
  from the in-memory cache. Crash-safety verified by a
  kill-9-mid-write integration test.

Phase C — **Orchestrator-driven port ingress**:
- C1: Sandbox-side: container `0.0.0.0:N` exposed back over the
  broker as a labeled stream (`workspace=<uuid>, port=N`).
  - C1.1 (landed): Agent in-VM port pump. `daft.port=<vsockPort>:<containerPort>`
    kernel cmdline (repeatable); the agent vsock-listens per token and
    piped each accepted connection to `127.0.0.1:<containerPort>` inside
    the workload container.
  - C1.2 (landed): Sandboxd-side host primitive. New
    `sandbox.DialGuestVsock(ctx, hostSocket, vsockPort)` performs the
    Cloud Hypervisor hybrid-vsock CONNECT handshake (`CONNECT N\n` →
    expect `OK …\n`) and returns the resulting byte tunnel.
    `sandbox.Config` gains a `PortForwards []PortForward` slice that
    emits `daft.port=…` tokens in `buildKernelCmdline`. The
    sandboxd executor walks `SandboxSpec.spec.ports` and assigns
    distinct guest-side vsock ports starting at
    `sandboxexec.BaseVsockPort` (10000) for every TCP entry; UDP and
    zero-container-port entries are skipped. Validation rejects
    duplicate VsockPort, zero ports, and TracePort collisions before
    boot.
  - C1.3 (in progress): Workspacebroker labeled-stream registration —
    sandboxd registers each (workspace_uuid, container_port) with the
    orchestrator and bridges the labeled inbound stream to a
    DialGuestVsock call. Enables C2. Split into three sub-slices:
    - C1.3a (landed): `internal/sandboxgateway.Bridger` — local
      (workspace_uuid, container_port) → DialFunc registry plus a
      Bridge(ctx, ws, port, upstream) that pipes bytes both
      directions until either side closes. Stateless DialFunc
      indirection keeps the package free of any vsock/CH knowledge;
      C1.3b registers a DialGuestVsock closure per spec.ports entry.
    - C1.3b (landed): Executor wires the Bridger. `sandboxexec.Config`
      gains Bridger + VsockDir fields; New rejects the half-configured
      case at construction. Each dispatch with ports allocates a fresh
      AF_VSOCK GuestCID (atomic counter ≥3) and a per-Sandbox UDS path
      under VsockDir, populates `sandbox.Config.Vsock`, and registers
      one DialGuestVsock closure per spec.ports entry on the Bridger.
      A deferred unregister cleans up on every dispatch exit. Skipped
      silently when no workspace_uuid (the smoke-test probe path) or
      no Bridger (back-compat for tests not exercising ingress).
      Production wiring (passing a real Bridger from sandboxd serve)
      lands with C1.3c so the Bridger has an actual consumer the
      moment it gains routes.
    - C1.3c.1 (landed): Orchestrator-driven Bridge — sandboxd
      half. Proto adds an `OpenPortRequest` variant on `NodeEvent`
      and a new `OpenPortStream` bidi RPC carrying
      `PortStreamMessage{hello|data}` frames. `nodectrlclient`
      dispatches the event to `handleOpenPort`, which dials
      `OpenPortStream`, sends Hello with the announced session_id,
      wraps the post-handshake bidi stream as `portStreamConn` (a
      `net.Conn`), and hands it to `Config.Bridger.Bridge`.
    - C1.3c.2 (landed): Orchestrator-driven Bridge — orchestrator
      half. `NodeControlHandler.OpenPortBridge(ctx, fp, ws, port)`
      mints a base64-url session id, registers a pending entry,
      pushes the `OpenPortRequest` event on the addressed Node's
      RegisterNode stream, and parks until the matching
      `OpenPortStream` arrives. `OpenPortStream` reads the Hello,
      matches against the pending map, hands the bidi stream off
      via a `portStreamArrival` channel, then blocks until the
      bridge caller closes its `net.Conn` (which closes the
      release signal, releasing the handler). Errors:
      ErrNodeNotConnected, ErrSandboxEventBacklog, context timeout,
      ErrHelloMissingSession, ErrUnknownPortSession.
    - C1.3c.3 (landed): production Bridger wiring + e2e test.
      `daft-sandboxd serve` instantiates one
      `sandboxgateway.Bridger`, passes it to both the
      `sandboxexec.Executor` (so dispatches register routes) and
      `nodectrlclient.Config` (so OpenPort events find them). New
      `--vsock-dir` CLI flag with a `<TempDir>/daft-sandboxd-vsock-<pid>`
      default; `resolveVsockDir` rejects relative paths and mkdirs
      the target. E2E test (`TestOpenPortBridgeEndToEnd`) wires a
      real Connect orchestrator + sandboxd over HTTP/2+mTLS, parks
      until the heartbeat refresh, calls `OpenPortBridge` from the
      orchestrator side, and round-trips `ping-pong` through every
      layer: orchestrator `OpenPortRequest` → sandboxd
      `handleOpenPort` → bidi `OpenPortStream` RPC →
      `Bridger.Bridge` → registered DialFunc → in-process echo
      loop. `TestOpenPortBridgeNoRouteClosesConn` covers the
      missing-route teardown path.

  **Phase C1 closed.** The byte path from `OpenPortBridge` rightward
  is exercised end-to-end; C2 (per-workspace HTTP listener that
  drives `OpenPortBridge` from inbound HTTP traffic) is the next
  user-visible step.
- C2 (deferred): Orchestrator side: per-workspace TCP listener
  on an orchestrator-host interface that calls OpenPortBridge and
  splices bytes. Per-workspace forwards persisted in
  Workspace.spec; opened on Launch, closed on Stop. TLS
  termination remains an option but isn't required — the raw-TCP
  forward is sufficient for most ingress.
- C3: User-host port forward.
  - C3.1 (landed): Orchestrator-side `ForwardPort` bidi RPC on
    `WorkspaceCatalogService` + `ForwardFrame{hello,data}` proto.
    The orchestrator handler reads the Hello (workspace handle +
    container_port), resolves the workspace, gates on
    `status.current_sandbox`, calls `OpenPortBridge` on the
    `WorkspacePortBridger` seam (production: NodeControlHandler;
    tests: a fake bridger), and `io.Copy`-splices the bidi stream
    with the resulting `net.Conn`. Errors:
    ErrForwardHelloMissing, ErrForwardWorkspaceNotRunning,
    NotFound (unknown workspace), Unimplemented (no bridger).
    `forwardFrameConn` adapts the bidi stream as `net.Conn`,
    structurally parallel to `serverPortStreamConn` (kept
    side-by-side with `//nolint:dupl` — proto types differ).
  - C3.2 (landed): `daft workspace forward <local>:<container>
    <workspace>` CLI on the user-host. Opens a local TCP listener
    on `--bind` (default 127.0.0.1), per-accepted-conn dials a
    fresh `ForwardPort` RPC, sends Hello, and `io.Copy`-splices
    bytes in both directions. The pump uses cross-cancellation
    (Close conn + CloseResponse stream on first goroutine exit)
    so a half that errors first doesn't leave its partner parked.
    forwardDeps injection seam lets tests cover the full
    listen → accept → tunnel → pump path with a fake echo
    stream factory and a real local TCP listener bound to an
    OS-assigned port.
  - C3.3 (landed): end-to-end test wiring CLI → orch → sandboxd
    → echo DialFunc. `TestWorkspaceForwardEndToEnd` brings up a
    real orchestrator (mTLS + HTTP/2 Connect, default
    WorkspacePortBridger = the orchestrator's own SandboxControl
    handler), a real `nodectrlclient` registered as a Node with a
    real `sandboxgateway.Bridger`, seeds a RUNNING workspace
    owned by the client identity, and runs
    `runWorkspaceForwardWith` against the orchestrator. A test
    TCP client dials the CLI's local listener and round-trips
    bytes through every layer (CLI → ForwardPort RPC → handler →
    OpenPortBridge → push OpenPortRequest → sandboxd dispatch →
    OpenPortStream RPC → Bridger.Bridge → DialFunc → echo
    net.Pipe). Catches integration regressions the per-layer
    unit tests can't see (proto rename ripples, missed Hello
    shape changes, cross-cancellation deadlocks). C3 is now
    fully exercised end-to-end; the user-host port-forward
    feature is shippable. C2 (orchestrator-side per-workspace
    TCP listener that drives `OpenPortBridge` from inbound
    traffic on an orchestrator-host interface) is the natural
    next step.
- C4: Allowlist of egress destinations per workspace (the
  "shadow services" hook): outbound from the container is
  intercepted at the controller and either forwarded
  upstream or routed back to the orchestrator for inspection.

Phase D — **Attach + GPU**:

The Phase D scope was reshaped (2026-05-17) once the actual
blocker for the reference CPU workflow became clear: the user
needs a shell inside the container to drive `nix develop` / `nix
run` against their flake. That is **attach**, not the heavier
multi-container scheduling story the old D2 conflated. Attach
(new D2) is a structural clone of C3 with a PTY on the far end
instead of a TCP port; multi-container moves to D5, where it
actually belongs (GPU-VM amortisation, not interactive dev).

In-VM build (was Phase E) is also descoped — for the CPU
reference workflow, "boot the nix container image with bash as
PID 1 and let the user `nix develop`" is the path. A dedicated
in-VM Dockerfile/flake build pipeline (the old Phase E) is
deferred until a workflow needs it; image construction lives on
the user-host or in CI today.

- D-pre (landed): **Agent lifecycle — listeners first, orchestrator
  Serve.** Two small but load-bearing changes that unblock anything
  past hello-world. (1) In `internal/agent`, port-forwarders (and
  future attach listeners) are spawned *before* `runContainerFromCmdline`
  so they're already accepting when a long-running container like
  `bash` would otherwise block the agent's main goroutine. (2) New
  `sandbox.Serve(ctx, w, cfg)` mirrors stdout for the VMM's full
  lifetime and returns on `ctx.Done()` OR VMM exit — no banner gate.
  `sandboxexec.Executor`'s default Runner switches from
  `sandbox.Hello` to `sandbox.Serve`, so the orchestrator-driven
  path supports interactive workloads where the container outlives
  any console-side handshake. `sandbox.Hello` stays as the
  smoke-test variant (boot → see banner → tear down) for the
  one-shot CLI path. Total ~80 lines, no test rewrites.
- D1: Sandboxd resource advertisement: GPU BDFs + VRAM, current
  utilisation; orchestrator scheduler.
- D2: **Interactive attach (CPU+GPU).** `daft workspace attach
  <workspace> [-- cmd ...]` opens a PTY into the running
  container's sandbox. Structurally a clone of C3 — same
  session-id rendezvous via the orchestrator, same bidi-stream
  pump — with a PTY on the agent side instead of a TCP forward.
  Default cmd is `/bin/sh -l` (bash if present). Unblocks the
  nix-shell reference workflow. Sliced:
  - D2a (landed): proto + orchestrator + sandboxd halves.
    New `AttachSandbox` bidi RPC on `WorkspaceCatalogService`
    carrying `AttachFrame{hello,data,resize}`; new
    `NodeEvent.AttachRequest{session_id, sandbox_uid,
    command[], tty, cols, rows}`; new `AttachStream` bidi on
    `NodeControlService`. Orchestrator gets
    `OpenAttachBridge` mirroring `OpenPortBridge` (mint
    session, push event, wait for sandboxd to dial back, hand
    back an `AttachSession` that wraps the bidi stream as a
    net.Conn plus a `Resize(cols, rows)` side-channel). The
    `AttachSandbox` connect handler reads the Hello, resolves
    the workspace + RUNNING gate (same shape as ForwardPort),
    calls `OpenAttachBridge`, and runs `pumpAttach` — a bidi
    pump that splices Data frames as bytes and forwards
    Resize frames via `session.Resize`. New transport-free
    `sandboxgateway.AttachBridger` + `AttachSession`
    interfaces; production `daft-sandboxd serve` wiring lands
    in D2b. `nodectrlclient` grows a parallel
    `handleAttach`/`runAttach`/`pumpAttachStream` dispatcher
    on the sandboxd side. Same `//nolint:dupl` discipline as
    OpenPort — proto types differ, side-by-side review is
    intentional.
  - D2b: daft-agent PTY exec. Vsock listener that accepts
    `AttachRequest`, spawns `crun exec -t <id> <cmd...>`
    against the running container, splices the PTY master fd
    to the vsock stream, forwards `Resize{rows, cols}`
    control frames. Sub-sliced:
    - D2b.1 (landed): proto + agent vsock listener + framing.
      New `proto/agent/v1/attach.proto` carrying
      `AgentAttachHello` + `AgentAttachFrame{data, resize, exit}`
      with the same 4-byte BE length-prefix framing as the
      trace stream. New `daft.attach_port=<n>` kernel cmdline
      parameter. New `internal/agent/attach.go`:
      `runAttachListener` accept loop, `handleAttach` per-conn
      (read Hello → `ExecAttach.Start` → bidi pump →
      terminal Exit frame). `ExecAttach` interface is
      injected so unit tests drive the listener against an
      in-process `io.Pipe`-backed `AttachProcess` without
      spawning a real PTY. Production `defaultAttachDeps`
      wires a stub `unavailableAttachExec` until D2b.2 lands
      the real crun-exec-with-PTY. The listener is started
      before the container in `runWith`, same lifecycle
      rule the port-forwarders follow (D-pre).
    - D2b.2 (landed): production `ExecAttach`
      implementation in `internal/agent/attach_exec_linux.go`.
      Opens `/dev/ptmx`, unlocks + opens the matching
      `/dev/pts/<n>` slave, wires both halves to the child
      stdio via `Setsid`+`Setctty`, returns a
      `ptyAttachProcess` that proxies Read/Write over the
      master fd, Resize over `TIOCSWINSZ`, and Wait over
      `cmd.Wait`. Close sends SIGHUP via master-close and
      SIGKILL to the process group as a safety net so a
      workload that ignores SIGHUP can't pin the agent. The
      cmd-builder is split out behind `newCmd` so tests
      substitute `/bin/cat` for crun and exercise the PTY
      plumbing without a running container; the production
      builder constructs `crun exec -t <ContainerID>
      <argv...>` (defaulting to `/bin/sh -l` for an empty
      command). New `golang.org/x/sys` direct dep. Non-Linux
      builds keep the unavailableAttachExec stub.
    - D2b.3: sandboxd-side `AttachBridger`. Sub-sliced:
      - D2b.3a (landed): host-side framing client +
        per-sandbox registry. New `internal/sandboxattach`
        package: `OpenSession(ctx, conn, req)` sends the
        AgentAttachHello and wraps the framed conn as a
        `sandboxgateway.AttachSession` (Read yields Data
        bytes, Write wraps in Data frames, Resize wraps in
        Resize frames, agent's Exit frame surfaces as
        io.EOF). New `sandboxgateway.AttachRegistry`:
        sandbox_uid-keyed table of `AttachDialer`s,
        implements `AttachBridger`. Both pieces are
        transport-free so unit tests drive them against
        `net.Pipe`.
      - D2b.3b (landed): executor + serve.go wiring.
        `agent.DefaultAttachVsockPort = 9000` is the
        conventional guest-side port (between TracePort=8000
        and BaseVsockPort=10000). `sandbox.VsockConfig`
        carries a parallel `AttachPort uint32`; when set,
        `buildKernelCmdline` emits `daft.attach_port=<N>` so
        the agent's listener comes up. `sandboxexec.Config`
        gains `AttachRegistry *sandboxgateway.AttachRegistry`
        (same `VsockDir` requirement as `Bridger`).
        `resolveBootConfig` allocates a Vsock device whenever
        either the port-forward or attach branch needs it,
        and sets `Vsock.AttachPort` when the attach registry
        is wired. `HandleSandboxEvent` defers
        `registerAttachRoute` alongside `registerPortRoutes`;
        the route is a closure that dials the per-sandbox
        hybrid-vsock UDS via `sandbox.DialGuestVsock`, runs
        `sandboxattach.OpenSession`, and returns the
        resulting `AttachSession`. `sandboxdcli/serve.go`
        instantiates the registry and shares it with both
        the executor and the nodectrlclient (as the
        `AttachBridger` Config field already added in D2a).
        End-to-end: an orchestrator-side `AttachSandbox`
        call now flows orch → sandboxd
        → executor's registered dial → in-VM agent's PTY,
        with no missing seam.
  - D2c (landed): user-host CLI. New
    `daft workspace exec <workspace> [-- cmd args...]` —
    chosen over `attach` because the existing
    `daft workspace attach` is the brokered sandbox-pairing
    command from Phase 3.5 and renaming it would churn the
    older workflow's tests. `exec` matches the
    docker/kubectl convention. New
    `internal/cli/workspace_exec.go` opens the
    `AttachSandbox` bidi RPC, sends the Hello with the
    workspace handle + initial winsize, then drives three
    concurrent pumps: stdin → Data frames, Data frames →
    stdout, SIGWINCH → Resize frames. Local terminal goes
    into raw mode via a small termios helper in
    `workspace_exec_terminal_unix.go` (linux || darwin); the
    raw-mode restore is deferred so a Ctrl-C cooked-mode
    restore is guaranteed. Falls back to cooked mode when
    raw-mode fails (warns on stderr), and to piped mode
    when stdin isn't a TTY. `execDeps` + `execTerminal` are
    injectable seams; unit tests drive the full session
    against an in-process fake stream and a PTY-backed
    terminal harness.
  - D2d (landed): full-stack attach e2e test
    (`internal/sandbox/integration_attach_test.go`). Boots a
    real Cloud Hypervisor VM via `sandbox.Serve` with a
    long-running container as PID 1 (the existing
    daft-hello-container gained a `--keepalive` flag that
    prints the banner and then blocks on SIGTERM/SIGINT, so
    the same binary is both the in-VM PID 1 *and* the exec
    target for attach). The test dials the agent's attach
    vsock listener via `DialGuestVsock`, opens a session with
    `sandboxattach.OpenSession`, asks the agent to exec
    `/hello` in the running container, and asserts
    `agent.ContainerBanner` round-trips back through the PTY
    tunnel. Surfaced two real bugs in the D2b implementation
    that unit tests on the host couldn't see: (a) the u-root
    initramfs doesn't mount devpts, so the agent's
    `openPtyPair` opening `/dev/ptmx` failed in the VM —
    fixed by a small `ensureDevPts` helper that the PTY
    allocator calls lazily, and by switching the path to
    `/dev/pts/ptmx`; (b) `crun exec -t` opens `/dev/ptmx` in
    the **container's** mount namespace, which is an empty
    tmpfs in v0 bundles — fixed by dropping `-t` (the agent's
    PTY slave is already a real TTY that crun-exec hands
    directly to the child).
- D3: GPU-bound VM lifecycle: orchestrator keeps the VM up
  while ≥1 workspace targets it; stops it when idle for $TTL
  (default 10 min) or on operator command.
- D4: `--isolation strict` opt-out from sharing — 1 VM per
  workspace even with `--gpu`. Same control surface as CPU
  mode.
- D6: **Black-box workflow harness.** Tests drive the real
  `daft` CLI through tmux on the host and assert on captured
  pane state, exactly as a human user would type and read at a
  terminal. The pane is always a real PTY (tmux allocates one)
  so any CLI driven through it exercises its raw-mode +
  winsize code path just like in production.
  - D6a (landed): `internal/testtmux` package. Each Session
    owns its own tmux server (via `-S <t.TempDir()>/tmux.sock`
    so nothing leaks into the user's user-default tmux). The
    pane shell is **dash** — POSIX-strict, predictable signal
    handling, no readline / history quirks that bash and zsh
    inject. Exposes `Send` (raw tmux key vocabulary), `Enter`
    (literal text + Enter), `Capture` (pane + scrollback),
    `WaitFor` (poll until substring matches, fatal on
    timeout). Boot handshake uses a shell-arithmetic sentinel
    (`printf 'TESTTMUX-READY-%d\n' $((1+2))`) so WaitFor only
    fires once dash has actually processed PS1 setup — a
    plain echo would be matched by WaitFor on the typed-
    command echo before dash had run it. Cleanup is
    `tmux kill-server` per Session, which reaps the pane's
    entire shell-tree. `tmux` and `dash` added to flake.nix's
    devShell; running these tests outside the dev shell is
    unsupported by design (same convention as the KVM-
    required integration tests refuse to skip when /dev/kvm
    is missing — skipping would zero the coverage gate). D6b
    will use this harness; D6a ships as the harness + smoke
    tests so flakiness in the harness itself surfaces on a
    handful of tiny tests rather than the full workflow proof.
  - D6b (landed) is the end-to-end nix-shell + dev-server proof,
    sliced further so each cut is reviewable:
    - D6b.1 (landed): `internal/testdaemons` subprocess
      harness. `NewCluster(t)` builds the daft / daft-
      orchestrator / daft-sandboxd binaries lazily (sync.Once
      shared across tests via `go build` + Go's build cache),
      generates orchestrator + sandboxd + client identities,
      cross-approves all four edges (orch↔sbx, orch↔client),
      grabs a free loopback port, and spawns each daemon with
      `Setpgid: true` + `Pdeathsig: SIGKILL`. Cleanup SIGTERMs
      the process group and Waits with a bounded `WaitDelay`
      so a daemon that leaks a goroutine pinning stdio can't
      hang the test. Preflight checks the artefact env vars
      (`DAFT_INITRAMFS` etc.) and fatals fast if they're
      missing — otherwise sandboxd exits ~10ms after spawn and
      the 15s WaitFor timeout becomes the user's first signal.
      Smoke test (`TestClusterDaftNodeListShowsSandboxd`)
      drives `daft node list` through a tmux pane and asserts
      the sandboxd's fingerprint appears in the output —
      proves the whole subprocess control plane (mTLS,
      registration, CLI rendering) end to end in ~1.8s, no VM
      involved. Makefile's `integration` target now runs
      `./internal/testdaemons/` alongside `./internal/sandbox/`.
    - D6b.2 (landed): workspace fixture + `daft workspace
      launch` against the hello bundle. First slice that
      actually boots a VM through the subprocess control
      plane. Stands up an in-process OCI registry, pushes the
      daft-hello-container binary as a single-layer image,
      drives `daft workspace create / image / mount / target /
      launch` through a tmux pane (sentinel-pattern per step:
      `printf 'D6B-%d\n' $((<base>+$?))` so the success token
      only appears in command output, not the typed-line
      echo). Asserts `Status: Dispatched` in launch's CLI
      response, then waits for the in-VM container banner to
      surface through the sandboxd's stdout — the testdaemons
      `outputCapture` (added in this slice) accumulates a
      thread-safe view of daemon output alongside the t.Log
      forwarding so tests can assert on it. Required one
      small production change: the `daft-sandboxd serve`
      wiring now wraps `bundled.Extract` with
      `extractWithEnvFallback`, which lets a plain
      `go build`-produced sandboxd boot VMs using the env-var-
      pointed artefacts when its embedded assets are stubs.
      The Nix-built binary still uses the embedded assets;
      this fallback only fires on dev builds. Test runs in
      ~3.1s end-to-end via `make integration`.
    - D6b.3 (landed): nix-based container image + workspace
      exec. flake.nix gained a `nix-shell-image` derivation
      (dockerTools.buildLayeredImage of dash + coreutils with
      a `sleep infinity` entrypoint that prints a readiness
      sentinel on startup); the dev shell exports
      `DAFT_NIX_IMAGE` pointing at the realised tarball. The
      new integration test loads the tarball into an in-process
      OCI registry, drives the full `daft workspace create /
      image / mount / target / launch` sequence through the
      tmux pane (same arithmetic-sentinel pattern as D6b.2),
      waits for the readiness sentinel on the sandboxd's
      stdout, then issues `daft workspace exec --tty=false
      ws -- /bin/dash -c 'echo D6B3-EXEC-$((6000+$?))'` and
      asserts the output appears in the pane. End-to-end in
      ~4.2s. Surfaced and fixed three real bugs along the way:
      (i) `internal/imagestore.writeTarEntry` honoured the tar
      header's directory mode, so Nix-built images that ship
      0o555 dirs blocked subsequent-layer extractions with
      EACCES; the extractor now relaxes ancestor dirs to u+rwx
      during extraction (rootfs is readonly at runtime, so
      on-disk modes don't affect container security);
      (ii) `cli.pumpExecSession` waited on both stdin and
      stream goroutines, deadlocking when the local stdin
      was a never-EOFing TTY (Go can't interrupt a blocked
      Read syscall) — the stream pump is now the lifecycle
      owner and the stdin pump is fire-and-forget;
      (iii) `nodectrlclient.dispatchEvent` ran sandbox events
      inline, freezing the registration-stream loop for the
      VM's full lifetime so AttachRequest / OpenPort events
      could never reach `handleAttach` / `handleOpenPort` —
      sandbox events now run in their own goroutine like the
      other event kinds, with handler errors surfaced via the
      log sink instead of tearing the stream down (one broken
      sandbox shouldn't punish concurrent dispatches).
    - D6b.4 (landed): dev-server + port-forward proof. A new
      `cmd/daft-hello-server` Go binary (a static HTTP listener
      with a known body) ships baked into the nix-shell-image
      at `/bin/daft-hello-server`. The integration test stands
      up the subprocess control plane, declares container
      port 8080 on the workspace, launches, then spawns
      `daft workspace exec` (running the HTTP server) and
      `daft workspace forward` as managed Go subprocesses
      (driving them through tmux with shell `&` was attempted
      but daft inherits stdin=/dev/null in the bg-job case and
      hangs silently before the orchestrator handshake — the
      direct exec.Cmd path is simpler and gives clean Cleanup
      via SIGTERM). The test waits for each subprocess's
      readiness banner via a file-tail helper, HTTP-GETs the
      local forwarder, and asserts the body surfaces back from
      the container. The byte path that succeeds is:
      local listener → ForwardPort RPC → orchestrator →
      sandboxd → vsock → agent (TCP-dial 127.0.0.1:8080 in
      the VM's loopback) → container → daft-hello-server →
      response back. ~96s end-to-end.

      Surfaced one more production bug: the OCI runtime spec
      generated by internal/imagestore unconditionally
      included a `network` namespace, putting the container's
      bound port out of reach of the agent's loopback dial.
      The v0 design has one container per sandbox VM with the
      VM as the security boundary, so the container netns is
      now omitted — the container shares the VM's loopback
      (where the agent already runs). D5 (multi-container per
      VM) will need to revisit; the runtime-spec comment
      flags it.
  - D6c: real nix-shell workflow — the dev-server proof above
    used a static Go HTTP listener baked into the image; the
    user-facing story is "the container has the nix CLI, the
    /workspace mount carries a flake, the user runs
    `nix develop -c <web server>`". D6c covers that proof,
    sliced again so each cut is reviewable:
    - D6c.1: hermetic nix-CLI proof — **landed**. The image
      build (`nix-tooling-image` + `d6c1-flake-fixture` in
      flake.nix), the mount-spec wiring through the orchestrator
      path (sandboxboot's BootOptions, sandboxexec
      mountsFromSpec), and the runtime-spec writable-rootfs +
      `/tmp` / `/nix/store/.links` tmpfs scaffolding +
      image-User parsing all shipped in the first commit.
      Integration test was initially t.Skip'd against a
      virtiofs-perms wall.
    - D6c.1b: virtiofs perms — **landed**. The unblock turned
      out to be a one-entry user-namespace mapping in the OCI
      spec: container uid 0 / gid 0 map to the sandboxd /
      virtiofsd process uid/gid. virtiofsd's unprivileged fs
      syscalls then come from the file owner so DAC checks
      pass. BundleOptions.HostUID / HostGID drive it;
      sandboxexec and sandboxdcli populate them with
      os.Geteuid() / os.Getegid(). t.Skip removed,
      `TestClusterWorkspaceForwardAgainstNixRun` green in
      ~14s. Full byte path proven: local listener → ForwardPort
      → orchestrator → sandboxd → vsock → agent → container →
      `nix run` → python3 → http.server → response.
    - D6c.2: sandbox VM network. The path: a gvisor-tap-vsock
      userspace TCP/IP stack runs inside sandboxd, accessed by
      the guest through the existing Cloud Hypervisor
      hybrid-vsock device on a second port. Guest agent dials
      AF_VSOCK CID 2 port N; CH proxies that stream onto a
      host UDS sandboxd is listening on (same pattern the
      trace exporter already uses); sandboxd hands the
      accepted conn to `virtualnetwork.AcceptStdio`. Frames
      are HyperKit-style: `<u16-le size><eth frame>`. The
      guest agent runs a tap-and-bridge loop (port of
      gvforwarder's ~150 LoC into daft-agent) between tap0
      and the vsock conn; the container netns picks up
      egress through tap0 via veth+bridge. No
      CAP_NET_ADMIN on the host process, no slirp/passt
      dependency — pure Go.
    - D6c.2-spike: **landed**. internal/sandboxnet/spike_test.go
      runs `virtualnetwork.New` in-process, pipes a net.Conn
      to `AcceptStdio`, and confirms an ARP request is
      reflected as an ARP reply with the gateway MAC.
      gvisor-tap-vsock v0.8.9 added to go.mod;
      vendorHash refreshed. Outstanding unknowns from
      research now closed:
        * Library accepts `net.Conn` directly (no HTTP /connect
          dance needed) via `AcceptStdio` + StdioProtocol.
        * Framing matches CH's UDS hop (raw byte stream, no
          extra framing injected by CH).
        * Building cleanly within our Nix Go module setup.
      Real D6c.2 slices:
        1. sandboxd-side: open `<HostSocket>_<netPort>` UDS
           listener (lift the pattern from
           startVsockTraceReceiver), run a per-VM
           VirtualNetwork, plumb accepted conns into
           AcceptStdio. Configure Subnet/GatewayIP per VM.
           **Landed.** internal/sandboxnet exposes Config /
           New / Accept around virtualnetwork. VsockConfig
           gains NetPort with cross-port-collision
           validation. sandbox.startVsockNetReceiver wires
           a UDS listener at `<HostSocket>_<NetPort>` into
           the network on accept; round-trip pinned by
           TestStartVsockNetReceiverRoundTrip.
        2a. agent-side wire path: new daft.net_port cmdline
            key, sandboxboot emits it from cfg.Vsock.NetPort,
            agent parses + dials AF_VSOCK CID 2 port net_port
            via StartNetBridge, holding the conn for the slice-2b
            pumps to plug into. No tap yet — this slice validates
            the byte path end-to-end against the slice-1 listener
            without pulling in tap/netlink deps. **Landed.**
        2b. agent-side tap + bridge: create tap0 via TUNSETIFF
            (raw ioctl, no songgao/water dep), bring up the link
            via vishvananda/netlink (LinkSetMTU + AddrAdd +
            LinkSetUp + RouteAdd default-via-gw), and run the
            rx/tx pump loops with HyperKit framing (2-byte LE
            size prefix) between tap0 and the slice-2a vsock
            conn. Static address 192.168.127.2/24, default route
            via 192.168.127.1 — matches sandboxnet's gvproxy
            gateway. No DHCP yet; can layer udhcpc on top later
            if the container needs DNS pushed by the embedded
            DHCP server. **Landed.** internal/agent/netpump.go
            holds the cross-platform framing pump;
            internal/agent/tap_linux.go holds the Linux-only
            TUNSETIFF + netlink wiring; net.go's tapDeps factors
            the side-effect surface so unit tests substitute
            net.Pipe pairs for the tap fd. Cross-port collision
            still pinned by sandbox.VsockConfig.validate. Pump +
            attachTapToBridge covered by unit tests; the
            kernel-side tap-up path stays untested until the
            slice-3 integration test exercises it with a real
            netns (no root in test runners).
        3. container egress wiring — **landed**. The
           "container netns + veth + bridge" speculation in
           this note was moot: D6b.4 already dropped the
           container netns, so the container inherits tap0 +
           the default route from the VM's init netns for
           free. The remaining gap was DNS. Two production
           changes shipped this slice: (a)
           `sandboxexec.Config.NetEnabled` toggles a per-VM
           sandboxnet listener — `resolveBootConfig` now
           allocates a Vsock device and sets
           `cfg.Vsock.NetPort = agent.DefaultNetVsockPort`
           (9500, between AttachPort=9000 and
           BaseVsockPort=10000) when the flag is set.
           `daft-sandboxd serve` turns it on; unit tests
           leave it off so they don't have to wire a real
           sandboxnet stack. (b) The agent's `runWith` writes
           `<bundle>/rootfs/etc/resolv.conf` with
           `nameserver 192.168.127.1` whenever the net
           bridge succeeds AND a `daft.bundle=` path was
           parsed. Atomic write-and-rename, idempotent
           across VMs sharing a bundle; overwrites any
           image-baked resolv.conf because the sandboxnet
           gateway is the only route the container has.
        4. integration test — **landed**. Hermetic, no public
           internet required:
           `testdaemons.TestClusterWorkspaceHTTPSEgress` stands
           up an `httptest.NewTLSServer` on the host's loopback,
           rewrites the URL host to `sandboxnet.DefaultHostLoopbackIP`
           (192.168.127.254), drives the standard workspace
           create/image/mount/target/launch sequence, then
           `daft workspace exec /bin/daft-http-fetch --insecure`
           inside the container and waits for the
           `daft-http-fetch ok status=200 bytes=<n> sha256=<hex>`
           sentinel in the tmux pane. Cross-checking sha256
           against the host body pins data integrity, not just
           reachability. Three production changes shipped this
           slice: (a) `sandboxnet.Config.HostLoopbackIP` (default
           192.168.127.254) drives `GatewayVirtualIPs` + `NAT`
           on gvisor-tap-vsock's `types.Configuration` so the
           guest can dial host loopback through the magic IP.
           (b) `cmd/daft-http-fetch` — a tiny static HTTPS
           client baked into the nix-shell-image alongside
           daft-hello-server; `--insecure` is opt-in and unit-
           tested to confirm the default (verify-on) path still
           rejects the same self-signed cert. (c)
           `third_party/kernel-config/daft.config` adds
           `CONFIG_TUN=y` — without it the guest had no /dev/net/tun
           and the agent's tap-up path failed with ENOENT, so
           the container saw ENETUNREACH on every connect. The
           kernel-side tap-up path that 2b left untested is now
           covered end to end. Slice-2b also shipped with a
           latent flake that didn't surface until this slice's
           e2e test exercised the pumps under real traffic: Go's
           runtime registers fds opened via `os.OpenFile` with
           its epoll-based netpoller, and `/dev/net/tun`'s poll
           method is incompatible — reads sometimes return
           `read /dev/net/tun: not pollable` and the tap pump
           aborts. The pump's deferred `stop()` then closes both
           the tap fd and the vsock conn; the kernel destroys
           `daft0`, the container's `/proc/net/route` empties,
           and every connect returns ENETUNREACH. The earlier
           "GC-finalizer race" hypothesis was a misdiagnosis;
           the real fix is to open `/dev/net/tun` via
           `syscall.Open` + `os.NewFile` so the fd lands on
           Go's non-pollable file path and reads go straight to
           `syscall.Read`. The bridge-pinning goroutine added in
           the misdiagnosis still earns its keep — it makes
           teardown explicit and idempotent — but the netpoller
           fix is what makes the integration test stable
           (8/8 in isolation, full e2e sweep green).
      Connectivity allowlists (C4 in Phase C) layer on top later.
    - D6c.3: full vision. Sliced:
        1. **landed**. `testdaemons.FetchNixOfficialImage`
           caches `docker.io/nixos/nix@sha256:bf1d…` under
           `os.UserCacheDir()/daft-test-images/`; first run
           pulls (~100 MB), subsequent runs reuse. The cache
           survives `make clean` (it doesn't live under bin/).
           Smoke test `TestClusterWorkspaceAgainstNixOfficial`
           loads the cached tarball, mutates the image config
           to swap the upstream `nix` entrypoint for
           `/bin/sh -c "echo D6C3-READY; exec sleep infinity"`
           (image layers untouched, so it's still upstream's
           nix binary), pushes via the existing in-process
           registry, launches a workspace, and asserts
           `daft workspace exec … -- /bin/sh -lc 'nix --version'`
           prints `nix (Nix) …` in the pane. `sh -lc` is
           needed because crun-exec doesn't inherit the
           entrypoint's process.env (same gap D6c.1 hit) so
           PATH only resolves nix after sourcing
           `/etc/profile.d/nix.sh`. 3/3 stable in ~4 s.
        2. real dev-shell workload — `nix develop -c bun dev`
           against a flake fixture that pins nixpkgs + pulls
           bun. Will exercise the D6c.2 egress path: the first
           `nix develop` fetches bun's closure from
           cache.nixos.org through sandboxnet's NAT.
           Port-forward + HTTP-GET asserts a known body
           served by `bun dev`.

           Throughput floor under investigation: in-VM
           `curl` of the nixpkgs tarball through the sandboxnet
           NAT clocks ~50 KB/s on a host that hits the same
           URL at megabits-per-second. Layer-by-layer
           benches so far:

           - `internal/agent.BenchmarkPump*` proves the
             agent's frame pump (tap ↔ stdio) runs at
             ~720 MB/s at MTU 1500 (net.Pipe substrate). Pump
             is not the bottleneck.
           - `internal/sandboxnet.BenchmarkAcceptARPRoundTrip`
             drives ARP request→reply through the gvisor
             switch + IP layer at ~180k frames/sec
             (~5.5 µs/frame, ~7.6 MB/s on 42-byte frames).
             Even allowing 10× cost per MTU-1500 TCP frame,
             that's still ~24 MB/s — three orders of
             magnitude above the observed floor. Switch + IP
             responder are not the bottleneck.
           - `internal/sandboxnet.BenchmarkForwarderTCPThroughput`
             wires two gvisor netstacks back-to-back over a
             `net.Pipe` (host + guest), dials from guest
             through host's TCP forwarder + NAT to a real
             127.0.0.1 listener, and streams bytes. Steady
             state across chunk sizes 1K/4K/32K/128K:
             25 / 38 / 30 / 32 MB/s. The forwarder path —
             gvisor TCP endpoint, NAT lookup, `net.Dial`,
             `tcpproxy.HandleConn` — is not the bottleneck
             either; it floors ~500× above the bun-test
             observation.

           - `TestClusterWorkspaceLoopbackThroughput`,
             `TestClusterWorkspaceLANThroughput`, and
             `TestClusterWorkspaceLatencyThroughput`
             (integration, all gated on
             `DAFT_RUN_THROUGHPUT_EXPERIMENT=1`) run the full
             real-VM path the synthetic bench couldn't cover:
             Linux-kernel-TCP in the VM → CH virtio-net → vsock
             to sandboxd → pump → gvisor netstack → real host
             listener serving 32 MiB. The variants differ in
             the host-side listener address and whether netem
             latency is injected on `lo`:
              - **loopback**: 127.0.0.1, kernel TCP fast-path.
              - **lan**: host's external IPv4 (scanned at
                runtime; `DAFT_THROUGHPUT_LAN_IP` overrides).
                gvisor's `net.Dial` targets a
                non-`127.0.0.0/8` IP so the host kernel uses
                the full IP/TCP TX path, not the loopback
                fast-path.
              - **latency50ms**: LAN setup plus
                `sudo tc qdisc add dev lo root netem delay
                50ms` injected after cluster bring-up. Tests
                gvisor's TCP under WAN-realistic RTT —
                continent-to-continent paths run ~50–150ms.
                Skips when passwordless `sudo tc` isn't
                available.

             Results: **142 MiB/s loopback, 144 MiB/s LAN,
             14.2 MiB/s at 50ms RTT.** The first two are
             within 1% of each other — kernel loopback
             fast-path was not hiding a real cost.

           What's *proven* by the four experiments + three
           variants:
            - The local end-to-end path (no upstream-internet
              variable) delivers ~1.1 Gbit/s. No layer this
              codebase owns is the bottleneck at low RTT.
            - At a WAN-realistic 50ms RTT, gvisor still
              delivers 14.2 MiB/s = ~112 Mbit/s. That's a
              10× drop from low-RTT but **still 280× faster
              than the bun-test 50 KB/s observation**.

           What's *not* proven and was previously claimed as
           if it were (corrected 2026-05-23): the bun-test
           50 KB/s is **not** explained by gvisor TCP being
           "BDP-limited at high RTT". 14.2 MiB/s at 50ms is
           way too fast for that story to fit; gvisor's
           receive window auto-tuning evidently works at
           least up to ~720 KB BDP. The honest situation
           is we don't yet have a mechanism that explains
           the 50 KB/s observation. Plausible candidates
           still in scope:
            - The 50 KB/s observation may not be steady-state
              single-stream TCP throughput. It may be the
              aggregate slowness of many small sequential
              fetches (TLS handshake + DNS per closure
              path), in which case "per-stream throughput"
              isn't the right metric.
            - RTT to `cache.nixos.org` from this host may be
              much higher than 50ms (CDN-dependent), or the
              path may have meaningful packet loss that
              caps cwnd via Reno.
            - gvisor's congestion control (Reno vs CUBIC)
              and lack of GSO/TSO/GRO may compound on a
              lossy WAN path beyond what netem-on-lo
              models (netem adds delay but not loss).

           RESOLVED (2026-05-24) — the failure is **memory, not
           network**. `cmd/daft-mitm-proxy` (a host-side HTTPS-MITM
           forward proxy: generates a CA at startup, MITMs CONNECT,
           logs `<ts> <method> <url> -> <status> bytes=<n>
           elapsed=<d>`, makes the real WAN call from the host) was
           wired into `TestClusterWorkspaceBunDevServer` via
           `DAFT_MITM_PROXY=1` (in-VM nix points at
           `https_proxy=http://192.168.127.254:PORT` +
           `NIX_SSL_CERT_FILE=/workspace/mitm-ca.pem`, CA dropped in
           the workspace mount). Two proxy fixes were needed to make
           it a usable probe: follow redirects internally (nix's
           flake fetch hits `github.com/.../archive/<rev>.tar.gz`
           which 302s to codeload; a bare `Transport.RoundTrip`
           hands the 302 back and nix's libcurl can't follow it
           through the MITM, leaving the tree empty) and *don't*
           force `NIX_REMOTE=` (local-store mode stalls this image,
           which expects the daemon).

           What the proxy log showed across runs:
            - `GET api.github.com/.../commits/nixos-25.05` → 200,
              3.7 KB, ~230 ms (latency-bound tiny request).
            - `GET codeload.github.com/.../tar.gz/<rev>` → 200,
              **44.8 MB in ~1.3–1.4 s = ~30–35 MiB/s** through the
              *entire* VM stack + MITM. Egress is fast; the
              "50 KB/s" was never the real story.

           nix never reached `cache.nixos.org` at all. Immediately
           after the tarball download the guest kernel logged
           `nix invoked oom-killer` /
           `Out of memory: Killed process (nix)
           total-vm:393448kB, anon-rss:178244kB`, with the guest
           reporting `65439 pages RAM` ≈ **256 MiB**. nix is
           OOM-killed mid-`nixpkgs` evaluation; the test then sits
           the full 15-minute cap waiting for a readiness line that
           can never come.

           Root cause + exact location: workspace VMs are
           hardwired to the 256 MiB default.
           `internal/sandboxexec/exec.go:445` `baseBootConfig`
           only returns `e.base` (which carries `DAFT_MEMORY` from
           `sandbox.ConfigFromEnv`) on the **`imageRef == ""`**
           smoke-test fallback; every real dispatch carries an
           image ref, so it calls the assembler, whose
           `internal/sandboxboot/sandboxboot.go:155` builds a fresh
           `sandbox.Config` **with no `Memory` field** →
           `defaultMemory = "256M"`. `DAFT_MEMORY` therefore never
           reaches a workspace VM (confirmed: a `DAFT_MEMORY=4G`
           run still booted 256 MiB and OOM'd identically). The
           standalone `daft-sandboxd run` path
           (`internal/sandboxdcli/run.go:607`) has the same gap.

           Fix landed (2026-05-24): per-workspace VM sizing.
           Added `memory_bytes` + `cpu_cores` to `SandboxSpec`
           and `WorkspaceSpec`, a `SetResources` workspace RPC, a
           `daft workspace resources --memory 4G --cpus N` CLI
           command, and `applyResourceSizing` in
           `internal/sandboxexec/exec.go` — which projects the
           spec onto the boot config and, for image-backed
           dispatches with no explicit CPU request, defaults
           vCPUs to half the host's logical cores (min 1) instead
           of the bare 1-core `sandbox.Config` default. Memory
           still defaults to 256 MiB when unset; workloads opt up.
           Verified: with `--memory 4G --cpus 4` the bun VM no
           longer OOMs (256 MiB OOM-killed nix deterministically
           at nixpkgs eval; 4 GiB does not).

           SECOND bottleneck, still open: even with the OOM gone,
           `TestClusterWorkspaceBunDevServer` still times out at
           the 15-min cap. nix goes silent right after
           "unpacking … into the Git cache" / the codeload
           tarball download (which itself clocks 7–35 MiB/s
           through the proxy — fine) and never emits another
           `-vv` line. No OOM, no error. The ~14-min silent phase
           is the nixpkgs git-cache unpack + store import +
           evaluation — filesystem/CPU work over the VM's
           virtiofs-backed store, not network. This is squarely
           **slice 3** (host-mounted persistent `/nix/store`) /
           a virtiofs-small-file-throughput investigation; the
           memory fix was necessary but not sufficient.

           Remaining (lower-priority, network-shaped) probes, now
           that the immediate blocker is understood:
            - Add a `loss=N%` variant to netem to test
              loss-recovery behaviour at WAN RTT.
            - To observe the `cache.nixos.org` closure fetch through
              the proxy, the **nix-daemon** (not our exec'd client)
              must inherit `https_proxy`/`NIX_SSL_CERT_FILE`; it's
              started by the image entrypoint, so this needs an
              entrypoint/daemon-env change rather than exec env.

           Regardless of where the bun-test root cause
           ultimately lands, **slice 3 below** (host-mounted
           persistent `/nix/store`) is still the right
           pragmatic unblocker for slice 2: the closure
           lives on the host's filesystem, the VM mounts it
           read-only, no fetch happens at all.
        3. **host-mounted persistent `/nix/store`** (promoted
           from deferred, 2026-05-23, after the throughput
           experiment ruled out an in-stack root cause for
           the bun-test 50 KB/s observation). Host-side
           `nix copy` once, mount the resulting store
           read-only into the VM via virtiofs, the in-VM nix
           sees its closure paths already populated and skips
           the cache.nixos.org fetch entirely. Slice 2's
           bun dev-shell becomes a launch + run, no network.

- D7: **Rootless storage translation** — bridge the
  CAP_CHOWN gap between the sandbox-host process and the
  container's view of the bundle / workspace.

  Background. Slice 1 (landed, 9b1202e) wraps daft-sandboxd in
  its own user namespace via `newuidmap`/`newgidmap` at startup
  so the rest of the slice plan ("rootless agent" in the
  privilege model section) can assume CAP_SYS_ADMIN + CAP_CHOWN
  within a non-init userns. Slice 2 (landed, 4135e0f) dropped
  the per-bundle OCI userns wiring now that the container can
  share sandboxd's userns directly. Slice 3 (landed) cleaned up
  the idmap-mount scaffolding that didn't work before the
  re-exec.

  The remaining gap: virtiofsd inherits sandboxd's userns
  (CAP_CHOWN scoped to it), but the bundle files live on the
  host fs in *init*'s userns. The kernel checks CAP_CHOWN
  against the userns owning the inode, so virtiofsd's chowns
  to subuid-range targets EPERM even though the kernel happily
  translates the gid via sandboxd's gid_map. Concretely: nix
  daemon's chown of `/nix/store` to gid 30000 (nixbld) fails
  the moment a fresh container touches the store. Bun
  dev-shell (D6c.3 slice 2) currently sidesteps this with
  `--option build-users-group ""` — fine for a dev-shell, not
  fine for any workload that actually builds derivations.

  Options to evaluate (none landed):
    1. **Sandboxd-private tmpfs bundles**. After re-exec
       sandboxd unshares CLONE_NEWNS, mounts a fresh tmpfs at
       a fixed location, and unpacks bundles into it. The
       tmpfs's superblock userns is sandboxd's userns, so
       CAP_CHOWN over its inodes works without idmapped
       mounts. Bound by RAM, but bundles in practice are
       small (~hundreds of MB) and the workspace mount is
       separate. Lowest-risk path.
    2. **Idmapped mounts on the bundle share**. Kernel's
       `mount_setattr(MOUNT_ATTR_IDMAP)` requires CAP_SYS_ADMIN
       in the *initial* userns regardless of where the mount
       lives, so this needs a small setuid helper or systemd
       socket-activated mount-helper. More moving parts.
    3. **virtiofsd `--uid-map` with a non-zero euid**.
       Virtiofsd rejects `--uid-map` when euid==0 (its sandbox
       layer expects to set up its own userns via newuidmap).
       Drop sandboxd's spawned virtiofsd to inside-uid 1 first;
       virtiofsd then does its own newuidmap dance with a
       single contiguous range. Per virtiofsd's idmap code the
       map is single-range only, so the OCI side needs to
       agree on one contiguous mapping; works for v0 but
       constrains future multi-uid-per-container designs.
    4. **FUSE translator in front of virtiofsd**. Write a
       daft-fuse passthrough that translates uids at the FUSE
       boundary. Gives us full control of the policy
       (per-mount, per-container) but is the most code.

  Slice plan when picked up:
    - D7.4: spike each of the four options enough to learn
      which the kernel actually permits in our environment;
      bun-dev-shell + a nix-daemon-running-build smoke test
      decide.
    - D7.5: land the chosen approach; drop the
      `--option build-users-group ""` workaround from
      D6c.3 slice 2.
    - D7.6: PLAN.md "Privilege model" section updates to
      reflect the actual mechanism (currently sketches a
      `newuidmap`-free model — that was the goal before we
      learned the CAP_CHOWN boundary; reconcile or rewrite).

- D8: **Diagnostic fabric — workload-agnostic
  introspection from test harness through to the in-VM
  kernel**.

  Motivation. The bun-test investigation that fixed CONFIG_TUN
  + landed D7.2 spent most of its time on archaeology
  (extracting a single line from a temp-dir log surviving a
  timeout panic) instead of on the actual bug. Slice 1
  (`testLogTee`, landed 754912c) closes the worst gap by
  fanning subprocess output into `t.Log` as lines arrive, so
  the failure report shows what the workload printed
  regardless of how the test died. The rest of the slice plan
  is the rest of the same fabric: every layer that knows
  something useful should be feeding into one place when a
  test fails.

  Slices, ordered cheapest-first:
    1. **landed** — `testLogTee` in
       `internal/testdaemons/workspace_forward_test.go`. Any
       `startDaftSubprocess` now streams stdout+stderr into
       the test's log buffer line by line.
    2. **Cluster output → t.Log**.
       `internal/testdaemons/cluster.go`'s pane prefixer
       (`[daft-sandboxd] …`) already captures stderr from the
       control-plane daemons. Make it use the same tee
       pattern so framework-timeout panics also surface that
       output, not just the daft-subprocess output.
    3. **Persistent capture-on-failure**. On any
       integration-test failure, copy the cluster's
       `t.TempDir()` contents to
       `/tmp/daft-test-failures/<test>-<ts>/`. Stable
       location, post-mortem-safe — no race with go test's
       tempdir cleanup. One `t.Cleanup` at cluster level,
       skipped on success.
    4. **virtiofsd `--log-level info`**. Default in tests,
       captured to a path under the cluster tempdir. Surfaces
       every FUSE-layer EPERM at the source of truth.
    5. **crun `--debug`**. Same pattern — agent passes
       `--debug` to crun when a `DAFT_DEBUG_OCI=1` env var is
       set, captures stderr to the per-container log. OCI
       lifecycle failures land in the timeline.
    6. **Agent-side `/proc` snapshotter**. OCI prestart hook
       writes `/proc/self/{status,mountinfo,uid_map,gid_map}`
       into `/workspace/.daft/container-<id>.snap`. Workspace
       mount is host-visible and outlives the VM, so the
       black-box state of the container is captured even when
       the VM doesn't come up cleanly.
    7. **VM ftrace channel**. Enable
       `CONFIG_FTRACE_SYSCALLS=y`, `CONFIG_KPROBE_EVENTS=y`,
       `CONFIG_UPROBE_EVENTS=y`, `CONFIG_HIST_TRIGGERS=y` in
       `third_party/kernel-config/daft.config`. Agent reads
       `/sys/kernel/tracing/trace_pipe` selectively (filter
       enabled by `DAFT_TRACE_SYSCALLS` env on the kernel
       cmdline) and frames events on a new
       `daft.trace_port=…` vsock channel. Host writes them
       into the unified timeline. Workload-agnostic
       eBPF-class visibility at ~150 LoC of agent code +
       kernel config.
    8. **Unified timeline merger**. Sandboxd-side aggregator
       that interleaves trace spans + virtiofsd + crun + the
       ftrace stream + cluster captures in chronological
       order. Exposed to tests as one stream; on failure,
       dumped alongside the captured tempdir.

  Property the fabric upholds: any failure mode of any
  workload running in any container is at-most-one-grep away.
  Production keeps the same hooks behind feature flags so
  field debugging is the same shape as test debugging.

- D5: **Multi-container per VM** (was the old D2). daft-agent
  vsock control stream: agent accepts `RunContainer(spec)` /
  `StopContainer(id)`, runs N crun children, isolates each by
  user-namespace subuid range and a per-container subpath of
  `/workspace`. Only needed for GPU amortisation (siblings
  sharing a GPU-bound VM) — pushed behind D2/D3/D4 because the
  CPU workflow is one-container-per-Sandbox by design.

Phase E (deferred) — **Image build in VM (Ayup-style)**:

Descoped 2026-05-17 for the CPU reference workflow: a nix-based
container image with bash as PID 1 is sufficient, and the user
runs `nix develop` / `nix run` interactively via D2 attach. A
dedicated server-side image build pipeline only becomes
necessary if a workflow can't pre-build its image
(user-host-side `nix build` + push to registry, or CI). Kept
here as a sketch in case that workflow appears.

- E1: `daft workspace image --dockerfile <path>` packages
  build context as a transient workspace; workspace image is
  set to `pending-build:<uuid>`.
- E2: Orchestrator schedules a buildkit-VM on a sandbox-host
  with that workspace mounted as the build context.
- E3: Build pushes into the orchestrator registry; orchestrator
  flips the workspace's `image` field to the new digest;
  workspace becomes launchable.
- E4: Build caching: orchestrator-side buildkit cache stored
  in the CAS layer.

Phase F — **Adoption + locks**:
- F1: `daft workspace adopt <name-or-uuid>` walks the spec,
  prompts for each mount's local mapping or `--skip`.
- F2: Per-user-host mount overrides (skip / pull-only /
  push-only / remap), persisted client-side under the
  orchestrator's identity.
- F3: Lock service: `Acquire`, `Release`, `--force` (with an
  audit-log entry on the orchestrator), auto-release on
  heartbeat timeout.

Phase G — **Freeze-on-disconnect** (subsumes Slice 4c):
- G1: Wire daft-fuse `Freeze` decision to QMP `stop` (the
  current Slice 4c).
- G2: Disconnect detection on the user-host's control channel
  promotes daft-fuse policy from "Allow" to "Freeze" on the
  active workspace until the channel reconnects. EAGAIN ceases
  to be a user-visible symptom — the VM stops instead of
  serving stale errors.
- G3: Thaw protocol: when the control channel reconnects,
  orchestrator sends `Thaw(workspace=<uuid>)` to the
  controller, which `cont`s the VM via QMP.

Phase H — **Docker-API endpoint for third-party sync tools**:

Rationale: rather than build our own two-way file-sync (with all
the rsync-delta / 3-way-merge / conflict-resolution surface that
implies), expose enough of the Docker Engine API at a known
endpoint that an unmodified Mutagen client on the user-host can
target a workspace as `docker://daft-<workspace>` and use its own
agent for sync. The user gets full bidi-sync ergonomics from a
tool they already know; we don't add a symmetric model that
fights our "user-host is source of truth" stance everywhere else.
This is also generic — anything that speaks docker (VS Code Dev
Containers, JetBrains Gateway, plain `docker cp`) works through
the same endpoint.

- H1: Minimal Docker API surface on the orchestrator,
  multiplexed per-workspace. The endpoint speaks just the
  subset Mutagen and similar tools touch: `GET /containers/json`,
  `GET /containers/{id}/json`, `POST /containers/{id}/exec`
  (+ `start` + WebSocket attach), `PUT /containers/{id}/archive`,
  `GET /containers/{id}/archive`. Backend is a forwarder over
  the broker into the sandboxd, which exec's against the actual
  container (crun) inside the VM. Auth: orchestrator's existing
  mTLS; the user points `DOCKER_HOST` at
  `https://<workspace>.<orchestrator>/docker` and supplies the
  client cert.
- H2: Mutagen-on-daft happy path: doc + integration test
  demonstrating `mutagen sync create ~/myapp
  docker://daft-<workspace>:/workspace` against a launched
  workspace. Mutagen's docker transport will `docker cp` its
  agent in and `docker exec` it; the agent then drives sync
  through that long-lived exec session.
- H3: Sync-direction guardrails. Two-way sync to an adversarial
  sandbox violates our default trust model. Options to land
  before H1 is broadly recommended: (a) refuse the exec if the
  workspace's mount policy is `pull-only` for the targeted path,
  forcing `mutagen sync create --mode one-way-replica`; (b)
  expose a `daft workspace allow-bidi-sync <mount>` opt-in that
  flips the per-mount policy explicitly. Default-deny on bidi.
- H4: Optional convenience: `daft workspace docker-host
  <workspace>` prints the `DOCKER_HOST=…` env line + cert paths
  so the user doesn't have to remember the URL shape.
- H5: Mutagen config import (small): `daft workspace import-mutagen
  <yaml>` translates ignore patterns, `mode`, and per-path
  overrides into mount-flag overrides on the workspace. Convenience
  only — anyone with an existing mutagen project can keep using
  the same `.mutagen.yml`.

Notes:
- We do NOT build symmetric / two-way / 3-way-merge file sync
  in daft itself. Workspace mounts stay asymmetric (push-only
  / pull-only / explicit `daft workspace pull`); users who want
  bidi reach for Mutagen via H.
- The Docker-API surface is intentionally minimum-viable —
  enough for sync and exec, not a full re-implementation. Things
  like `image build` stay daft-native (Phase E). If a tool
  asks for an unsupported endpoint we 501 it with a pointer to
  the daft equivalent.

### What survives, what moves

- **Survives**: broker (relay), daft-fuse, overlayfs, mTLS
  identity, registry, CAS, manifest format, FastCDC, tracing,
  image bundle refcount, privilege model, microVM stack.
- **Moves**: workspace metadata (sandbox-host → orchestrator);
  invocation control (sandbox-host CLI → orchestrator dispatch);
  port exposure (none → orchestrator ingress); image build
  (none → buildkit-VM); freeze trigger (policy-only →
  policy-or-disconnect).
- **Deprecated** (kept as `debug` for e2e): `daft-sandboxd run`,
  `daft-sandboxd workspace serve`, the `--workspace-from` /
  `--workspace-broker` / `--workspace-attach` /
  `--workspace-overlay` / `--keep-workspace` flag set. Replaced
  by orchestrator dispatch driving the same code paths.

### Open questions for this re-orientation

- **Workspace identity across hosts**: use `<owner-fp>/<name>`
  everywhere, or mint a UUID at create time and let names be
  per-user-host aliases? UUID seems necessary once adoption is
  real — `name` collisions across owners are inevitable.
- **Dispatch granularity**: SandboxSpec as a one-shot RPC vs a
  continuously-mutable workspace state mirrored to the sandbox?
  *Resolved in B4 by the resource model*: a Workspace edit bumps
  `metadata.generation`; the running Sandbox is immutable (snapshot
  of `Workspace.spec` at Launch time); re-applying an edit means
  a new Launch creating a fresh Sandbox. UI shows
  `observed_generation < generation` as "edit pending restart".
- **K8s integration path**: containerd shim (Path 1), Virtual
  Kubelet (Path 2), or direct CRI (Path 3)? Not in scope until
  a real user pulls — but the resource model in B4 is K8s-shaped
  so any of the three stays cheap. See
  [ARCHITECTURE.md](ARCHITECTURE.md#kubernetes-integration-paths).
- **Port allocation**: per-workspace subpath under one shared
  TLS vhost, or per-workspace hostname with a wildcard cert?
  Subpaths simpler for v1; hostnames needed once apps care
  about their `Origin` / cookies.
- **Lock semantics on adopt**: does adopt take the lock
  automatically, or require an explicit lock? Probably explicit
  + warn if the previous owner is still alive.
- **GPU isolation within a shared VM**: NVIDIA driver fairness
  is FIFO; do we expose a queue or rely on cgroups + per-uid
  scheduling? Defer to D2.
- **Dockerfile build escape hatch**: if no sandbox-host is
  free, do we build on the user-host with their docker? Probably
  not — keeps the "everything reproducible inside daft" story.
- **dev-defaults set**: `~/.config/git`, `~/.claude`,
  `~/.ssh` (read-only), `~/.gitconfig`, and an env-var subset
  (`GH_TOKEN`, `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`) — what
  goes in, what doesn't? Conservative default + `--with-dev-
  defaults` opt-in; explicit catalog in CONTRIBUTING.
- **Docker-API surface scope** (Phase H): which endpoints stay
  501 vs implemented? Sync + exec are the must-haves; `logs`,
  `stats`, `events` are useful but optional; `build`, `pull`,
  `images` are explicit no-ops with a `daft …` pointer. Final
  list belongs in H1 once we audit what Mutagen / Dev Containers
  / JetBrains Gateway actually call.
- **Bidi-sync default** (Phase H3): default-deny with an explicit
  opt-in (`daft workspace allow-bidi-sync`) seems right, but
  the UX hit on "I just want to use mutagen" is real. Re-evaluate
  once H2 has an end-to-end demo.

## Status

- [x] Repository skeleton (Go layout, Nix flake, direnv, git hooks)
- [x] CONTRIBUTING.md
- [x] `daft` CLI scaffolding (cobra-based, with `completion` and `version` subcommands)
- [x] GitHub CI (`validate-build`, `validate-tag`)
- [x] Define project domain and scope (what does `daft` actually do?)
- [x] First feature slice — `daft sandbox hello` boots a custom kernel +
      cpio initramfs in Cloud Hypervisor and captures the daft-agent's
      banner from the serial console. Cold boot ~300 ms.
- [x] All shipped binaries statically linked (Go via CGO=0, Cloud
      Hypervisor via static-musl). Single-executable `daft-sandboxd`
      bundles VMM + kernel + initramfs (~23 MiB).
- [x] u-root-based init: `cmds/core/init` runs as PID 1, mounts /proc,
      /sys, devtmpfs, cgroup2, then execs daft-agent via /bin/uinit.
      Initramfs is assembled by the `u-root` command driven from the
      Nix flake.
- [x] Performance regression gate: `make bench` writes a ns/op baseline
      to `bench.txt`; `make bench-check` (run by the pre-commit hook)
      fails on >2.00× regressions and warns on >1.10×. Includes a
      `BenchmarkHelloBoot` e2e boot-time benchmark.
- [x] Host → guest file sharing (v0): virtiofsd vendored as submodule
      (third_party/virtiofsd, v1.9.0) and bundled into daft-sandboxd.
      `Config.Share` causes Hello() to spawn virtiofsd alongside CH;
      the in-VM agent reads `daft.share=<tag>:<mp>` from `/proc/cmdline`,
      mounts the share, reads `<mp>/hello.txt`, and reports its
      contents on the console with `share: <payload>`. Fuzz target
      `FuzzParseShareCmdline` covers the cmdline parser; mock-based
      unit tests cover the orchestration;
      `BenchmarkHelloBootWithShare` and `TestHelloIntegrationBootsVMWithShare`
      cover the e2e path (real VM, real virtiofsd).
      - **Hardening**: virtiofsd is built static-musl against
        `pkgsStatic.libcap_ng` + `pkgsStatic.libseccomp`. Every
        binary in the daft-sandboxd bundle is now self-contained.
- [x] Container in the VM (v0): crun vendored at third_party/crun
      v1.9.2, built static-musl, bundled into the initramfs at
      `/bin/crun`. A new in-repo binary `daft-hello-container` plus
      a hand-rolled `contrib/hello-bundle/config.json` produce a
      Nix-built OCI bundle (`.#hello-bundle`). `Config.Bundle` on the
      sandbox API surfaces a `daft.bundle=<path>` token on the kernel
      cmdline; the agent parses it after the share, execs
      `crun run --no-pivot --bundle <path> daft-container`, and
      forwards the container's stdout+stderr line-by-line with
      `container: <…>` prefix on the shared console.
      `TestHelloIntegrationBootsVMWithContainer` and
      `BenchmarkHelloBootWithContainer` cover the e2e path; mock
      tests + a fake-crun TestMain self-exec cover unit-level
      branches; `FuzzParseBundleCmdline` joins the cmdline-parser
      fuzz family. Container tax measured at ~10 ms over the
      share-only baseline (343→353 ms cold-boot).
      - **Note**: `--no-pivot` is needed because the bundle is
        delivered through the share, so the rootfs is a sub-dir of a
        virtiofs mount rather than its own mountpoint. Acceptable
        for v0 — the VM is the security boundary, not the container.
- [x] OCI image pull on the sandbox-host: new `internal/imagestore`
      package wraps go-containerregistry. `Store.Pull` resolves a
      ref, sums layer sizes, allocates against a quota (default
      `imagestore.DefaultQuota` = 50 GiB), evicts LRU images when a
      pull would overflow, and refuses up-front if a single image
      is larger than the entire quota. `Store.Bundle` flattens
      cached layers into a runnable OCI bundle (config.json built
      from the image's Entrypoint/Cmd/Env/Cwd plus our default
      mount/namespace template). Bundle directories are
      refcount-locked so a running VM's bundle can't be evicted.
      `daft-sandboxd` gains `pull`, `images ls`, `images rm`,
      `images prune --older-than DUR`. Quota and store root via
      `DAFT_STORE_QUOTA` (accepts K/M/G/T suffix) and
      `DAFT_STORE_DIR`. Tests use go-containerregistry's
      in-process registry handler so pull/bundle paths exercise
      the real crane code without a network. Fuzz target:
      `FuzzParseBytes`. Coverage held at 90% baseline via real
      registry fixtures + targeted error-path tests (write-error,
      malformed meta.json, locked-bundle eviction refusal).
- [x] Per-file coverage gate: `scripts/cover-check.sh` enforces
      ≥80% per non-test file (>10 statements) on top of the
      existing 1pp total-drop limit. Catches the case where
      total coverage stays high while one file is dark.
- [x] E2E tests run on every commit. Behind a `linux,integration`
      build tag (target name is historical — see CONTRIBUTING.md
      "Test categories" for the unit / integration / e2e
      taxonomy; these are e2e by that taxonomy because they boot
      a real Cloud Hypervisor against `/dev/kvm`). Artefacts auto-
      resolve from `result-*` symlinks (env-var override
      available). No skips: missing artefact → `t.Fatalf`,
      missing `/dev/kvm` → `t.Fatalf`. Linux + KVM is a hard
      commit prerequisite; environments without them are not a
      use case we support.
- [x] Project ethos codified in CONTRIBUTING.md: correctness +
      security + perf > shipping speed; reduce scope, never
      correctness; verification monotonically increases. Coverage
      baseline only ever rises (cover-check.sh refuses to write
      a lower value); strict ≥-baseline gate (no 1pp tolerance).
      `--no-verify` is never OK.
- [x] Static analysis tightened: `errcheck` (was already on),
      `errorlint`, `nilerr`, `nilnil`, `bodyclose`, `contextcheck`,
      `gosec`, `gocritic`, `prealloc`, `copyloopvar`, `dupword`,
      `misspell`, plus the existing complexity ceilings. Lint
      surface is itself monotonic — see `.golangci.yml`.
- [x] Zero error discards in production code. `internal/oops`
      provides `Log` (write to sink, exit if sink fails), `Skip`
      (allowlist of expected errors; everything else is `Must`),
      `Must` (log + exit). Every `_ = f()` and `_, _ = f()` in
      production has been replaced by either a propagated error,
      a logged-and-continued case via `oops.Log`, or a known-benign
      skip via `oops.Skip` with an explicit allowlist. The agent's
      console-write helper exits the process if the kernel console
      is gone. Cleanup paths surface unexpected `EPERM`/`EROFS`/
      `EIO` etc. instead of walking past them.
- [x] `daft-sandboxd run <image-ref> [cmd…]` closes the loop:
      pulls the image into the local store (LRU under quota),
      bundles it, locks the bundle for the VM lifetime, shares
      the bundles dir into the guest via virtiofs, and crun-runs
      the container. Container stdout+stderr stream back as
      `container: <…>` lines on the host's terminal. Optional
      `--workspace ./dir` flag adds a second read-write virtiofs
      share mounted at `/workspace` inside both the VM and the
      container — files written by the container persist on the
      host. Multi-share API throughout: sandbox.Config.Shares is
      a slice; agent's ParseShareCmdlines walks every daft.share=
      token; cloud-hypervisor gets one --fs flag per share; one
      virtiofsd process per share. Marker-read of hello.txt is
      skipped automatically when Bundle is set, since container
      output is the proof of life.
- [x] Build output now lands under `bin/` rather than the repo
      root, so a dev `make build` can't be confused with a
      release-grade `nix build .#daft-sandboxd`.
- [x] mTLS identity primitives (`internal/identity`): Ed25519
      keypair + self-signed leaf cert (10y, ClientAuth+ServerAuth
      EKU, CN `daft-<role>-<rand>`); atomic Save/Load to
      `<dir>/cert.pem` + `<dir>/key.pem` with 0o600 perms;
      `Fingerprint` = SHA-256 of the cert's SPKI, rendered as
      `sha256:AA:BB:...` (stable across cert rotation while the
      key is the same); JSON-friendly `Fingerprint` (text
      marshaller); `PeerStore` is the on-disk TOFU set, atomic
      JSON file rewrite. Fuzz target: `FuzzParseFingerprint`.
      `identity.ServerTLSConfig` / `identity.ClientTLSConfig`
      build matched mTLS templates whose `VerifyPeerCertificate`
      and `VerifyConnection` look up the peer's leaf-cert SPKI
      fingerprint in the local `PeerStore`; session resumption is
      disabled so every handshake re-runs the trust check (gosec
      G123). Slice 1 of the mTLS skeleton.
- [x] Connect-RPC over mTLS — orchestrator skeleton. Proto
      pipeline lands: `proto/health/v1/health.proto`, `buf.yaml`,
      `buf.gen.yaml`, codegen plugins (`protoc-gen-go`,
      `protoc-gen-connect-go`) pinned via Go 1.24+'s `tool`
      directive in `go.mod`, `make proto` regenerates code under
      `internal/genpb/`, `buf` itself ships in the Nix dev shell.
      `internal/orchestrator` exposes a `Health.Ping` Connect-RPC
      endpoint over HTTP/2 + mTLS, with `VerifyPeerCertificate`
      gating connections via `PeerStore`. New
      `cmd/daft-orchestrator` binary serves it: subcommands
      `version` and `serve --addr H:P --config-dir DIR`,
      auto-generates an Ed25519 identity on first run under
      `$XDG_CONFIG_HOME/daft/orchestrator/`. Integration tests
      (`internal/orchestrator/server_test.go`) cover the
      approved/unapproved/unknown-server matrix. Slice 2 of the
      mTLS skeleton; client-side `daft auth approve` lands next.
- [x] Slice 3: client-side mTLS. `daft auth show/list/approve/remove`
      subcommands manage the local TOFU peer set under
      `$XDG_CONFIG_HOME/daft/client/`; first run autogenerates an
      Ed25519 client identity (cert.pem + key.pem, 0o600) the same
      way the orchestrator does. `daft ping <addr> --message=X
      --timeout=...` rounds-trips a Health.Ping over HTTP/2 + mTLS
      to verify both sides of the trust loop in one command.
      Identity bootstrap is now a single shared function
      (`identity.LoadOrGenerate`) used by both daft and
      daft-orchestrator, so the trust state machine has one
      implementation to audit. Tests cover approved /
      unapproved-server / unapproved-client / corrupt-identity /
      corrupt-peer-store branches end-to-end via an in-process
      orchestrator.
- [x] Tracing: daft-native cross-component tracing. **Slice 1
      landed**: `internal/trace` package with Span / Tracer / ring
      buffer / Connect interceptors / abandoned-span reaper. Errors
      attach to the active span via `trace.RecordError(ctx, err)` /
      `trace.Errorf(ctx, fmt, args...)` (Ayup's `terror` pattern,
      ours speaks our own span library instead of OTel). Wire format
      `proto/trace/v1/trace.proto` defines Span +
      `TraceService.ReportSpans`. `daft ping` wraps its RPC in a
      span, propagates trace context via the `Daft-Trace-Id` /
      `Daft-Parent-Span-Id` headers, and ships the local buffer to
      the orchestrator on success. The orchestrator side has a
      TraceService handler ingesting into a central Tracer plus a
      `/debug/traces` HTTP endpoint that renders text or JSON
      (Accept-driven). Test integration: `trace.LogOnFailure(t, tr)`
      registers a Cleanup that dumps the snapshot via `t.Logf` on
      failure — open spans are abandoned first so a hung span
      surfaces in the dump. Tests can plant a Tracer on the context
      via `trace.ContextWithTracer` so they inspect spans without
      needing the orchestrator running. Reaper / abandonment
      semantic: spans not Ended within their deadline are emitted
      with `STATUS_ABANDONED` rather than silently lost; same for
      spans still active at `Tracer.Shutdown`.
      **Slice 3 landed**: `daft workspace pull` and `daft workspace
      serve` are fully instrumented. Pull wraps the request in a
      `daft.workspace.pull` root span (attrs: from / workspace /
      dest / preserve_mtime); the trace ClientInterceptor
      auto-spans every Connect call so each `GetManifest` and
      per-chunk `GetChunk` becomes a child span linked across the
      wire to the serve-side ServerInterceptor's matching span.
      `workspace.ApplyManifest` is now a span on its own (attrs:
      entries=N, dest), so per-chunk fetches sit underneath it in
      the tree. `trace.Start(ctx, name, opts...)` is a free function
      that locates a Tracer via `TracerFromContext` or falls back to
      the active span's tracer — library code uses it without
      caring how the caller wired the tracer up. End-to-end test
      `TestWorkspacePullTraceShape` asserts the resulting span tree
      shape (root + apply + per-chunk client/server pairs all share
      one trace_id). Component identifiers consolidated into
      `internal/component/names.go` so producer/consumer can't
      drift.
      **Sandbox path also instrumented**: `daft-sandboxd run`
      opens a root span; `imagestore.Pull` (attrs: ref, digest,
      size_bytes), `imagestore.Bundle` (attrs: digest, with
      "cached" event when reusing), and `sandbox.Hello` (attrs:
      kernel, shares, bundle) are spans. Hello fans out into
      `sandbox.start_virtiofsd`, `sandbox.run_cloud_hypervisor`,
      and `sandbox.scan_for_banner` so the cold-boot 310 ms
      breakdown surfaces in the trace. The sandbox e2e tests get a
      Tracer + LogOnFailure via the new `traceCtx(tb)` helper, so
      failed real-VM boots dump the per-stage timing breakdown
      automatically.
      **Slice 2 landed**: orchestrator-side `IngressReaper` watches
      the central Tracer's buffer; for each trace_id that has gone
      `quietAfter` without new spans it injects a synthetic
      `trace.quiesced` marker (StatusAbandoned when no root span
      ever arrived — the producer-crashed-mid-trace signal — else
      StatusOK). The reaper runs as a goroutine bounded to the
      Server's Serve/Shutdown lifetime via a private cancel/done
      pair on `orchestrator.Server`. Unit tests pin down the
      no-root/has-root branches with an injected clock + ID
      generator; an integration test ships an orphan child span
      via real ReportSpans, waits past the configured quiet
      window, and asserts the marker appears in /debug/traces.
      **Slice 4 landed**: vsock channel from the in-VM agent to
      the sandbox-host. `sandbox.Config.Vsock` selects a Cloud
      Hypervisor hybrid-vsock device (`--vsock cid=N,socket=PATH`);
      a `daft.vsock.trace_port=N` kernel cmdline token tells the
      agent which AF_VSOCK port to dial. Wire protocol is
      transport-agnostic length-prefixed `ReportSpansRequest`
      proto, implemented as `trace.StreamExporter` (producer side,
      one fresh conn per Flush) and `trace.StreamReceiver`
      (accept loop). The agent wires a Tracer with a vsock
      StreamExporter, emits an `agent.boot` span around the
      share+container setup, and Flushes *before* printing the
      banner so the host-side scan-for-banner doesn't race the
      VM teardown against the final write. Sandbox-host runs the
      StreamReceiver as a goroutine bounded by Hello's defer
      stop(); spans ingest into the Tracer reachable from ctx.
      Unit tests cover both halves over a UDS pair + net.Pipe;
      `TestHelloIntegrationShipsAgentSpansOverVsock` boots a real
      VM and asserts the `agent.boot` span lands in the host
      Tracer via real AF_VSOCK → CH hybrid-vsock → UDS proxy.
      **Slice 5 (optional)**: an OTel proxy-exporter as a separate
      component, translating our wire format to OTLP for users
      who want their existing observability stack to consume
      daft traces. Deferred — not load-bearing for daft's own
      operations.
- [/] Workspace sync: chunked, content-addressed snapshots
      flowing user-host → orchestrator → sandbox-host → VM, with
      lazy fetch via daft-fuse under virtiofsd. **Slice 1 landed**:
      proto data model (`proto/workspace/v1/workspace.proto`:
      Manifest / FileEntry / ChunkRef + WorkspaceService stubs);
      `internal/cas` content-addressed chunk store (sha256 +
      zstd, two-level fan-out, verifying-reader catches digest
      mismatch on read); `internal/workspace` BuildManifest walks
      a tree, chunks files (FixedSizeChunker for now; FastCDC
      swap-in next), records dirs/symlinks. Fuzz target on
      digest validation and path sanitisation.
      **Slice 2 landed**: `WorkspaceService` Connect-RPC handler
      (`internal/workspace.Server`) backed by the CAS plus an
      in-memory name → manifest registry; `ApplyManifest`
      reconstructs a tree at a destination path, fetching missing
      chunks lazily from the server, atomic .tmp+Rename per file.
      Reusable mTLS listener extracted to `internal/mtls.Server`
      so the orchestrator and the laptop's chunk server share one
      implementation. New CLI: `daft workspace serve --workspace
      name=path --addr ...` exposes a workspace; `daft workspace
      pull --from addr --workspace name <dest>` reconstructs it.
      End-to-end test runs serve+pull in one process.
      **Slice 2.5 landed (cross-host pull)**: `daft-sandboxd run`
      gained `--workspace-from <addr>` + `--workspace-name <name>`
      flags. On startup it dials the user-host's `daft workspace
      serve` over mTLS (with a fresh sandbox-host identity bootstrapped
      under `$XDG_CONFIG_HOME/daft/sandboxd`), pulls the manifest +
      chunks via `workspace.ApplyManifest` into a tmpdir, and
      virtiofs-mounts that tmpdir at `/workspace` inside the VM —
      the user's code lands inside the sandbox without leaving the
      sandbox-host filesystem first. Pre-fetch (not lazy fetch via
      FUSE) is the v0 model: a future `daft-fuse` slice will move
      this to lazy-on-read so multi-GB workspaces don't need to
      materialise on disk before the VM boots.
      **Slice 2.6 landed (pull-back)**: `daft-sandboxd run
      --keep-workspace` skips the post-VM tmpdir cleanup and prints
      its path; new `daft-sandboxd workspace serve --path=<dir>
      --workspace=<name>` exposes that dir back over mTLS so the
      user-host can `daft workspace pull` modifications back. New
      `daft-sandboxd auth show/list/approve/remove` mirror the daft
      client's auth subcommands, scoped to the sandboxd config dir,
      so the operator can manage the cross-host TOFU peer set on
      both ends without editing peers.json by hand.
      **Slice 2.7 landed (e2e + multi-share fix)**: real-VM
      round-trip test `TestWorkspaceRoundTripE2E` boots Cloud
      Hypervisor against `/dev/kvm`, runs a real container that
      reads `/workspace/in.txt` and writes `/workspace/out.txt`
      through a rw virtiofs share, then pulls the modified
      workspace back as the user-host. The test caught a real
      regression: `cloudHypervisorRunner.run` had been emitting
      `--fs A --fs B` for multi-share configs but clap rejects
      that — Cloud Hypervisor expects the variadic form
      (`--fs A B`). Production multi-share (e.g. `daft-sandboxd
      run --workspace`) had never been exercised on a real VM
      before this slice. The fix is in `buildCloudHypervisorArgs`,
      now a pure function with focused unit tests covering the
      0/1/N share cases so the regression can't reappear silently.
      **Slice 3 landed (FastCDC chunker)**: content-defined
      chunking via 64-bit gear-hash with normalized chunking
      (NC=2) — boundary mask popcount is log2(AvgSize)±2 so the
      realised chunk-size distribution is tighter around the
      target than vanilla CDC. Defaults: Min 256 KiB, Avg 1 MiB,
      Max 4 MiB. Gear table is generated deterministically from
      SplitMix64 with a fixed seed; a pinned sha256 in
      `TestGearTableStability` makes any accidental change fail
      loudly (changing the gear table silently invalidates every
      chunk boundary in existing stores). `FastCDCChunker` is now
      the default in `BuildManifest`; `FixedSizeChunker` is kept
      for tests and predictable-boundary workloads. Tested:
      determinism (same bytes → same digests), locality (mid-stream
      edit shares >= 50% of chunks), max-size enforcement on
      all-zero input, custom Min/Avg/Max, bad-config rejection,
      read/put error propagation, `FuzzFastCDCCut` (84k execs/s in
      pre-commit, no panics observed).
      **Slice 4a landed.** `internal/daftfuse` is the read-only,
      lazy-fetch FUSE library: a Manifest plus a `cas.Store` plus a
      `ChunkFetcher` gives a `Mount` with goroutine-safe `ReadAt`,
      Stat, Readdir, Readlink. Chunk misses fetch via the supplied
      `ChunkFetcher` (transport-agnostic — connect-RPC adapter is a
      caller concern), are de-duped by digest via `singleflight`,
      retried with exponential backoff (default 3 attempts, 50ms
      initial), and committed to the CAS via `PutCompressed` (so
      the existing digest verification stays the source of truth).
      The Policy interface (`OnRead` → Allow/Deny/Freeze) is wired
      end-to-end: Deny surfaces EACCES to the VM, Freeze surfaces
      EAGAIN (the QMP `stop` wiring is Slice 4c). Linux-only FUSE
      adapter (`fuse_linux.go`) ships behind a build tag; pure-Go
      tree + read logic stays in `tree.go` so unit tests don't need
      `/dev/fuse`. Coverage: 27 unit tests via mock-fetcher exercise
      every read/policy/retry branch + a real `fusermount` mount
      drives Read/Lookup/Readdir/Readlink/Stat/EACCES/EAGAIN/EIO on
      the kernel-facing side. No daft-sandboxd integration yet —
      that lands in 4b.

      **Slice 5a landed.** `workspace.DiffManifests(from, to)`
      compares two manifests entry-by-entry and returns a `Diff`
      with classified `Change`s — `Added` / `Removed` / `Modified`
      / `ModeChanged` / `TypeChanged`, sorted by path so identical
      inputs always produce byte-identical output. Mtime is
      deliberately ignored — it's best-effort metadata, not
      content. New CLI surface `daft workspace diff <local-dir>
      --from <addr> --workspace <name>` builds a local manifest of
      `<local-dir>` and prints the per-entry diff:
      `+`/`-`/`~`/`m`/`t` prefixes; `--detail` includes the mode
      transition and entry-kind transition on those lines. Direct-
      dial only in v1 (the broker-aware path mirrors `pull` and
      can be added when there's a driver). Pull's RPC setup
      consolidated into a shared `dialWorkspaceClient` helper.

      **Slice 4b landed.** `daft-sandboxd run --workspace-attach`
      mounts the workspace lazily via daft-fuse instead of
      pre-fetching every chunk. Requires either `--workspace-from`
      (direct mTLS) or `--workspace-broker` (NAT-friendly via the
      orchestrator) for transport; daftfuse.NewConnectFetcher
      bridges the GetChunk RPC to ChunkFetcher under the hood.
      The FUSE mount is exposed to the VM via virtiofsd as a
      regular share, so the container sees `/workspace` exactly
      as it does with the eager-pull path — chunks materialise
      on first read instead of at boot.
      Mutually exclusive with `--workspace` (no local-dir input)
      and `--keep-workspace` (chunks live in the CAS, no eager-
      materialised tmpdir to preserve). New
      `openBrokeredWorkspaceClient` helper consolidates the broker
      handshake shared by `pullWorkspaceBroker` and
      `attachLazyWorkspaceBroker`. Cleanup error-logs adopted
      `oops.Log` to fit the project's "zero error discards" rule.
      Non-linux stub of `daftfuse.Mount.Serve` returns an
      "unsupported" error so the daft-sandboxd package still
      compiles on darwin for IDE / cross-platform tooling.

      **Slice 5b landed.** `daft-sandboxd run --workspace-overlay`
      wraps the workspace virtiofs share in a tmpfs-backed
      overlayfs *inside the VM*. The agent honours a new
      `daft.workspace.overlay=<mountpoint>` kernel-cmdline token:
      the share is mounted at `<mp>.lower` instead of `<mp>`, a
      single tmpfs at `/run/daft/overlay` backs per-overlay
      `upper`+`work` dirs, and an overlayfs at `<mp>` merges them
      so the container sees a writable view layered on the
      read-only share. Container writes land in the in-VM tmpfs
      upper and vanish with the VM — the underlying share is
      untouched, exactly what's needed so `daft pull` can later
      surface the diff explicitly. The flag is paired with any
      workspace source (`--workspace`, `--workspace-from`,
      `--workspace-broker`, `--workspace-attach`); validation
      rejects it when there's nothing to overlay. Effectively
      mandatory with `--workspace-attach` since daft-fuse is
      read-only by construction. Real-VM e2e
      (`TestWorkspaceOverlayE2E`) boots the workload container,
      asserts the container banner is reached (write succeeded
      inside the VM) and that `/workspace/out.txt` never appears
      on the sandbox-host source directory (write was isolated).
      The "diff + pull back" half of "default local-only overlay,
      surfaced as a diff and pulled explicitly via `daft pull`"
      is deferred — Slice 5a already gives the diff primitive
      `workspace.DiffManifests`; a future slice will wire it to
      the upper-layer contents (vsock-served or virtiofs-shared
      upper) so the user-host can pull the changeset.

      **Remaining slices**:
      - Slice 4c: QMP `stop` enforcement on the `Freeze` decision
        — the policy engine signals the running VM to freeze
        rather than just surfacing EAGAIN to the workload.

      **Slice 3.5 design (orchestrator workspace broker + relay)**
      — *plan-only as of 2026-05-11.*

      *Why this slice exists.* Slice 2.5 ships a working but
      naive transport: sandbox-host dials the user-host's
      `daft workspace serve` on a static `host:port`. That
      fails the moment the user-host is a laptop behind NAT
      (the common case). Production needs the orchestrator on
      the discovery path — it's the only entity that knows
      about both peers — and, for v1, on the data path as a
      byte-relay. Direct dial stays available as an opt-in for
      LAN/dev setups; it doesn't gate Slice 4.

      *Conceptual direction.* The user-host is the chunk
      **server** (source of truth); the sandbox-host's
      daft-fuse is the chunk **client**. Connection direction
      depends on transport mode:
      - **Direct (LAN/opt-in):** client (user-host) dials
        sandbox-host. Once mTLS is up, RPC roles invert —
        user-host serves `WorkspaceService` on the connection
        it just opened, sandbox-host calls into it.
      - **Relay (v1 default):** both endpoints have a
        persistent outbound control channel to the
        orchestrator. Orchestrator splices their byte streams
        on demand. mTLS is end-to-end between client and
        sandbox-host; the orchestrator forwards opaque
        encrypted bytes and never sees plaintext workspace
        data. (Same property the project ethos requires:
        orchestrator authenticates but does not store
        workspace state.)

      *Handshake (client-initiated).* The user explicitly
      pushes data — matches the "user controls when data
      moves" principle and avoids silent-wait failure modes.
      Flow:
      1. User runs `daft workspace serve --workspace name=path
         --orchestrator <addr>`. Client opens persistent
         outbound mTLS to orchestrator; sends
         `BrokerService.RegisterServe(workspace_name=...)`;
         keeps the stream open. Orchestrator notes
         `(identity=client-fingerprint, workspace=name)` →
         "available".
      2. User runs `daft workspace attach --sandbox <sb-id>
         --workspace <name>`. Client sends
         `BrokerService.RequestAttach(sandbox=<sb-id>,
         workspace=<name>)`. Orchestrator generates a session
         token, validates ACLs, replies with
         `{relay_endpoint, session_id}` to the client AND
         pushes a `PendingAttach{session_id, peer=client-fp,
         workspace=name}` event down the sandbox-host's open
         control stream.
      3. Both endpoints open a `BrokerService.Relay` bidi
         stream to the orchestrator, presenting `session_id`.
         Orchestrator splices them at the byte level.
      4. Inside the spliced byte channel, client and
         sandbox-host run their own mTLS handshake end-to-end
         (using their existing daft identities; orchestrator
         is not a CA for this).
      5. Inside that inner mTLS, the user-host serves
         `WorkspaceService` Connect-RPC; sandbox-host calls
         `GetManifest` / `GetChunk` as needed.
      6. Sandbox-host's daft-fuse uses the inner Connect-RPC
         client for chunk fetches (Slice 4).

      *Why HTTP/2 server on a dialed connection.* The
      user-host has no inbound port. For both relay and
      direct modes, the user-host needs to *serve* HTTP/2 on a
      connection it itself opened. Standard HTTP/2 and
      Connect-RPC support this — the protocol is symmetric
      once the TLS handshake completes; only the dialing
      convention treats one side as "client". Go's
      `http.Server.Serve(net.Listener)` over an already-mTLS
      `net.Conn` (wrapped in a one-conn listener) is the
      idiom.

      *Wire surface — orchestrator broker.* New proto
      `proto/workspacebroker/v1/broker.proto`:
      - `BrokerService.RegisterServe(stream) → stream` —
        client opens with `RegisterServeRequest{workspace}`;
        orchestrator returns a stream that pushes
        `PendingAttach{session_id, peer_fp}` events as they
        arrive.
      - `BrokerService.RegisterSandbox(stream) → stream` —
        sandbox-host's persistent control channel. Same
        pattern as `RegisterServe` but for sandbox-side
        notifications.
      - `BrokerService.RequestAttach(req) → resp` — unary,
        client-initiated. Validates that both peers are
        registered, mints session_id, notifies the sandbox via
        its open stream, returns `{relay_endpoint, session_id}`
        to the caller.
      - `BrokerService.Relay(stream bytes) → stream bytes` —
        both peers open this with `session_id` in the first
        message; orchestrator splices the two streams. Inner
        traffic is opaque (end-to-end mTLS).

      *Scope-cut for the first commit (Slice 3.5a) — landed.*
      Proto + orchestrator-side `BrokerService` handler skeleton
      with the matching logic (Register/RequestAttach pairing)
      but no actual byte relay yet. Tests pin the matching state
      machine with mock streams.

      *Slice 3.5b — landed.* `Relay` byte-splice. New
      `SessionBroker` in `internal/workspacebroker` rendezvouses
      two halves of a session_id (Reserve at RequestAttach time,
      Pair at Relay-handler time). `Splice` is a transport-neutral
      bidirectional copy with a shared `Done` channel so either
      side returning unblocks the partner's blocked Receive.
      Connect bidi stream is adapted to the `DataStream` interface
      via `connectRelayAdapter`. Reservations have a 30-second
      TTL (lazy-swept on Reserve/Pair) and same-identity
      sessions (alice→alice) are allowed — session_id is the
      bearer credential, outer mTLS already authenticated each
      half. Error codes: ErrNoSession→FailedPrecondition,
      ErrWrongPeer→PermissionDenied. End-to-end test pumps
      payloads in both directions through the splice and verifies
      clean shutdown propagation.

      *Slice 3.5c-1 — landed.* Transport-neutral RelayConn
      (DataStream as net.Conn) and OneConnListener (single-conn
      net.Listener for http.Server.Serve over a dialed connection)
      in `internal/workspacebroker`. Deadlines are no-ops in v1.

      *Slice 3.5c-2 — landed.* `internal/brokerclient` package
      wrapping the user/sandbox-host side of the broker:
      outer-mTLS HTTP/2 to the orchestrator, RegisterServe /
      RegisterSandbox with blocking-until-Ready, Attach
      (RequestAttach wrapper), DialRelay returning a net.Conn
      over the byte-spliced channel. End-to-end test runs a full
      TLS 1.3 handshake plus echo through the orchestrator's
      Relay to pin that the spliced channel behaves like a
      net.Conn well enough for crypto/tls (and by extension
      HTTP/2) to ride over it.

      *Slice 3.5c-3 — landed.* `daft workspace serve
      --orchestrator <url>` and `daft-sandboxd run
      --workspace-broker <url>` now run through the broker.
      Both keep the direct-dial flags (`--addr` and
      `--workspace-from`) as LAN-mode fallbacks rather than
      renaming — backward-compatible. Inner mTLS terminates at
      each endpoint with a per-attach `identity.NewMemoryPeerStore`
      trusting only the orchestrator-announced peer. Tests pin
      the full cobra→broker→inner-mTLS→WorkspaceService round-trip
      on both sides.

      *Slice 3.5d — landed.* `daft workspace attach
      --orchestrator <url> --workspace <name> --sandbox <id>`
      wraps `brokerclient.Client.Attach`, the user-host's
      trigger for an outstanding registered sandbox to pair
      with the running serve. The v1 broker ACL accepts
      RequestAttach only from the serve owner (see
      matcher.go), so the caller's own fingerprint is used as
      the serve identity. End-to-end test drives the cobra
      surface through the orchestrator and asserts both halves
      complete; bad-flag and unreachable-orchestrator branches
      are pinned independently. Slice 3.5 is now complete.

      *Direct-dial mode (kept, not default).* The existing
      Slice 2.5 path (`--workspace-from <host:port>`) becomes
      `--workspace-direct <host:port>` with a deprecation
      warning. Useful for the dev loop where both ends are on
      `localhost`; gated behind a flag because it depends on
      reachability assumptions that don't hold in production.

      *Open sub-questions to resolve during 3.5b/3.5c.*
      - Connection reuse: should `daft workspace serve` hold
        one orchestrator connection forever, or reconnect on
        every workspace attach? Decision pending profiling —
        default "one persistent connection, exponential
        reconnect backoff on drop".
      - ACL model: who can attach a workspace to which
        sandbox? v1: sandbox owner == workspace owner == same
        identity. Cross-identity attach is out of scope; it
        invites a whole permissions UX problem.
      - Bandwidth accounting: the orchestrator's uplink is
        v1's only bottleneck. Worth an attribute on relay
        spans (`bytes_relayed`, `peak_concurrent_streams`)
        so the operator sees when to enable direct-dial.

      **Slice 4 design (daft-fuse)** — *unchanged from the
      previous design block, except the chunk-fetch transport
      now runs inside the Slice 3.5 relay rather than dialing
      a user-host port directly.*

      *Goal.* Replace the Slice 2.5 pre-fetch model with
      lazy-on-read so multi-GiB workspaces don't materialise
      on disk before the VM boots. Establishes the seam for
      freeze-on-suspicious-IO (enforcement is a follow-up).

      *Mount layout.* daft-fuse → virtiofsd → VM. Sandbox-host
      mounts daft-fuse at
      `/run/daft/sandboxd/<sandbox-id>/workspace`; virtiofsd's
      `--shared-dir` points there; VM gets a virtiofs share
      at `/workspace`. No virtiofsd fork.

      *Library.* `github.com/hanwen/go-fuse/v2`, NodeFS API.
      Pure Go.

      *Backing model.* Manifest fetched once at attach time
      via `WorkspaceService.GetManifest` (over the Slice 3.5
      tunnel). In-memory inode tree from `FileEntry`. Reads
      resolve chunk-by-chunk via the chunk-ref list.

      *Read path.* On `Read(inode, offset, size)`:
      1. Walk `[]ChunkRef`, identify the chunk(s) covering
         `[offset, offset+size)`.
      2. For each chunk: `cas.Store.Get(digest)`. Hit →
         serve. Miss → call `WorkspaceService.GetChunk` over
         the tunnel, write into CAS, serve.
      3. `singleflight.Group` keyed on digest so concurrent
         readers of the same chunk share one fetch.
      4. 3 retries with exponential backoff on transient
         remote errors; permanent error → `EIO` to VM and
         record span with `STATUS_ERROR`.

      *CAS integration.* Reuse existing
      `internal/cas.Store`. daft-fuse refcounts chunks for
      the mount lifetime (same pattern as image-bundle
      refcounts) so LRU eviction can't yank live chunks.

      *Policy hook (interface lands now, enforcement later).*
      `daftfuse.Policy.OnRead(*ReadEvent) Decision` with
      `Allow | Deny | Freeze`. Default `AllowAll`. `Deny` →
      `EACCES`. `Freeze` → `EAGAIN` + signal an external
      freeze handler (the QMP `stop` wiring is a separate
      commit).

      *Integration with daft-sandboxd.* New flag
      `daft-sandboxd run --workspace-attach <name>` (lazy mode,
      uses the broker tunnel from 3.5) mutually exclusive
      with `--workspace-from <name>` (pre-fetch mode, also
      via broker but eager) and `--workspace-direct <addr>`
      (LAN fallback).

      *Tests.*
      - Unit: synthetic manifest + mock chunk-fetcher.
      - Integration: real mount via `fusermount`, real CAS,
        in-process broker + serve fixture.
      - E2E: full mount → virtiofsd → VM → container reads
        file → assert content. `BenchmarkLazyWorkspaceCold`
        vs `BenchmarkPrefetchWorkspaceCold` baseline.

      *First-commit scope (Slice 4a).* Read-only daft-fuse
      library with mock-fetcher unit tests + a single
      `fusermount`-driven integration test. No daft-sandboxd
      integration yet; that lands in Slice 4b.

      *Out of scope for Slice 4.*
      - QMP freeze enforcement (Slice 4c or its own slice).
      - Prefetch/readahead heuristics. v0 is pure lazy.
      - Write overlay (Slice 5).
      - Manifest live-invalidation.

      *Open sub-questions.*
      - FUSE `allow_other` for virtiofsd's per-sandbox uid.
        Likely needed; resolve when wiring up sandboxd.
      - Bundle `fusermount` setuid binary into
        daft-sandboxd's Nix closure (decision: yes, to stay
        self-contained per project ethos).
- [x] Power of 10 (Go) — Holzmann's safety-critical-code rules,
      translated to Go and enforced by custom analyzers (review-only
      "rules" don't survive contact with deadlines; every adopted
      rule has a static check). See CONTRIBUTING.md "Power of 10
      (Go)" for the full rule list + escape-hatch convention.
      All 5 program-of-work slices landed; 10 analyzers shipped.

      Why now: the sandbox is the security boundary between
      untrusted AI agents and the user's data — "we'll harden it
      later" doesn't survive. Static enforcement is cheaper than
      audits.

      **Program of work** (5 slices):
      - **Slice 1 — foundation** (in progress): `internal/assert`
        package (`True` / `NoErr`); `internal/lint/analyzers/`
        scaffolding; `cmd/daft-lint` multichecker binary wired
        into `make lint`; first two analyzers — `assertpure`
        (`assert.*` first arg must be side-effect free and
        non-trivial) and `lintreasons` (every
        `//daft:lint:disable=…` line must carry a non-empty
        `reason="…"`). Adopts Rule 5 (in its purity-checked form)
        and the escape-hatch convention used by every later
        slice.
      - **Slice 2 — keyword/pattern bans (landed)**: three new
        custom analyzers — `nogoto` (rule 1a), `nolinkname`
        (rule 1e), `nodoubleptr` (rule 9 Go-form). `forbidigo`
        enabled in `.golangci.yml` with patterns banning
        `panic(`, `recover()`, and `unsafe.*` — legitimate uses
        survive at the call site with `//nolint:forbidigo` plus
        a reason (assert vocabulary, trace rand.Read fatality,
        analyzer-framework integrity). `gochecknoglobals`
        enabled with path-scoped exclusions for systemic
        patterns (cmd/*/main.go testable-main, _test.go
        fixtures, internal/lint/analyzers Analyzer vars);
        per-line nolints justify each remaining production
        global (oops singletons, container/vsock/keychain
        injection points, FastCDC lookup tables, etc.).
        Adopts rules 1a/1b/1c/1d/1e, 6, 9.
      - **Slice 3 — bounded-input checks (landed)**: `chanconst`
        (channel buffer sizes must be a typed constant or
        unbuffered) and `maxbytes` (`io.ReadAll` on
        `http.Request.Body` must be preceded by
        `http.MaxBytesReader` in the same function). Adopts rule
        3 in its Go-shaped form. Cleanup: `brokerclient.ServeBrokered`
        gained a hard `maxConcurrentAttachesCeiling` upward cap
        so the semaphore buffer's bound is statically knowable
        even though the make() call itself reads from
        opts.MaxConcurrentAttaches.
      - **Slice 4 — annotation-driven checks (landed)**:
        `forloopbound` (bare `for {}` must carry
        `//daft:lint:disable=forloopbound reason="…"`) and
        `tagcatalog` (`//go:build` tag identifiers must come
        from a closed allowlist: `linux`, `darwin`, `integration`).
        Adopts rules 2 and 8.
        Cleanup pass added 10 directive comments to existing
        production for-loops, naming each loop's termination
        signal in the `reason` clause. The 6 ctx-driven loops
        also got a runtime `assert.True(ctx.Done() != nil, …)`
        at function entry (after input validation, before the
        blocking loop) — shifts the "non-cancellable context"
        failure from a 3am hang to an immediate panic at the
        call site. See CONTRIBUTING.md "Assert caller contracts
        at function entry" for the broader pattern.
      - **Slice 5 — call-graph analyzer (landed)**: `norecursion`
        flags self-recursive and mutually-recursive functions
        within a single package. Three-colour DFS over a
        per-package call graph built from `pass.TypesInfo` (AST
        only, no SSA needed — runs in milliseconds). Tracks
        method calls and same-package function calls; deliberately
        doesn't follow function values, closures, or interface
        dispatch (those need CHA on a whole-program SSA model,
        far more expensive and harder to keep stable in CI).
        Cleanup: two recursive tree-walks (`trace.writeSpan` over
        span children, `tagcatalog.walkTags` over a constraint
        expression) suppressed with reasons citing the structural
        depth bound of each input. Adopts rule 1f.

      *Out of scope.*
      - "Two assertions per function" density target — density
        theatre without a useful signal.
      - "No function pointers" / one-level-deref limit — Go's
        interface dispatch and closures are the main abstraction
        tool; banning them is not viable.
      - Broad "no unbounded `append` from untrusted input" — no
        honest static check; stays a review concern.
- [x] Local-artefact integration build — `make integration` no
      longer calls `nix build` for the initramfs or the hello
      bundle. The slow-moving artefacts (cloud-hypervisor, kernel,
      virtiofsd, crun, u-root) are pinned by the Nix dev shell and
      exposed via `DAFT_*` env vars; per-commit artefacts assemble
      locally under `bin/initramfs.cpio.gz` + `bin/hello-bundle/`
      via Make + u-root. Why: `nix build .#initramfs` minted a new
      store path on every commit (the daft-agent binary embeds into
      the cpio), flooding `/nix/store` over time and exhausting the
      root partition on dev machines. The flake's initramfs /
      hello-bundle derivations are still used by
      `nix build .#daft-sandboxd` for release builds — only the
      iteration path moved.
- [x] Workflow reorientation Slice A1 — `daft-sandboxd serve` daemon
      mode. New `SandboxControlService` proto under
      `proto/sandboxctrl/v1` (`RegisterSandbox` server-stream +
      unary `Heartbeat`); orchestrator-side registry in
      `internal/sandboxctrl` keyed by mTLS fingerprint with
      `RegisteredAt` / `LastSeenAt`; outbound client in
      `internal/sandboxctrlclient` (mTLS dial → register → 5s
      heartbeat ticker until ctx cancel); CLI surface added as
      `daft-sandboxd serve --orchestrator <url>` with a
      `runRegisterAndServe` test seam. Resource probe reads CPU
      via `runtime.NumCPU()` and total RAM via `/proc/meminfo`
      MemTotal. End-to-end test in `internal/sandboxctrlclient`
      stands up an in-process orchestrator and verifies
      registration appears, heartbeat refreshes `LastSeenAt`, and
      the entry is released on stream close.
- [x] Workflow reorientation Slice A2 — `ListSandboxes` RPC on
      `SandboxControlService`. Adds `ListSandboxesRequest` /
      `ListSandboxesResponse` + `SandboxRegistration` messages and a
      handler that projects `Registry.Snapshot()` onto the wire.
      Operator surface is `daft sandbox list --orchestrator <url>`
      (mTLS-dial, render fingerprint / version / CPUs / memory /
      last-seen age as a tab-aligned table). Read-only; persistence
      still deferred to Slice B5.
- [x] Workflow reorientation Slice A3 — `SandboxSpec` proto. Adds
      `SandboxSpec` plus supporting messages `Mount`, `EnvVar`,
      `PortMapping`, `GpuRequest`, and enums `Protocol`, `Isolation`
      under `daft.sandboxctrl.v1`. Wire-only — no Dispatch RPC, no
      sandboxd-side executor yet. Field set matches the PLAN.md A3
      sketch (image_ref, mounts[], env[], ports[], gpu_request,
      isolation, workspace_uuid); `Mount.source` is documented as
      orchestrator-allocated (workspace UUID or registered tag),
      not free-form host paths — adoption (Slice F1) is how
      user-host directories enter the namespace.
- [x] Workflow reorientation Slice A5b — `daft-sandboxd run`
      deprecation rename. The verb now lives at `daft-sandboxd
      debug run` under a new `debug` parent for legacy / offline
      subcommands. Each invocation prints a stderr banner
      directing users at orchestrator-driven dispatch as the
      production path. Phase A's loop is now closed end-to-end:
      orchestrator dispatches a SandboxSpec → sandboxd pulls +
      bundles + boots a container, with no CLI involvement.
- [x] Workflow reorientation Slice A5a — sandbox-host executor
      consumes `spec.image_ref`. New `internal/sandboxboot`
      package factors the image-pull + bundle + lock + artefact-
      extract glue into a `BuildBundleConfig` function with a
      testable `Deps` struct. `internal/sandboxexec.Executor`
      gains a `Config` struct and an `Assembler` seam:
      `daft-sandboxd serve` wires the production assembler (which
      calls `sandboxboot.BuildBundleConfig` against the local
      imagestore + `bundled.Extract`), so dispatches carrying
      `image_ref` now pull + bundle + boot a real container.
      Empty `image_ref` still falls through to the bare Hello
      baseline (smoke-test path). The legacy `daft-sandboxd run`
      command is unchanged — A5b deprecates it.
- [x] Workflow reorientation Slice A4b (minimal) — sandboxd-side
      executor. New `internal/sandboxexec` package: a
      `DispatchHandler` that, on every Dispatch event, runs
      `sandbox.Hello` with the baseline `sandbox.Config` resolved at
      startup from the `DAFT_*` env vars. `daft-sandboxd serve` now
      constructs the executor via a `buildDispatcher` test seam and
      passes it to `sandboxctrlclient`. The spec's `image_ref` /
      `mounts[]` / `workspace_uuid` are recorded as span attributes
      and logged but do not yet drive a container — Slice A5 will
      route the existing `daft-sandboxd run` image-pull + bundle
      pipeline through this executor and deprecate the `run`
      subcommand.
- [x] Workflow reorientation Slice A4a — Dispatch wire path.
      `RegisterEvent` oneof gains a `Dispatch` variant
      (`dispatch_id` + `SandboxSpec`). Orchestrator-side handler
      now owns a per-fingerprint bounded event channel and exposes
      `SandboxControlHandler.Dispatch(fp, id, spec)`; the
      `RegisterSandbox` goroutine is the sole `stream.Send` writer.
      Sandboxd-side `sandboxctrlclient.Config` gains a
      `DispatchHandler` seam — `consumeStream` invokes it on every
      Dispatch event, and a non-nil error tears the registration
      down. Defaults to nil (drop events silently), so the real
      executor lands in A4b without churning the wire path again.
      End-to-end tests cover delivery, handler error propagation,
      ErrSandboxNotConnected, and ErrDispatchBacklog.

## Architecture

The standalone reference is [ARCHITECTURE.md](ARCHITECTURE.md). The
condensed view follows; anything in conflict, the standalone doc wins.

### Components

There are 4 main components:
- **client CLI** (`daft`): runs on the user-host; talks to the orchestrator
- **Orchestrator**: authority over Workspaces and Sandboxes; authenticates
  clients and Nodes; binds Sandboxes to Nodes; will host the Phase C ingress
- **Node** (`sandboxd`): per-host daemon that runs Sandboxes (VMs + the
  container inside); reports status. Single-binary bundle including VMM,
  virtiofsd, kernel, initramfs.
- **In-VM agent** (`daft-agent`): runs inside the VM; spawns `crun`-managed
  containers; ships traces back over vsock.

Note that the containers the agent creates are not a security boundary —
the VM is. Containers exist for filesystem/userland convenience.

### Resource model (one-line summary)

Three resources, each with the K8s-style metadata+spec+status triple:

- **Workspace**: user-owned, long-lived, declarative. The thing the user
  edits incrementally and Launches.
- **Sandbox**: controller-derived, short-lived, one per Launch. The unit
  of execution: 1 VM × 1 container in strict isolation; packed into a
  shared VM in non-strict.
- **Node**: long-lived, the registered `sandboxd`. Identified by mTLS
  fingerprint.

Full shape, conditions, naming conventions, and the K8s mapping +
integration paths live in [ARCHITECTURE.md](ARCHITECTURE.md).

### Concepts

Workspace:
- Directories and single files the user synchronises with the Sandbox. 
  Essentially like a docker bind mount or volume, except that changes are not automatically synchronised. The user chooses when to send (except when streaming) and receive.
  Changes are diffed so the user can see what has changed and to save network bandwidth
- Env vars sent to the sandbox
- State and data in the sandbox that is not necessarily retreived by the user, but persists
- Configuration of the sandbox and workspace itself

### file layout
- `cmd/<name>/main.go` — entry points for binaries. Three so far:
  - `cmd/daft` — host-side client CLI (`daft sandbox hello`, `daft version`,
    `daft completion`).
  - `cmd/daft-agent` — in-VM agent. Linux-only; runs as PID 1's child via
    u-root's `-uinitcmd`.
  - `cmd/daft-sandboxd` — sandbox-host single-executable bundle (~26 MiB).
    Embeds cloud-hypervisor + virtiofsd + the kernel + the initramfs and
    extracts them to `$XDG_CACHE_HOME/daft-sandboxd` on first run.
- `internal/<pkg>` — non-exported library code; everything that isn't a `main`
  package or a top-level config file lives here.
- `third_party/` — vendored upstream sources as git submodules (Linux
  kernel 6.19.14, Cloud Hypervisor 51.1, virtiofsd 1.9.0, u-root 0.16.0).
  No Go code under here is owned by us — Makefile and golangci-lint are
  scoped to `cmd/` + `internal/` only.
- `bench.txt`, `coverage.txt` — committed baselines; pre-commit hook
  fails on coverage drop > 1pp or any benchmark > 2× regression.
- `version.txt` — single source of truth for the released version. Read by
  `flake.nix` (Nix package version) and embedded into the `daft` binary at
  build time.

## Implementation

Client CLI:
  - Pure Go
  - Athenticates with the

### I/O and protocols

- Wire transport: HTTP/2 over TLS 1.3, Go stdlib, multiple connections per peer (split bulk from low-latency control/streams)
- Peer identity: self-signed Ed25519 mTLS certs
  - TOFU + manual `daft auth approve` on first contact, with timeout
  - Orchestrator can later act as a CA and issue short-lived component certs
  - No login RPC: identity is the TLS connection
- RPC: Connect-RPC (`connectrpc.com/connect`) over HTTP/2
  - Plain HTTP/2, browser/curl-debuggable, no gRPC runtime, no 4 MiB ceiling
- Port forwarding: `CONNECT`-style stream hijack over HTTP/2, both directions
- Controller ↔ in-VM agent: virtio-vsock (`AF_VSOCK`), Connect-RPC over it
  - Lets the sandbox VM run with no virtual NIC when a workload doesn't need one
- Terminals (no SSH anywhere in `daft`):
  - VM kernel/init console: virtio-serial → controller → orchestrator (debug only)
  - Container TUI: PTY in agent → vsock stream → controller → HTTP/2 to client

### Workspace file sync

- Network layer (user-host ↔ sandbox-host): chunked, content-addressed, lazy
  - Chunking: desync (FastCDC + zstd), reuse its HTTP chunk-store protocol
  - Manifest: our own protobuf (file tree + per-file chunk hash list), exchanged on attach
  - User-host runs an embedded chunk server in the `daft` client; relay via orchestrator when sandbox-host can't dial directly
- Sandbox-host ↔ VM: virtiofs, unmodified upstream `virtiofsd` (Rust), spawned by the sandbox controller
- Inspection / freeze-on-suspicious-IO hook: our own FUSE FS (`daft-fuse`, go-fuse v2) sits *under* virtiofsd
  - virtiofsd serves a directory that is actually a FUSE mount we provide — no virtiofsd fork
  - `daft-fuse` resolves reads via local desync CAS, fetches missing chunks over HTTP/2 from the user-host, optionally prefetches
  - Policy/audit/freeze decisions live here; trip → `EAGAIN` + QMP `stop` the VM
- Writes: default local-only overlay, surfaced as a diff and pulled explicitly via `daft pull` (matches "user controls when data moves")
- Manifest semantics: snapshot at attach time; updates require an explicit re-sync (live invalidation deferred)

### Tracing

- **Custom format, orchestrator-as-collector.** Producers (every
  daft binary) keep an in-memory ring buffer of finished spans;
  shipped to the orchestrator via Connect-RPC `TraceService.
  ReportSpans`. Wire format is daft-native (`proto/trace/v1`); a
  separate proxy-exporter component can later translate to OTLP
  for users who already run OTel collectors. We don't depend on
  third-party components (cloud-hypervisor, virtiofsd, crun, the
  in-VM agent) implementing OTel because most don't.
- **Abandoned-span first-class.** Unlike OTel which silently
  drops spans whose End never arrives, daft marks them
  `STATUS_ABANDONED` and ships them anyway: a deadline reaper on
  the producer catches in-process leaks; `Tracer.Shutdown` sweeps
  any still-open spans on graceful exit; orchestrator side gets
  to see partial traces from a crashed component (subject to the
  buffer making it across before the crash).
- **Trace context on the wire.** `Daft-Trace-Id` (32 hex chars)
  and `Daft-Parent-Span-Id` (16 hex chars) headers set by the
  Connect client interceptor; server interceptor reads them,
  validates, opens a child span. Malformed values are ignored —
  the server starts a fresh trace rather than parent off
  attacker-controlled IDs. We use our own header names instead of
  W3C `traceparent` to keep a clean translation seam in the
  future proxy-exporter rather than colliding with other
  instrumentation in the same request.
- **Errors are events on spans.** Following Ayup's `terror`
  pattern, `trace.RecordError(ctx, err)` and `trace.Errorf(ctx,
  format, args...)` attach the error as a span event and (if not
  already set) mark the span `STATUS_ERROR`. Cleanup-path
  errors continue to use `internal/oops` — the two are
  complementary.
- **Debugging surface.** Orchestrator's `/debug/traces` renders
  the central buffer as text (default) or JSON (`Accept:
  application/json`). Tests use `trace.LogOnFailure(t, tracer)`
  to register a `Cleanup` that dumps via `t.Logf` on failure;
  works with no orchestrator running.

### MicroVM stack

- VMM: **Cloud Hypervisor** (Rust, LF, REST over Unix socket)
  - Only candidate covering virtiofs + vsock + virtio-pci/blk/serial + PCI/VFIO incl. NVIDIA GPUDirect P2P
  - Go control: vendor the OpenAPI spec, generate client with `oapi-codegen`; wrap behind `internal/vmm` interface
  - Fallbacks behind the same interface: Firecracker (no virtiofs/PCI, but snapshot-restore < 50 ms); QEMU q35 for exotic passthrough
  - Boot-to-agent target: 150–250 ms cold; sub-50 ms with snapshot-restore later
  - Measured (BenchmarkHelloBoot, KVM, Intel i5-14500): ~310 ms cold,
    ~349 ms with virtiofs share. Above target — kernel decompression and
    cgroup setup dominate; revisit when we have a snapshot path or a
    leaner kernel config.
- Kernel: custom build, LLVM cross-compiled, stripped defconfig
  - Reuse the kernel-build approach from `richiejp/m`; replace its Zig init
  - On for CH guest: `CONFIG_VIRTIO_PCI`, `CONFIG_VIRTIO_FS`, `CONFIG_VSOCKETS`, `CONFIG_VIRTIO_VSOCKETS`, `CONFIG_FUSE_FS`, `CONFIG_VFIO_PCI`, `CONFIG_BPF`, `CONFIG_BPF_LSM`
  - Cmdline: `console=hvc0 reboot=t panic=-1 tsc=reliable clocksource=tsc random.trust_cpu=on`
- Init: **u-root** `cmds/core/init` only (`github.com/u-root/u-root`)
  - Pure Go; mounts essentials, reaps zombies, execs `-uinitcmd`
  - Initramfs is built from the flake via `u-root -base=/dev/null
    -defaultsh="" -uinitcmd=/bin/daft-agent ./cmds/core/init` so only
    u-root's busybox-style init binary and our agent end up in the
    image — no shell, no diagnostic tools.
  - daft-agent runs as `/bin/uinit` (symlink to `/bin/daft-agent`).
  - Replace with bespoke ~300-line Go init only if u-root blocks us.

### Container runtime (inside the VM)

- The container is packaging, not isolation — VM is the security boundary
- Runtime: **crun** (~2 MB, static-musl, runc-CLI compatible)
  - Invoked from the agent via `os/exec`; swappable for runc/youki by changing the path
- **Pull on the host, not in the VM:**
  - Sandbox-controller uses `github.com/google/go-containerregistry` (`crane`) to fetch layers into a content-addressed cache
  - Unpack via `github.com/opencontainers/umoci` into `/var/lib/daft/bundles/<digest>/rootfs`
  - Bundle dir virtiofs-shared read-only into VM at `/bundle`; workspace at `/workspace` (rw)
  - Agent runs `crun run --bundle /bundle …`; tmpfs-backed overlayfs in the VM for container writes to rootfs
- Consequences: VM image carries no registry client, no auth code, no image cache; layer cache shared across jobs; VM can run with zero network egress

### Container image distribution

- **Orchestrator hosts the registry.** `daft-orchestrator` embeds
  `github.com/google/go-containerregistry/pkg/registry` as another
  HTTP/2 handler alongside the Connect-RPC endpoints. Pure Go,
  ~500 lines, already a transitive dep via crane — fits the "every
  shipped binary is statically linked and self-contained" ethos
  without pulling in `distribution/distribution` or shelling out
  to zot.
- **Auth reuses the orchestrator's CA.** The orchestrator already
  issues short-lived mTLS certs to clients and sandbox-hosts (see
  "I/O and protocols"); the registry handler authenticates the
  same certs and maps the cert subject → namespace. No second
  authn scheme, no Hub account, no per-host registry credentials.
- **User flow with a Dockerfile**:
  1. `docker build -t orchestrator.example.com/<user>/myimage .`
  2. `docker push orchestrator.example.com/<user>/myimage`
     (mTLS cert handed to `docker login` once, scoped to the user).
  3. `daft sandbox run --image orchestrator…/myimage cmd` —
     sandbox-host pulls by digest from the same registry.
- **Cleanup policies (two stores, two policies):**
  - Layer cache (`/var/lib/daft/images/sha256:.../`): the only
    bandwidth-saver. LRU under a configurable byte budget,
    plus `daft images prune --older-than 7d`.
  - Bundle cache (`/var/lib/daft/bundles/<digest>/rootfs/`):
    derived state, umoci can rebuild from layers in seconds.
    Refcount via per-VM lockfiles (cgroup-lifecycle hooked) so
    a running VM's bundle is never GC'd; otherwise GC at job
    end or on a short TTL.
  - Orchestrator-side registry storage gets the same byte-budget
    policy plus an explicit `daft image rm <ref>` for the user.
- **Local-only escape hatch.** For developers without an
  orchestrator handy, `daft sandbox run --image-tar img.tar`
  ingests `docker save` / `docker buildx --output type=oci` tarballs
  directly via `crane.Load`. Same pipeline downstream of the load —
  no "registry only" assumption at the bundle layer.

### Privilege model — rootless agent, unprivileged containers

- **Goal**: after boot, no daemon runs as real root inside the VM. Forces attackers into a kernel exploit (or a VM escape) for any privilege gain
- Layering:
  - `init` (PID 1) holds real root for the **boot phase only**: mounts `/proc`, `/sys`, devtmpfs, cgroup2, virtiofs; cgroup v2 delegation to the agent's uid; user-namespace creation with a 65k sub-uid range; chowns `/workspace`, `/bundle`, and exposed device nodes; `setns` into the userns; drops all caps; execs `daft-agent`
  - `daft-agent` runs unprivileged on the VM but is **namespace-root within its userns** — has full caps scoped to namespace-owned resources (mounts, namespaces, scoped netns)
  - Containers run nested rootless inside the agent's userns; default is single-uid (container uid 0 → one agent-namespace uid)
- **No `newuidmap`/`newgidmap`**: multi-uid mapping is set up once by privileged init before dropping into the userns — eliminates the most common rootless-container setuid LPE target
- Container capabilities are **opt-in per workspace**, scoped to the agent's userns; the agent has authority to grant them because they're namespace-scoped
- Networking: tap/macvlan device assigned by the controller and placed into the agent's netns by init; alternative slirp4netns-style userspace networking for the "no host net" mode
- GPU device nodes: init chowns `/dev/nvidia*`, `/dev/dri/*` into the userns mapping range so the agent can pass them in
- cgroup v2 only (v1 lacks clean unprivileged delegation) — already in our defconfig
- Out-of-scope for v1 (would require an opt-in "privileged" sandbox profile): loading kernel modules from the container, mounting arbitrary block devices, real `CAP_SYS_ADMIN` operations outside the userns

### Syscall policy (post-v1)

- Not gVisor — enforcement requires forking Sentry, and Falco dropped its consumer in 0.43
- Primary: `SECCOMP_RET_USER_NOTIF` with a Go supervisor inside the VM (block / spoof return / inject FD)
- Secondary: eBPF-LSM on FS/network paths (we own the guest kernel config)
- File-IO policy is already covered host-side by `daft-fuse` under virtiofsd

### GPU / PCI passthrough

- VFIO with `intel_iommu=on iommu=pt` / `amd_iommu=on iommu=pt`; **no ACS overrides** in production
- v1 target: full-card-per-VM passthrough of consumer NVIDIA (RTX 4090/4080/5080/5070 Ti)
  - Avoid RTX 5090 / RTX PRO 6000 Blackwell until the FLR-after-guest-exit bug is fixed
  - Skip AMD consumer for v1 (reset story still flaky); skip Intel for v1 (compute ecosystem trails CUDA)
- Resource model: `{kind: full_card | mig_instance | sriov_vf, host_bdf, vram_gib}` — all three terminate at a PCI BDF
- Per-class `reset_strategy`: `flr | bus_reset | vendor_reset | host_reboot_required`; quarantine cards after one job when required
- bind/unbind verifies the IOMMU group is empty of host-owned devices before VM start
- Multi-GPU sharing (later): MIG (NVIDIA datacenter, AI Enterprise license) or SR-IOV VFs
- IOMMUFD migration deferred until Cloud Hypervisor lands native support (issue #6892); keep the binding logic abstracted

### Hardening — minimize in-VM LPE surface

- **Principle**: VM is the security boundary, but the userland inside it should still be hostile to LPE. Force the attacker to either (a) find a kernel exploit or (b) escape the VM without LPE first
- VM rootfs (outside the container):
  - **Zero setuid/setgid binaries**; CI check fails the build if any appear
  - **Zero file capabilities** (`getcap -r /` empty)
  - No sudo, su, polkit, PAM, dbus, login, shadow tooling
  - No interpreters (no bash, no python, no perl) — agent provides any in-band debugging
  - No daemons running as root beyond `init` and `daft-agent`
  - Rootfs read-only; tmpfs runtime state mounted `nodev,nosuid,noexec`
- Container defaults (applied by daft-agent within its user namespace, overridable per workspace via opt-in caps — see **Privilege model**):
  - Single-uid user-namespace mapping by default (container uid 0 → one agent-namespace uid)
  - All capabilities dropped; allowlist only what's requested
  - `no_new_privs` always
  - Strict default seccomp profile (deny-by-default allowlist)
  - `nodev,nosuid` on every container mount including the workspace
  - Read-only base layer; writes on tmpfs-backed overlayfs upper
  - eBPF-LSM enforces FS/network policy (guest kernel built with `CONFIG_BPF_LSM`)
- daft-agent itself:
  - Single static Go binary; no plugins, no eval, no shell-out helpers
  - Runs **unprivileged** on the VM (namespace-root only); see **Privilege model**
- Kernel hardening (KSPP-aligned defconfig):
  - `INIT_ON_ALLOC_DEFAULT_ON`, `INIT_ON_FREE_DEFAULT_ON`, `INIT_STACK_ALL_ZERO`
  - `RANDOMIZE_KSTACK_OFFSET_DEFAULT`, `VMAP_STACK`, `STACKPROTECTOR_STRONG`
  - `SLAB_FREELIST_HARDENED`, `SLAB_FREELIST_RANDOM`, `SHUFFLE_PAGE_ALLOCATOR`
  - `FORTIFY_SOURCE`, `STRICT_KERNEL_RWX`, `STRICT_MODULE_RWX`, loadable modules disabled in prod
  - `BPF_UNPRIV_DEFAULT_OFF`, `LEGACY_PTYS=n`, `LEGACY_TIOCSTI=n`, `DEVMEM=n`, `KCMP=n`
  - KCFI (clang) once toolchain is solid
  - sysctl: `panic_on_oops=1`, `kernel.dmesg_restrict=1`, `kernel.kptr_restrict=2`, `kernel.unprivileged_bpf_disabled=1`
- Sandbox-host side:
  - Cloud Hypervisor and virtiofsd run as per-VM unprivileged users; `/dev/kvm` ACLs scope KVM access
  - Each VM in its own cgroup, namespace set, and user namespace

### Build and distribution

- **Zero reliance on the host distribution.** Everything `daft` ships — host-side and in-VM — is built from source by us
- Upstream sources vendored as **git submodules**: Linux kernel, Cloud Hypervisor, virtiofsd, u-root, crun
- All binaries **statically linked**, no dynamic loader, no libc on the target rootfs:
  - Go (daft, sandbox-controller, sandbox-agent, init): `CGO_ENABLED=0`
  - Rust (Cloud Hypervisor, virtiofsd): `--target x86_64-unknown-linux-musl`
  - C (crun): static-musl
- Nix flake is the reference build environment and drives every submodule build

### Programming language

Main programming language is Go. Components that would be very difficult to write in Go
can be written in C/C++ or Zig then used either via Purego (no CGO) or as a separate executable
that Go runs with command args or communicates with a socket using some simple protocol. What
language we choose depends on the libraries we need. If we can use Zig then that is preferable because 
cross compilation becomes easy if we only use Go and Zig.




## Conventions

See [CONTRIBUTING.md](CONTRIBUTING.md). Highlights:

- Commit early and often.
- No Go files at the repository root — they go in `cmd/` or `internal/`.
- Every feature ships with tests.
- Use runtime assertions liberally: any invariant that can be checked, should be.
- Write doc comments on exported identifiers (so `go doc` picks them up).
- Any function that ingests outside input gets a `FuzzXxx` target — see
  `CONTRIBUTING.md` for the pattern.

## Open questions

- **Authentication ergonomics**: TOFU + manual `daft auth approve` works
  for SSH-style trust but is awkward for a fleet of sandbox-hosts. When
  do we promote the orchestrator to a CA and stop bothering the user
  for every new host?
- **Workspace sync UX**: ~~push/pull vs streaming~~ resolved by the
  2026-05-12 direction — both are offered. Streaming via `--stream-files`
  (daft-fuse over the broker, freeze-on-disconnect via Phase G);
  explicit push/pull via `daft workspace push|pull` for the deterministic
  path. The user picks per workspace.
- **virtiofsd static-musl**: ~~missing libc symbol references~~ Done.
  Switched to `pkgsStatic.rustPlatform.buildRustPackage` with
  `LIBCAPNG_LINK_TYPE=static` + `pkgsStatic.libcap_ng`. virtiofsd is
  now `statically linked` like every other daft-sandboxd binary.
- **Boot-time gap**: measured ~310 ms vs the 150–250 ms target. Worth
  the kernel-config diet before snapshot-restore lands?
- **Multi-container-per-VM mode (user-selected isolation)**: current
  design is one container per VM, with the VM boundary doing all the
  isolation work. On consumer hardware with a single passthrough GPU
  this means at most one GPU-using sandbox at a time, and every task
  pays full VM boot + model-load-into-VRAM cost. Proposed knob: let
  the user opt a sandbox into "shared" mode where the in-VM agent
  runs N crun containers sharing the kernel, GPU driver, VRAM cache,
  and (optionally) workspace. Container-to-container isolation falls
  back to namespaces + seccomp + cgroups + eBPF-LSM — kernel
  exploits and LPE bugs reach sibling containers and their data.
  The VM boundary still protects the host. Default stays "strict"
  (VM-per-task); user explicitly chooses "shared" per sandbox when
  they trust the workloads with each other. Implementation: extend
  `daft-agent` to manage N crun children with independent runtime
  specs; extend the workspace share with per-container subpaths;
  extend the sandbox-host CLI with an isolation flag. Open
  sub-questions: do siblings see each other's `/workspace`, or do
  we mount per-container subdirs? How does GPU scheduling work
  *inside* the VM — first-come-first-served on the driver, or do
  we expose a queue?

## How to update this plan

When you (human or agent) change direction, finish a milestone, or surface a
new open question, edit this file in the same commit. The first line of
CLAUDE.md points future agents here, so stale plans actively mislead.
