# Detailed resource-model reference

This older detailed reference describes how darkaifactory is built: the resource model, the
naming conventions, the control flow between components, and how the system
could integrate with Kubernetes if that path is taken. It is the standalone
reference for the project's vocabulary and conventions.

[ARCHITECTURE.md](../../ARCHITECTURE.md) is the compact architecture map and
[PLAN.md](../../PLAN.md) is the living plan. This document is retained while
its implementation-level material is split into bounded component pages. Some
target-shape and future-work statements below are historical and are not
authoritative over the linked architecture map or current source.

## Components

Four processes, each with one job:

| Process | Role | Identity |
|---|---|---|
| **`daft`** CLI | User-host client | mTLS leaf cert under `~/.config/daft` |
| **Orchestrator** | Authority over Workspaces and Sandboxes; binds Sandboxes to Nodes; speaks to clients over Connect-RPC; owns Phase C ingress | mTLS, optionally an internal CA |
| **`daft-sandboxd`** | Per-Node daemon. Runs Sandboxes (VMs + containers), reports status. Single-binary bundle including the kernel + initramfs + VMM | mTLS leaf cert; fingerprint is the Node's canonical id |
| **`daft-agent`** | In-VM init/agent. Spawns `crun`-managed containers, ships traces, and consumes host-provided virtiofs/vsock services | vsock to its sandbox-host; no TLS inside the VM |

The sandbox-host machine (the physical or cloud box running `daft-sandboxd`) is a
**Node**. Container boundaries inside the VM are not a security boundary —
the VM is. Containers exist for filesystem/userland convenience.

## Resource model

The target model has three first-class resources with the K8s-style triple
**metadata + spec + status**. Workspace implements that model today. Sandbox
has the wire shape but is currently a transient per-Launch object rather than
a persisted orchestrator resource. Node registration is still a flat wire
record; splitting it into metadata/spec/status is future work.

- `metadata` — identity, labels, audit timestamps, generation.
- `spec` — desired state. User-writable for Workspaces; controller-written
  (by the orchestrator's scheduler) for Sandboxes.
- `status` — observed state. Controller-owned, clients read but don't write.

### Workspace

User-owned, long-lived, declarative. The user edits a Workspace incrementally
(`daft workspace image …`, `daft workspace mount add …`) and Launches it.
Multiple Launches reuse the same Workspace; each one produces a fresh
**Sandbox** child resource derived from `Workspace.spec`.

```
Workspace
├── metadata { uuid, name, owner_fingerprint, generation,
│              created_at, updated_at }
├── spec     { image_ref, mounts[], env[], ports[], target_sandbox_fingerprint,
│              gpu_request, isolation, memory_bytes, cpu_cores,
│              nested_virtualization, privileged_fuse_mounts,
│              privileged_build, devcontainer_path }
└── status   { observed_generation, conditions[], current_sandbox_ref, lock }
```

Closest K8s analog: a single-replica Deployment + PVC. Not an exact match —
nothing in K8s is both "the persistent declared workload" and "the persistent
filesystem" simultaneously — but the spec/status mechanics are identical.

### Sandbox

Short-lived, controller-derived. One Sandbox per Launch. A Sandbox is the
**unit of execution**:

- **Current execution**: every Sandbox watch event boots one VM containing one
  container. The isolation value is carried on the wire but does not yet alter
  scheduling or packing.
- **Planned strict isolation** (`Sandbox.spec.isolation = STRICT`): one VM
  hosting one container.
- **Planned shared isolation** (`Sandbox.spec.isolation = SHARED`): multiple
  trusted sibling workloads may share a GPU-bound VM. This is Phase D3-D5,
  not a current security guarantee.

A Sandbox is always one container. Multi-container per Sandbox — K8s's
multi-container Pod semantics (init containers, sidecars, shared netns
between sibling containers) — is **not** in the model. That decision is
deferred until and unless serious Kubernetes integration is undertaken
(see [K8s integration paths](#kubernetes-integration-paths)).

```
Sandbox
├── metadata { uid, workspace, created_at, deletion_timestamp }
├── spec     { image_ref, mounts[], env[], ports[], gpu_request,
│              isolation, node, memory_bytes, cpu_cores,
│              nested_virtualization, privileged_fuse_mounts,
│              privileged_build, devcontainer_path }
└── status   { phase (Pending|Running|Exited|Killed|Unknown), exit_code,
│              conditions[], last_transition_time }
```

Closest K8s analog: a single-container Pod. The Sandbox's `metadata.uid`
plays the role of the dispatch correlation handle in the old design — the
identity is the resource itself, not a side-channel string.

### Node

Long-lived, registered. One Node per `daft-sandboxd` process. Identity is the
mTLS leaf-cert fingerprint of `daft-sandboxd` — the daemon doesn't pick its
own name. Capacity advertised at registration; runtime utilisation reported
on heartbeats.

```
Node (current flat registration)
└── { fingerprint, hostname, version, resources,
      registered_at, last_seen_at }

Target shape
├── metadata { fingerprint, version, registered_at }
├── spec     { capacity (cpus, memory_bytes, gpus...) }
└── status   { last_seen_at, current_load, live_sandboxes[], conditions[] }
```

Closest K8s analog: Node, almost exactly.

## Naming conventions

These hold across protos, Go types, and CLI surfaces. They are deliberately
K8s-flavoured to reduce vocabulary friction for anyone arriving from that
ecosystem.

| Convention | Example | Notes |
|---|---|---|
| Resource names | `Workspace`, `Sandbox`, `Node` | PascalCase singular. URL/CLI plural is the lowercase wordform (`workspaces`, `sandboxes`, `nodes`). |
| Proto field names | `lower_snake_case` | Generated Go uses lowerCamelCase getters (`GetImageRef()`). |
| Cross-resource pointers | `*Ref` suffix: `WorkspaceRef`, `NodeRef`, `SandboxRef` | Carries the strong id plus any commonly-needed denormalised fields. |
| Workspace lifecycle state | `conditions[]` array, **not** an enum | Each Condition is `{type, status, reason, message, last_transition_time, observed_generation}`. Types and reasons are PascalCase tokens. |
| Change tracking | `metadata.generation` | Increments on every Workspace spec change. `if_generation` Update/Patch preconditions are a future API, not implemented today. |
| Status freshness | `status.observed_generation` | Compared against `metadata.generation` to detect "edit pending" state. |
| Operational enums | `Isolation`, `Protocol`, `Phase` | Proto-native (closed sets). Reserved 0 = UNSPECIFIED. |
| Lifecycle "phase" string | `"Pending"`, `"Running"`, `"Exited"`, `"Killed"` | PascalCase string for forward-compat (new phases don't break old readers). Distinct from operational enums. |
| Action verbs | `Launch`, `Stop`, `Validate` | Reserved for non-CRUD operations. The current catalog exposes Create/Get/List plus focused spec-mutator RPCs; generic Update/Patch/Delete/Watch remain design vocabulary. |
| Daemon binary names | `daft`, `daft-sandboxd`, `daft-agent` | Independent of resource names — K8s precedent (kubelet ≠ Node). |
| Reserved field 0 | every enum starts at `*_UNSPECIFIED = 0` | Catches missing-field bugs at deserialisation. |

What the rename explicitly fixed (vs the prior vocabulary):

| Was | Now | Why |
|---|---|---|
| `Sandbox` (the daemon's registration record) | `Node` | "Sandbox" should be the unit of execution. |
| `SandboxControlService` | `NodeControlService` | Service is the Node API. |
| `Dispatch` (proto message) | `SandboxWatchEvent` carrying a `Sandbox` | Dispatch survives only as an English verb. |
| `dispatch_id` (correlation handle) | `Sandbox.metadata.uid` | The handle is the resource. |
| `Workspace.current_dispatch_id` | `Workspace.status.current_sandbox_ref` | Strong-typed `*Ref` instead of an opaque string. |
| `WorkspaceStatus` enum | `Workspace.status.conditions[]` | Conditions extend; enums break. |
| `sandbox-host` (English) | "node" (English) | One name for one concept. |
| `SandboxSpec` (workload spec inside Dispatch) | `SandboxSpec` (the spec of the Sandbox resource) | Same name, now correctly named after its resource. |

## Control flow

End-to-end, in resource terms:

```
user-host                    orchestrator                       node (sandboxd)
─────────                    ────────────                       ───────────────
daft workspace launch  ──▶   Workspace.Launch
                             ├── compose Sandbox.spec
                             │   from Workspace.spec
                             ├── resolve the targeted Node
                             │   (or the sole registered Node)
                             ├── construct a transient Sandbox
                             └── emit SandboxWatchEvent(ADDED)
                                 on this Node's watch stream
                                                          ─────▶ sandboxd sees
                                                                 a new Sandbox
                                                                 bound to it,
                                                                 boots the VM,
                                                                 runs the
                                                                 container
                                                          ◀───── periodic
                                                                 Heartbeat with
                                                                 live_sandboxes
                             ◀── reconciler compares the
                                 Workspace's current Sandbox ref
                                 with the Node's LiveSandbox set
                                 and updates Workspace.status
```

There is not yet a scheduler or persistent Sandbox store. B6 adds durable
declarative state; GPU/shared-VM scheduling is Phase D.

### Watch + reconcile

`NodeControlService.RegisterNode` is the one long-lived downward control
stream. Each `daft-sandboxd` opens it once at startup and holds it for its
lifetime. Other server/bidirectional streams exist for observer output, port
forwarding, and exec/attach data. The orchestrator pushes these `NodeEvent`
variants down the registration stream:

| Variant | Carries | When |
|---|---|---|
| `SubscriptionReady` | `accepted_at`, `protocol_version` | Once, first event on every stream. |
| `SandboxWatchEvent` | `EventType (ADDED/MODIFIED/DELETED/BOOKMARK)`, `Sandbox` | Whenever a Sandbox bound to this Node changes. |
| `OpenPortRequest` | session id + workspace/port | Ask the Node to open a reverse port stream. |
| `AttachRequest` | session id + Sandbox/command/TTY shape | Ask the Node to open an exec stream. |
| `ObserverRequest` | session id + Sandbox | Ask the Node to stream observer data. |

The Node reacts to `SandboxWatchEvent`s by bringing local state into alignment
with the full Sandbox object. Full-object events make handling idempotent, but
there is no resource-version cursor or persisted replay log yet; reconnect
replay semantics depend on the future persistence work.

### Heartbeats as reconciliation

`HeartbeatRequest` carries `repeated LiveSandbox live_sandboxes`. Every
heartbeat is therefore a liveness-reconciliation tick:

1. Orchestrator diffs the Node's reported `live_sandboxes` against the set
   of Sandboxes it has bound to this Node.
2. If an expected uid is absent for three consecutive heartbeats, clear the
   Workspace's current Sandbox and set `Dispatched=False` with
   `reason=SandboxLost`.
3. A periodic sweep marks workspaces on Nodes beyond the heartbeat grace
   period `NodeUnreachable`.

Reported terminal phases also clear the Workspace reference. Orphan Sandbox
cleanup and orchestrator-restart recovery are not implemented yet; the latter
depends on B6 persistence. There is no separate exit-event RPC.

### Pre-action reconcile

`Launch` invokes the pre-action reconciler before reading status. Extending
that hook to Stop and status reads remains follow-up work.

## Protocol

- **Transport**: HTTP/2 over TLS 1.3, mTLS-authenticated. Connect-RPC
  (`connectrpc.com/connect`) for method dispatch. Browser- and
  curl-debuggable, no gRPC runtime.
- **One persistent control stream per Node**: `RegisterNode`. Downward-only,
  carries `NodeEvent`s. Nodes use unary Heartbeat plus short-lived
  bidirectional streams for port, attach, and observer sessions.
- **Inside-VM**: virtio-vsock (`AF_VSOCK`), Connect-RPC over it. Lets a
  workload boot without a virtual NIC if it doesn't need one.
- **Protocol versioning**: `ProtocolVersion{major, minor, patch}` exchanged
  on `RegisterNode`. Major mismatch refused on both sides; minor/patch
  informational. Pinned at 1.0.0 so a peer omitting the field
  (proto3 zero ⇒ major=0) is rejected.

## Identity and trust

- **Self-signed Ed25519 mTLS** end-to-end. Trust is established explicitly
  out of band with `daft auth approve`; automated first-contact approval is
  not implemented.
- **Caller identity = leaf-cert fingerprint**. There is no login RPC and no
  user/account abstraction. The TLS connection is the identity.
- **The Node's fingerprint is its canonical id.** It can't rename itself
  by reconnecting — the cert is the thing.
- **Workspace owner**: the fingerprint of the user-host that minted the
  Workspace (`metadata.owner_fingerprint`). Cross-client adoption without
  changing the owner is planned in Phase F1.
- **Optional internal CA**: the orchestrator may later issue short-lived
  component certs for `sandboxd` registration, replacing the manual approve
  step for fleet deployment.

## File layout

| Path | Contents |
|---|---|
| `cmd/daft/` | User-host CLI entry point. |
| `cmd/daft-sandboxd/` | Node daemon. Single binary bundling cloud-hypervisor + virtiofsd + kernel + initramfs. |
| `cmd/daft-agent/` | In-VM init/agent. Linux-only; runs as PID 1's child via u-root `-uinitcmd`. |
| `internal/orchestrator/` | Orchestrator server + RPC handlers. |
| `internal/nodectrl/` *(renamed from `sandboxctrl/`)* | Node registry, the in-memory side of `NodeControlService`. |
| `internal/nodectrlclient/` *(renamed from `sandboxctrlclient/`)* | Sandboxd-side client: dials the orchestrator, holds the watch stream. |
| `internal/workspacecatalog/` | Workspace store + state machine. |
| `internal/sandbox/` | The VMM + agent supervision layer (the actual VM-booting code). |
| `internal/sandboxexec/` | The Node's executor: consumes SandboxWatchEvents, drives `internal/sandbox`. |
| `internal/reconciler/` | Heartbeat, pre-Launch, and periodic Workspace liveness reconciliation. |
| `internal/workspacebroker/`, `internal/cas/` | Brokered file synchronization and content-addressed storage. |
| `internal/sandboxgateway/`, `internal/sandboxattach/` | Reverse port and exec/attach session bridges. |
| `internal/sandboxnet/`, `internal/mitmproxy/` | Per-VM networking, default-deny egress, and cached HTTP(S) proxying. |
| `internal/observability/`, `internal/trace/` | Host resource observation and cross-component tracing. |
| `internal/devcontainer/`, `internal/devcontainerspec/` | Devcontainer parsing, projection, and dispatch-time overlays. |
| `internal/imagestore/` | OCI image cache on the Node. |
| `proto/nodectrl/v1/` *(renamed from `sandboxctrl/v1/`)* | `NodeControlService` definitions. |
| `proto/sandbox/v1/` *(new, split out)* | `Sandbox` resource definitions. |
| `proto/workspace/v1/` | Workspace resource + catalog service. |
| `internal/genpb/` | Generated Go from `make proto`. Committed. |
| `third_party/` | Vendored upstream sources (Linux kernel, Cloud Hypervisor, virtiofsd, u-root, crun). |

## What the model leaves out, deliberately

- **Multi-container per Sandbox.** A Sandbox is one container. K8s Pods
  with init containers / sidecars / shared netns siblings don't translate
  until a) we want to be a real K8s runtime, or b) a user workflow
  demands it. Until then, "Sandbox = single-container Pod" is the contract
  and we make no decisions about Pod-internal semantics.
- **Symmetric two-way file sync.** Workspace synchronization is explicit
  push/pull, with read-only mounts excluded from pull. Per-client directional
  flags are planned in Phase F. Bidi-sync would be delegated to existing tools
  via the future Phase H Docker-API endpoint
  (Mutagen, VS Code Dev Containers, JetBrains Gateway).
- **URL-as-identity / GVK addressing.** Connect-RPC method-per-RPC is
  fine for our scale; we don't need REST URLs or apiVersion negotiation.
- **Sub-resources** (K8s `/status` separate from `/spec`). We have one
  writer per status field, so the RBAC separation that motivates K8s
  sub-resources doesn't apply.
- **Server-side apply / field managers.** Not enough writers to need
  three-way merge.

## Kubernetes integration paths

The model above is K8s-shaped to make integration cheap *if* it is ever
wanted. Integration is **not in scope** for the current plan — this section
is the design space we want to keep open.

Three paths, in order of how much of the K8s surface darkaifactory would
own:

### Path 1 — containerd runtime shim (Kata-style)

Closest prior art: [Kata Containers](https://katacontainers.io). On each
Kubernetes node:

```
kubelet ──CRI──▶ containerd ──runtime-v2 shim──▶ daft-sandboxd (per pod)
                     │
                     └── ImageService, snapshotting, CRI compliance
```

What darkaifactory implements: a `containerd-shim-daft-v2` binary speaking
the containerd **runtime-v2 TaskService** gRPC interface (`Create`, `Start`,
`Delete`, `Exec`, `Kill`, `State`, `Wait`, `Pause`, `Resume`, `Stats`). One
shim process per Pod, spawned by containerd. The shim translates each
TaskService method into operations on a daft Sandbox.

What darkaifactory does **not** implement: CRI, kubelet, image pulling,
layer storage, snapshotting, node registration with the apiserver. containerd
and kubelet own those.

**Required gaps to close**:
- Multi-container per VM (so one Pod with N containers maps to one Sandbox
  with N containers; the largest single piece of work).
- OCI runtime spec ↔ `Sandbox.spec` translator.
- Exec/attach/port-forward streaming exposed by the shim (via the existing
  vsock channel back into the agent).
- CNI plugin invocation on RunPodSandbox for Pod IP assignment.

**Tradeoffs**: smallest new surface (~3–5k Go lines plus the multi-container
work). Inherits a known-good integration with the K8s ecosystem. Requires
containerd already running on the node.

**Recommend when**: the goal is "users with existing K8s clusters can opt
into VM-isolated pods."

### Path 2 — Virtual Kubelet provider

Closest prior art: Azure ACI, AWS Fargate Virtual Kubelet providers. The
orchestrator presents itself as a Node to a real Kubernetes apiserver:

```
apiserver ◀── watches Pods, reports Node status ── virtual-kubelet binary
                                                       │
                                                       └─▶ daft orchestrator
```

What darkaifactory implements: the Virtual Kubelet
`PodLifecycleHandler` + `NodeProvider` interfaces (`CreatePod`, `UpdatePod`,
`DeletePod`, `GetPod`, `GetPodStatus`, `GetPods`, `Ping`,
`NotifyNodeStatus`), plus streaming endpoints (`GetContainerLogs`,
`RunInContainer`, `GetStatsSummary`).

Each Pod becomes a Workspace + Sandbox pair under the hood. Mapping is
nearly mechanical given the spec/status shape on both sides.

**Tradeoffs**: the apiserver sees one virtual Node per orchestrator. Bin-
packing across the underlying daft Nodes is done by daft's own scheduler,
not the K8s scheduler — many users see this as a feature (daft is
workspace-aware in ways K8s isn't), but `kubectl get nodes` won't show the
real fleet, and cluster autoscaler / topology spread / node affinity all
operate at the "one virtual Node" granularity.

**Recommend when**: the goal is "darkaifactory is one workload-aware
endpoint exposed to a K8s cluster, scheduled internally."

### Path 3 — Direct CRI implementation

Implement the kubelet-facing **CRI** gRPC surface directly:
`RuntimeService` (≈30 RPCs) + `ImageService` (≈5 RPCs). No containerd in
the path.

**Tradeoffs**: largest new surface — CRI is underspecified in places and
empirically defined by what real kubelets do. CRI-O, the reference CRI
implementation, is 50k+ lines of Go. Avoid unless there's a compelling
reason not to use containerd (Path 1).

**Recommend when**: rarely. Path 1 dominates this option in almost every
dimension.

### What gaps exist regardless of path

| Capability | Status | Effort |
|---|---|---|
| Multi-container per Sandbox | Not supported | ~1–2k Go in `internal/sandboxboot` + agent; supervise N `crun` instances sharing the VM's netns. |
| Pod-IP allocation (CNI) | Not exposed | ~500 Go lines + a CNI plugin invocation on Sandbox bring-up. |
| Image pull as a separate service | Implemented monolithically per dispatch | Defer to containerd (Path 1) or extract a separate service. |
| Volume drivers beyond bind | `Mount` covers hostPath-equivalent only | CSI integration if PVCs are required; ~1–2k. |
| Exec / attach / port-forward streaming | Implemented for single-container Sandboxes | Reuse these streams when adding multi-container addressing. |
| Per-Sandbox VM resource limits | CPU and memory implemented; per-container limits not exposed | Pass through OCI runtime spec to `crun` for shared-VM containers. |
| Stats per container | Heartbeat carries Sandbox phase, while host observation is streamed separately | Add per-container accounting for shared-VM mode. |
| Logging | No stream | `GetContainerLogs(sandbox_uid)` server-streaming RPC. |
| Security context | Mapped to `Isolation` | Pass OCI spec through; `crun` enforces. |

### Recommendation if K8s integration is ever taken on

**Path 1** for "drop into an existing cluster"; **Path 2** for "expose
darkaifactory as one K8s-visible workload sink." Multi-container per
Sandbox is on the critical path for either, and is the load-bearing
prerequisite — the rest is mechanical glue.

## See also

- [PLAN.md](../../PLAN.md) — slice list and current direction.
- [CONTRIBUTING.md](../../CONTRIBUTING.md) — contributor rules (apply to agents too).
- `proto/` — wire-level source of truth.
