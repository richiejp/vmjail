# Architecture

This document describes how darkaifactory is built: the resource model, the
naming conventions, the control flow between components, and how the system
could integrate with Kubernetes if that path is taken. It is the standalone
reference for the project's vocabulary and conventions.

[PLAN.md](PLAN.md) is the living plan and slice list; this document is the
shape the plan is converging on.

## Components

Four processes, each with one job:

| Process | Role | Identity |
|---|---|---|
| **`daft`** CLI | User-host client | mTLS leaf cert under `~/.config/daft` |
| **Orchestrator** | Authority over Workspaces and Sandboxes; binds Sandboxes to Nodes; speaks to clients over Connect-RPC; owns Phase C ingress | mTLS, optionally an internal CA |
| **`sandboxd`** | Per-Node daemon. Runs Sandboxes (VMs + containers), reports status. Single-binary bundle including the kernel + initramfs + VMM | mTLS leaf cert; fingerprint is the Node's canonical id |
| **`daft-agent`** | In-VM init/agent. Spawns `crun`-managed containers, ships traces back, drives daft-fuse | vsock to its sandbox-host; no TLS inside the VM |

The sandbox-host machine (the physical or cloud box running `sandboxd`) is a
**Node**. Container boundaries inside the VM are not a security boundary —
the VM is. Containers exist for filesystem/userland convenience.

## Resource model

Three first-class resources. Each has the K8s-style triple
**metadata + spec + status**:

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
├── metadata { uuid, name, owner_fingerprint, generation, labels,
│              created_at, updated_at, deletion_timestamp, finalizers }
├── spec     { image_ref, mounts[], env[], ports[], target_sandbox,
│              gpu_request, isolation, pending_build_id }
└── status   { observed_generation, conditions[], current_sandbox_ref, lock }
```

Closest K8s analog: a single-replica Deployment + PVC. Not an exact match —
nothing in K8s is both "the persistent declared workload" and "the persistent
filesystem" simultaneously — but the spec/status mechanics are identical.

### Sandbox

Short-lived, controller-derived. One Sandbox per Launch. A Sandbox is the
**unit of execution**:

- **Strict isolation** (`Sandbox.spec.isolation = STRICT`): exactly one VM
  hosting exactly one container. Maps 1:1 to a single-container Pod.
- **Shared isolation** (`Sandbox.spec.isolation = SHARED`): the scheduler may
  pack multiple Sandboxes into one VM. Inter-Sandbox isolation in this mode
  is whatever the container runtime provides; cross-Workspace packing is
  permitted only between Sandboxes owned by the same identity. Default for
  CPU-mode workspaces.

A Sandbox is always one container. Multi-container per Sandbox — K8s's
multi-container Pod semantics (init containers, sidecars, shared netns
between sibling containers) — is **not** in the model. That decision is
deferred until and unless serious Kubernetes integration is undertaken
(see [K8s integration paths](#kubernetes-integration-paths)).

```
Sandbox
├── metadata { uid, workspace_ref, created_at, deletion_timestamp }
├── spec     { image_ref, mounts[], env[], ports[], gpu_request,
│              isolation, node_ref }
└── status   { phase (Pending|Running|Exited|Killed), exit_code,
│              conditions[], last_transition_time }
```

Closest K8s analog: a single-container Pod. The Sandbox's `metadata.uid`
plays the role of the dispatch correlation handle in the old design — the
identity is the resource itself, not a side-channel string.

### Node

Long-lived, registered. One Node per `sandboxd` process. Identity is the
mTLS leaf-cert fingerprint of the `sandboxd` — `sandboxd` doesn't pick its
own name. Capacity advertised at registration; runtime utilisation reported
on heartbeats.

```
Node
├── metadata { fingerprint, version, registered_at }
├── spec     { capacity (cpus, memory_bytes, gpus...) }
└── status   { last_seen_at, current_load, live_sandboxes[],
               conditions[] }
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
| Lifecycle state | `conditions[]` array, **not** an enum | Each Condition is `{type, status, reason, message, last_transition_time, observed_generation}`. Types and reasons are PascalCase tokens. |
| Optimistic concurrency | `metadata.generation` | Increments on every spec change. `if_generation` preconditions on Update/Patch. |
| Status freshness | `status.observed_generation` | Compared against `metadata.generation` to detect "edit pending" state. |
| Operational enums | `Isolation`, `Protocol`, `Phase` | Proto-native (closed sets). Reserved 0 = UNSPECIFIED. |
| Lifecycle "phase" string | `"Pending"`, `"Running"`, `"Exited"`, `"Killed"` | PascalCase string for forward-compat (new phases don't break old readers). Distinct from operational enums. |
| Action verbs | `Launch`, `Stop`, `Validate` | Reserved for non-CRUD operations. CRUD is `Create`/`Get`/`List`/`Update`/`Patch`/`Delete`/`Watch`. |
| Daemon binary names | `daft`, `sandboxd`, `daft-agent` | Independent of resource names — K8s precedent (kubelet ≠ Node). |
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
                             ├── scheduler picks Node
                             │   (sets Sandbox.spec.node_ref)
                             ├── create Sandbox resource
                             └── emit SandboxWatchEvent(ADDED)
                                 on this Node's watch stream
                                                          ─────▶ sandboxd sees
                                                                 a new Sandbox
                                                                 bound to it,
                                                                 boots the VM,
                                                                 runs the
                                                                 container
                                                          ◀───── periodic
                                                                 Heartbeat:
                                                                 NodeStatus +
                                                                 live_sandboxes
                             ◀── reconciler diffs
                                 Sandbox.status vs the
                                 Node's reported set,
                                 updates conditions and
                                 Workspace.status
```

### Watch + reconcile

There is one server-streaming RPC in the system: `NodeControlService.
RegisterNode`. Each `sandboxd` opens it once at startup and holds it for its
lifetime. The orchestrator pushes `NodeEvent`s down it:

| Variant | Carries | When |
|---|---|---|
| `SubscriptionReady` | `accepted_at`, `protocol_version` | Once, first event on every stream. |
| `SandboxWatchEvent` | `EventType (ADDED/MODIFIED/DELETED/BOOKMARK)`, `Sandbox` | Whenever a Sandbox bound to this Node changes. |

The Node reacts to `SandboxWatchEvent`s the way a kubelet reacts to Pod
watch events: bring local state into alignment with declared state. The
event payload is the full Sandbox object, not a delta — the protocol is
level-triggered, replay-safe, and idempotent.

### Heartbeats as reconciliation

`HeartbeatRequest` carries the Node's current `NodeStatus`, which includes
`repeated SandboxStatus live_sandboxes`. Every heartbeat is therefore a full
reconciliation tick:

1. Orchestrator diffs the Node's reported `live_sandboxes` against the set
   of Sandboxes it has bound to this Node.
2. Sandboxes the orchestrator thinks are RUNNING but the Node doesn't know
   about → mark `phase = Exited` (or `Killed` if context indicates) and
   advance the workspace status.
3. Sandboxes the Node reports but the orchestrator has marked terminal →
   instruct the Node to stop them on the next event (cleanup of a leaked
   instance).

This single mechanism handles: missed exit signals, sandboxd restart with
amnesia, orchestrator restart, VM killed externally, host reboot. There is
no separate "exit event" RPC — heartbeats are the safety net. A future
optimisation could add a unary `ReportSandboxExit` for prompt notification
without breaking this contract.

### Pre-action reconcile

Before any status-sensitive RPC (`Launch`, `Stop`, status reads from the
CLI), the orchestrator reconciles the target resource first. This closes
the TOCTOU window between a stale cache and the action — e.g. Stop on a
Sandbox that's already gone returns immediately rather than firing a
pointless control event.

## Protocol

- **Transport**: HTTP/2 over TLS 1.3, mTLS-authenticated. Connect-RPC
  (`connectrpc.com/connect`) for method dispatch. Browser- and
  curl-debuggable, no gRPC runtime.
- **One persistent stream per Node**: `RegisterNode`. Downward-only, carries
  `NodeEvent`s. No upward stream — Nodes that need to push to the
  orchestrator use unary RPCs (`Heartbeat` today; `ReportSandboxExit` if it
  lands later).
- **Inside-VM**: virtio-vsock (`AF_VSOCK`), Connect-RPC over it. Lets a
  workload boot without a virtual NIC if it doesn't need one.
- **Protocol versioning**: `ProtocolVersion{major, minor, patch}` exchanged
  on `RegisterNode`. Major mismatch refused on both sides; minor/patch
  informational. Pinned at 1.0.0 so a peer omitting the field
  (proto3 zero ⇒ major=0) is rejected.

## Identity and trust

- **Self-signed Ed25519 mTLS** end-to-end. TOFU + `daft auth approve` on
  first contact, with a timeout window.
- **Caller identity = leaf-cert fingerprint**. There is no login RPC and no
  user/account abstraction. The TLS connection is the identity.
- **The Node's fingerprint is its canonical id.** It can't rename itself
  by reconnecting — the cert is the thing.
- **Workspace owner**: the fingerprint of the user-host that minted the
  Workspace (`metadata.owner_fingerprint`). Adoption (Phase F1) lets a
  second user-host attach to the same Workspace without changing the owner.
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
| `internal/sandboxstore/` *(new)* | Sandbox store (the per-Launch resource). |
| `internal/sandbox/` | The VMM + agent supervision layer (the actual VM-booting code). |
| `internal/sandboxexec/` | The Node's executor: consumes SandboxWatchEvents, drives `internal/sandbox`. |
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
- **Symmetric two-way file sync.** Workspace mounts are asymmetric
  (push-only / pull-only / explicit `daft workspace pull`). Bidi-sync is
  delegated to existing tools via the Phase H Docker-API endpoint
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
| Exec / attach / port-forward streaming | Not exposed | New RPCs on `NodeControlService`; reuses the existing vsock channel inside the VM. |
| Per-container resource limits | `GpuRequest` + isolation only | Pass through OCI runtime spec to `crun`; ~200 lines mapping. |
| Stats per container | Heartbeat carries host load only | cAdvisor-equivalent inside the guest, surfaced through the heartbeat. |
| Logging | No stream | `GetContainerLogs(sandbox_uid)` server-streaming RPC. |
| Security context | Mapped to `Isolation` | Pass OCI spec through; `crun` enforces. |

### Recommendation if K8s integration is ever taken on

**Path 1** for "drop into an existing cluster"; **Path 2** for "expose
darkaifactory as one K8s-visible workload sink." Multi-container per
Sandbox is on the critical path for either, and is the load-bearing
prerequisite — the rest is mechanical glue.

## See also

- [PLAN.md](PLAN.md) — slice list and current direction.
- [CONTRIBUTING.md](CONTRIBUTING.md) — contributor rules (apply to agents too).
- `proto/` — wire-level source of truth.
