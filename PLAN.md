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
- [/] Tracing: daft-native cross-component tracing. **Slice 1
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
      automatically. **Remaining slices**:
      - Slice 2: orchestrator-side reaper for trace-id sets that
        haven't seen a span in a while (catch crashed producers
        whose buffer never made it across the wire).
      - Slice 4: in-VM agent → sandbox-host trace channel over
        vsock once vsock plumbing exists.
      - Slice 5 (optional): an OTel proxy-exporter as a separate
        component, translating our wire format to OTLP for users
        who want their existing observability stack to consume
        daft traces.
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
      **Remaining slices**:
      - Slice 3: FastCDC chunker behind the same Chunker
        interface (replace FixedSizeChunker).
      - Slice 4: `daft-fuse` (go-fuse v2) — a FUSE FS the
        sandbox-host mounts, that virtiofsd then re-exports into
        the VM. Reads resolve via the local CAS, fetching missing
        chunks from the user-host over HTTP/2. Policy hook for
        freeze-on-suspicious-IO lives here.
      - Slice 5: write overlay + `daft pull` diff workflow (the
        user-host pulling sandbox-side modifications back).

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
- **Workspace sync UX**: do we want a `daft push` / `daft pull` model
  (explicit, diff-visible, matches the "user controls when data moves"
  principle) or live virtiofs streaming with a "freeze on suspicious IO"
  hook (faster iteration, but the user trusts the policy engine to
  catch exfil)? PLAN.md currently assumes the explicit model — revisit
  when we have a real workload.
- **virtiofsd static-musl**: ~~missing libc symbol references~~ Done.
  Switched to `pkgsStatic.rustPlatform.buildRustPackage` with
  `LIBCAPNG_LINK_TYPE=static` + `pkgsStatic.libcap_ng`. virtiofsd is
  now `statically linked` like every other daft-sandboxd binary.
- **Boot-time gap**: measured ~310 ms vs the 150–250 ms target. Worth
  the kernel-config diet before snapshot-restore lands?

## How to update this plan

When you (human or agent) change direction, finish a milestone, or surface a
new open question, edit this file in the same commit. The first line of
CLAUDE.md points future agents here, so stale plans actively mislead.
