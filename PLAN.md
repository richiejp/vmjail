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
      KVM-gated `BenchmarkHelloBoot` end-to-end boot-time benchmark.
- [ ] HTTP/2 + mTLS auth: client ↔ orchestrator
- [ ] Workspace sync: desync chunks + daft-fuse

## Architecture

### Components

There are 4 main components:
- client CLI: runs on the user-host, performs I/O with the sandbox-hosts
- Orchestrator: Runs on any host, athenticates clients and sandbox controllers, provides a web UI with various information
- Sandbox Controller: Creates sandbox VMs and virtual networks, performs I/O with user-hosts and Sandbox Agents
- Sandbox Agent: Runs inside the sandbox VM and creates containers, performs I/O with the controller.

Note that the containers the sandbox agent creates are not a security boundary. They are just a convenient way to create a Linux userland that avoids
creating VM images.

### Concepts

Workspace:
- Directories and single files the user synchronises with the Sandbox. 
  Essentially like a docker bind mount or volume, except that changes are not automatically synchronised. The user chooses when to send (except when streaming) and receive.
  Changes are diffed so the user can see what has changed and to save network bandwidth
- Env vars sent to the sandbox
- State and data in the sandbox that is not necessarily retreived by the user, but persists
- Configuration of the sandbox and workspace itself

### file layout
- `cmd/<name>/main.go` — entry points for binaries. Currently only `daft`.
- `internal/<pkg>` — non-exported library code; everything that isn't a `main`
  package or a top-level config file lives here.
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

### MicroVM stack

- VMM: **Cloud Hypervisor** (Rust, LF, REST over Unix socket)
  - Only candidate covering virtiofs + vsock + virtio-pci/blk/serial + PCI/VFIO incl. NVIDIA GPUDirect P2P
  - Go control: vendor the OpenAPI spec, generate client with `oapi-codegen`; wrap behind `internal/vmm` interface
  - Fallbacks behind the same interface: Firecracker (no virtiofs/PCI, but snapshot-restore < 50 ms); QEMU q35 for exotic passthrough
  - Boot-to-agent target: 150–250 ms cold; sub-50 ms with snapshot-restore later
- Kernel: custom build, LLVM cross-compiled, stripped defconfig
  - Reuse the kernel-build approach from `richiejp/m`; replace its Zig init
  - On for CH guest: `CONFIG_VIRTIO_PCI`, `CONFIG_VIRTIO_FS`, `CONFIG_VSOCKETS`, `CONFIG_VIRTIO_VSOCKETS`, `CONFIG_FUSE_FS`, `CONFIG_VFIO_PCI`, `CONFIG_BPF`, `CONFIG_BPF_LSM`
  - Cmdline: `console=hvc0 reboot=t panic=-1 tsc=reliable clocksource=tsc random.trust_cpu=on`
- Init: **u-root** `cmds/core/init` only (`github.com/u-root/u-root`)
  - Pure Go; mounts essentials, reaps zombies, execs `-uinitcmd`
  - **No shell, no diagnostic tools shipped** — debugging is the agent's job over vsock
  - daft-agent launched as `-uinitcmd`
  - Replace with bespoke ~300-line Go init only if u-root blocks us

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

- What is the actual product? `daft` is currently a shell with no real
  subcommands beyond `completion` and `version`.
- Code coverage baseline: the pre-commit hook compares against `coverage.txt`
  in the repo root. Initial baseline is whatever the first run produces; we
  refuse new commits that drop coverage by more than 1.0 percentage points.

## How to update this plan

When you (human or agent) change direction, finish a milestone, or surface a
new open question, edit this file in the same commit. The first line of
CLAUDE.md points future agents here, so stale plans actively mislead.
