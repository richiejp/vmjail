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

## Status

- [x] Repository skeleton (Go layout, Nix flake, direnv, git hooks)
- [x] CONTRIBUTING.md
- [x] `daft` CLI scaffolding (cobra-based, with `completion` and `version` subcommands)
- [x] GitHub CI (`validate-build`, `validate-tag`)
- [ ] Define project domain and scope (what does `daft` actually do?)
- [ ] First feature slice

## Architecture

- `cmd/<name>/main.go` — entry points for binaries. Currently only `daft`.
- `internal/<pkg>` — non-exported library code; everything that isn't a `main`
  package or a top-level config file lives here.
- `version.txt` — single source of truth for the released version. Read by
  `flake.nix` (Nix package version) and embedded into the `daft` binary at
  build time.

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
