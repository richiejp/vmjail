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

## Active milestone: overseer-controlled agent runs

The target interaction is: “develop `/project/path` in darkaifactory on
`user@node`.” A trusted overseer then installs or updates the static Node binary
over SSH, establishes trust, applies and pushes a workspace, starts a sandboxed
coding agent with one-shot OAuth delivery, monitors durable events and bounded
observability, and tears down or reports a stuck run.

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
- Real daemon/MCP lifecycle tests for workspace and AgentRun paths.
- Adaptive SSH Node deployment skill and deterministic probe, stage, verify,
  upgrade, and rollback scripts.
- Validated overseer workflow skill composing adaptive Node deployment with
  retry-safe Workspace and AgentRun MCP operations, durable cursor recovery,
  bounded monitoring, and verified teardown.
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

Remaining:

- Prove the full overseer recipe across a remote Node: install, trust,
  apply/push, start, poll/result, teardown, retry, and recovery.
- Document takeover requirements for a future terminal/web attachment path;
  direct interactive control remains deferred.

## Other open product work

- Durable control-plane and manifest persistence beyond the AgentRun/operator
  stores.
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
