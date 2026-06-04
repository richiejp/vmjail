// Package sandboxexec is the sandbox-host-side executor: it
// receives Dispatch events from the orchestrator's
// NodeControlService and runs the corresponding workload by
// driving the existing internal/sandbox boot pipeline.
//
// When spec.image_ref is set, the executor pulls the image into
// the local imagestore, bundles it, and boots a VM that runs the
// container (via internal/sandboxboot). When image_ref is empty,
// the executor falls back to a bare Hello VM — useful for the
// smoke-test path the orchestrator uses to verify a sandbox is
// alive.
package sandboxexec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/localai-org/darkaifactory/internal/agent"
	"github.com/localai-org/darkaifactory/internal/assert"
	nodectrlv1 "github.com/localai-org/darkaifactory/internal/genpb/nodectrl/v1"
	sandboxv1 "github.com/localai-org/darkaifactory/internal/genpb/sandbox/v1"
	"github.com/localai-org/darkaifactory/internal/observability"
	"github.com/localai-org/darkaifactory/internal/sandbox"
	"github.com/localai-org/darkaifactory/internal/sandboxattach"
	"github.com/localai-org/darkaifactory/internal/sandboxboot"
	"github.com/localai-org/darkaifactory/internal/sandboxgateway"
	"github.com/localai-org/darkaifactory/internal/sandboxnet"
	"github.com/localai-org/darkaifactory/internal/trace"
	"github.com/localai-org/darkaifactory/internal/workspace"
)

// Phase tokens reported on heartbeat. Match the PascalCase values
// documented on daft.sandbox.v1.SandboxStatus.phase and read by the
// orchestrator's reconciler.
const (
	phasePending = "Pending"
	phaseRunning = "Running"
	phaseExited  = "Exited"
	phaseKilled  = "Killed"
)

// Assembler is the seam through which the Executor turns an OCI
// image reference + spec-derived BootOptions into the bootable
// sandbox.Config + a deferred unlock for the bundle lock.
// Production wires sandboxboot.BuildBundleConfig; tests pass fakes
// that don't touch the imagestore.
type Assembler func(ctx context.Context, ref string, opts sandboxboot.BootOptions) (sandbox.Config, func(), error)

// MountMaterializer is the node's mount-sync surface, shared between the
// executor and the CAS-backed workspace.Server. MaterializeMount
// reconstructs a previously-pushed workspace mount from the node's
// persistent CAS into a host backing directory, with no network
// round-trip (Slice S3): syncKey is workspace.SyncKey(workspace_uuid,
// in-VM target); dest is the absolute backing dir to populate (created
// if absent); applied is false when nothing was pushed under syncKey,
// so the mount boots as an empty read-write tree.
//
// SetLiveMount / ClearLiveMount bracket the lifetime of a running VM's
// writable mount (Slice S4): while a (syncKey → dir) entry is live, a
// `daft workspace pull` re-manifests dir so in-VM writes surface,
// instead of the last-pushed snapshot. ClearLiveMount is idempotent.
// Production wires the node's workspace.Server.
type MountMaterializer interface {
	MaterializeMount(ctx context.Context, syncKey, dest string) (applied bool, err error)
	SetLiveMount(syncKey, dir string)
	ClearLiveMount(syncKey string)
}

// Runner is the function the Executor calls to boot a VM. The
// default in production is sandbox.Serve — the long-running
// variant that blocks on ctx.Done OR VMM exit, suitable for
// interactive workloads where the container outlives any banner
// handshake. Tests inject a fake so they don't need /dev/kvm.
type Runner func(ctx context.Context, w io.Writer, cfg sandbox.Config) error

// liveSandbox tracks one sandbox the Executor is supervising.
// Reported as a snapshot on each heartbeat (Slice B4d).
type liveSandbox struct {
	uid      string
	phase    string
	exitCode int32
}

// Executor implements nodectrlclient.SandboxEventHandler. One
// Executor per sandboxd process — concurrent dispatches share the
// same baseline config and stdout writer.
type Executor struct {
	base      sandbox.Config
	stdout    io.Writer
	run       Runner
	assembler Assembler

	// bridger and vsockDir are populated only when the orchestrator-
	// driven port-ingress path is wired (Slice C1.3b). When nil/empty
	// the executor falls back to the pre-C1 behaviour: no Vsock
	// device on the dispatched Config and no port-route registration.
	bridger  *sandboxgateway.Bridger
	vsockDir string
	// attachRegistry, when non-nil, is the per-sandbox attach-route
	// table the executor populates on every dispatch (Slice D2b.3b).
	// Each route's dialer dials the per-Sandbox hybrid-vsock UDS at
	// agent.DefaultAttachVsockPort and runs sandboxattach.OpenSession
	// to wrap the framed conn as an AttachSession. Sharing
	// VsockDir with bridger is enforced by New().
	attachRegistry *sandboxgateway.AttachRegistry
	// observerRegistry, when non-nil, is the per-sandbox observer-log
	// table the executor populates on dispatch (sandbox_uid → the
	// dispatched Config.ObserverLogPath). The orchestrator-driven
	// ObserverStream handler resolves a uid to a path to tail. Only
	// dispatches whose ObserverLogPath is set register a route.
	observerRegistry *sandboxgateway.ObserverRegistry
	// summaryRegistry, when non-nil, is the per-sandbox build-progress
	// summary table the executor populates on dispatch (sandbox_uid → the
	// host path of the summary JSON the guest writes onto the observe
	// share). The ObserveSummary handler resolves a uid to a path to read.
	// Only dispatches that set up the observe share register a route.
	summaryRegistry *sandboxgateway.SummaryRegistry
	// observeSummary opts every image-backed dispatch into the Tier-2
	// observe share: the open-snoop eBPF gadget + the ~1 Hz build-progress
	// summary, written onto a small writable virtiofs share the host reads.
	// Off by default because it adds a gadget load + a virtiofs device to
	// each VM's boot; the operator turns it on when build observability is
	// worth that cost. Requires observerDir (the share's host backing dir).
	observeSummary bool
	// netEnabled toggles the sandboxnet listener wiring on every
	// dispatched VM (Slice D6c.2 step 3). When true, resolveBootConfig
	// sets cfg.Vsock.Net so the agent dials the host-side sandboxnet
	// stack (on the fixed DefaultNetVsockPort) at boot.
	netEnabled bool
	// egressProxy, when non-nil, drives Phase-J proxy-env + CA
	// injection on every image-backed dispatch (see Config.EgressProxy).
	egressProxy *EgressProxy
	// mountMaterializer and mountBackingDir are the Slice S3 launch-time
	// mount wiring. When set, every image-backed dispatch materialises
	// each spec mount from the node's persistent CAS into a per-Sandbox
	// backing dir under mountBackingDir and exports that dir via
	// virtiofsd (instead of treating the mount's logical Source as a
	// host path). nil materializer keeps the pre-S3 passthrough (Source
	// is taken as a host path) so unit tests and the smoke path are
	// unaffected.
	mountMaterializer MountMaterializer
	mountBackingDir   string
	// observerDir, when non-empty, is the absolute parent directory
	// under which each dispatch gets a per-uid host-side observer log
	// (<observerDir>/<uid>/observer.jsonl). resolveBootConfig defaults
	// the dispatched Config.ObserverLogPath to it unless the baseline
	// already carries one. Empty disables the default (the pre-Tier-1
	// behaviour); observability is best-effort and never fails a
	// dispatch.
	observerDir string
	// nextCID hands out fresh AF_VSOCK Context IDs for each dispatch.
	// Initialised to 2 so the first .Add(1) returns 3 — the lowest
	// CID the ABI allows for a guest (0/1/2 are reserved).
	nextCID atomic.Uint32

	mu   sync.Mutex
	live map[string]*liveSandbox
}

// Config bundles the Executor's dependencies. The baseline
// sandbox.Config is the one resolved at startup from DAFT_*
// (used for the bare-Hello fallback when image_ref is empty);
// Assembler is the image-ref → bootable-config seam used when
// image_ref is set. Runner is the function that actually boots
// the VM — production wiring uses sandbox.Serve (long-running).
type Config struct {
	Base      sandbox.Config
	Stdout    io.Writer
	Runner    Runner
	Assembler Assembler
	// Bridger, when non-nil, is the per-(workspace, port) routing
	// table the executor populates on every dispatch with a
	// DialGuestVsock closure per spec.ports entry. Slice C1.3c's
	// orchestrator-driven ingress handler calls Bridger.Bridge to
	// reach the running VM. Leave nil for tests/baselines that don't
	// exercise ingress.
	Bridger *sandboxgateway.Bridger
	// AttachRegistry, when non-nil, is the per-sandbox attach-route
	// table the executor populates with a vsock-dial closure on
	// every dispatch (Slice D2b.3b). The orchestrator-driven
	// AttachStream handler dispatches through it (via the
	// AttachBridger interface) to reach the agent's attach listener
	// inside the right VM. Requires VsockDir (same reason as
	// Bridger). Leave nil for tests/baselines that don't exercise
	// attach.
	AttachRegistry *sandboxgateway.AttachRegistry
	// ObserverRegistry, when non-nil, is the per-sandbox observer-log
	// table the executor populates on every dispatch whose
	// ObserverLogPath is set (sandbox_uid → path). The
	// orchestrator-driven ObserverStream handler resolves through it to
	// tail a running sandbox's log. Leave nil for tests/baselines that
	// don't exercise observer-tail.
	ObserverRegistry *sandboxgateway.ObserverRegistry
	// SummaryRegistry, when non-nil, is the per-sandbox build-progress
	// summary table the executor populates on every dispatch that sets up
	// the observe share (sandbox_uid → host summary path). The
	// ObserveSummary handler resolves through it to read a running
	// sandbox's summary. Requires ObserverDir (the observe share's host
	// backing dir lives under it). Leave nil to disable the summary route.
	SummaryRegistry *sandboxgateway.SummaryRegistry
	// ObserveSummary opts every image-backed dispatch into the Tier-2
	// observe share (open-snoop gadget + build-progress summary). Off by
	// default because it costs a gadget load + a virtiofs device per VM;
	// `daft-sandboxd serve` flips it on from DAFT_OBSERVE_SUMMARY. Requires
	// ObserverDir and SummaryRegistry to be useful.
	ObserveSummary bool
	// VsockDir is the absolute parent directory where per-Sandbox
	// hybrid-vsock UDS files live (one per running VM, named after
	// the Sandbox uid). Required when Bridger or AttachRegistry is
	// non-nil; ignored otherwise. Typically `/var/run/daft-sandboxd`
	// in production.
	VsockDir string
	// NetEnabled, when true, wires a sandboxnet-backed AF_VSOCK
	// listener into every dispatched VM so the in-VM agent's tap
	// bridge has somewhere to dial. Implies a per-Sandbox Vsock
	// device (allocated like the attach/bridger paths). Production
	// `daft-sandboxd serve` enables it; unit tests leave it off so
	// they don't have to wire a real sandboxnet stack.
	NetEnabled bool
	// EgressProxy, when non-nil, makes every image-backed dispatch
	// route guest TLS egress through the host-side caching proxy:
	// the executor injects http(s)_proxy + NIX_SSL_CERT_FILE /
	// SSL_CERT_FILE into the container env and writes the proxy CA
	// into the bundle rootfs. Leave nil to keep direct egress (the
	// pre-Phase-J behaviour).
	EgressProxy *EgressProxy
	// MountMaterializer, when non-nil, materialises each image-backed
	// dispatch's spec mounts from the node's persistent CAS into host
	// backing dirs at launch (Slice S3). Production wires the node's
	// workspace.Server. Requires MountBackingDir. Leave nil to keep the
	// pre-S3 passthrough where a mount's Source is used as a host path.
	MountMaterializer MountMaterializer
	// MountBackingDir is the absolute parent directory under which
	// per-Sandbox mount backing trees are materialised. Required when
	// MountMaterializer is set; ignored otherwise.
	MountBackingDir string
	// ObserverDir, when non-empty, is the absolute parent directory
	// under which each dispatched VM gets a per-uid host-side observer
	// log. `daft-sandboxd serve` sets it to the per-sandbox state dir
	// ($XDG_STATE_HOME/daft/sandboxes); leave empty in tests/baselines
	// that don't want observer logs written.
	ObserverDir string
}

// EgressProxy configures the in-stack egress proxy (Phase J6). The
// proxy is served inside each VM's netstack on the gateway (see
// sandboxnet); the executor injects http(s)_proxy pointing at that
// fixed gateway:port and ships the proxy CA into the bundle.
type EgressProxy struct {
	// Handler is the proxy's http.Handler (an internal/mitmproxy
	// Proxy). sandboxd builds one and shares it across every VM's
	// netstack, which serves it on gateway:Port. Must be safe for
	// concurrent use. A nil Handler means "no proxy" — egressActive
	// returns false and neither injection nor default-deny kicks in.
	Handler http.Handler
	// CACertPEM is the proxy's CA certificate (PEM). Written into the
	// container rootfs at egressCAGuestPath so guest TLS clients trust
	// the proxy's MITM leaf certs.
	CACertPEM []byte
	// Port is the gateway TCP port the proxy listens on inside the
	// netstack. Zero falls back to sandboxnet.DefaultProxyPort; the
	// injected https_proxy uses whichever applies.
	Port uint16
}

// New builds an Executor. Stdout is required; Runner defaults to
// sandbox.Serve when nil; Assembler defaults to a wrapper around
// sandboxboot.BuildBundleConfig with the deps the caller passes
// via the production helper (see ProductionAssembler below).
//
// When Bridger is set, VsockDir must be a non-empty absolute path —
// the executor cannot mint per-Sandbox hybrid-vsock sockets without
// somewhere to put them.
func New(cfg Config) (*Executor, error) {
	if cfg.Stdout == nil {
		return nil, errors.New("sandboxexec: nil stdout writer")
	}
	if cfg.Runner == nil {
		cfg.Runner = sandbox.Serve
	}
	if cfg.Bridger != nil || cfg.AttachRegistry != nil || cfg.NetEnabled {
		if cfg.VsockDir == "" {
			return nil, errors.New("sandboxexec: VsockDir required when Bridger, AttachRegistry, or NetEnabled is set")
		}
		if !filepath.IsAbs(cfg.VsockDir) {
			return nil, fmt.Errorf("sandboxexec: VsockDir %q must be absolute", cfg.VsockDir)
		}
	}
	if cfg.MountMaterializer != nil {
		if cfg.MountBackingDir == "" {
			return nil, errors.New("sandboxexec: MountBackingDir required when MountMaterializer is set")
		}
		if !filepath.IsAbs(cfg.MountBackingDir) {
			return nil, fmt.Errorf("sandboxexec: MountBackingDir %q must be absolute", cfg.MountBackingDir)
		}
	}
	if cfg.ObserverDir != "" && !filepath.IsAbs(cfg.ObserverDir) {
		return nil, fmt.Errorf("sandboxexec: ObserverDir %q must be absolute", cfg.ObserverDir)
	}
	e := &Executor{
		base:              cfg.Base,
		stdout:            cfg.Stdout,
		run:               cfg.Runner,
		assembler:         cfg.Assembler,
		bridger:           cfg.Bridger,
		attachRegistry:    cfg.AttachRegistry,
		observerRegistry:  cfg.ObserverRegistry,
		summaryRegistry:   cfg.SummaryRegistry,
		observeSummary:    cfg.ObserveSummary,
		vsockDir:          cfg.VsockDir,
		netEnabled:        cfg.NetEnabled,
		egressProxy:       cfg.EgressProxy,
		mountMaterializer: cfg.MountMaterializer,
		mountBackingDir:   cfg.MountBackingDir,
		observerDir:       cfg.ObserverDir,
		live:              map[string]*liveSandbox{},
	}
	// First .Add(1) returns 3 — the lowest CID the AF_VSOCK ABI
	// allows for a guest.
	e.nextCID.Store(2)
	return e, nil
}

// LiveSnapshot returns the wire-shaped set of sandboxes the Executor
// currently knows about. Wired into nodectrlclient.Config so each
// Heartbeat carries the level-triggered reconciliation signal that
// drives Workspace.status.observed_generation upstream (Slice B4d).
//
// Snapshot semantics: a sandbox is included until it transitions to
// a terminal phase AND the orchestrator has had a heartbeat to
// observe that terminal state. Terminal entries are dropped from
// the live map on the next snapshot to keep the wire payload
// bounded; the orchestrator's reconciler treats absence as "exited"
// going forward.
func (e *Executor) LiveSnapshot() []*nodectrlv1.LiveSandbox {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*nodectrlv1.LiveSandbox, 0, len(e.live))
	var drop []string
	for uid, ls := range e.live {
		out = append(out, &nodectrlv1.LiveSandbox{
			Uid:      ls.uid,
			Phase:    ls.phase,
			ExitCode: ls.exitCode,
		})
		if ls.phase == phaseExited || ls.phase == phaseKilled {
			drop = append(drop, uid)
		}
	}
	for _, uid := range drop {
		delete(e.live, uid)
	}
	// Sort by uid so heartbeats are byte-stable across replays —
	// makes test golden-file comparisons trivial.
	slices.SortFunc(out, func(a, b *nodectrlv1.LiveSandbox) int {
		switch {
		case a.GetUid() < b.GetUid():
			return -1
		case a.GetUid() > b.GetUid():
			return 1
		}
		return 0
	})
	return out
}

// markPhase records a phase transition for uid. Used by
// HandleSandboxEvent's lifecycle path to populate the snapshot.
func (e *Executor) markPhase(uid, phase string, exitCode int32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ls, ok := e.live[uid]
	if !ok {
		ls = &liveSandbox{uid: uid}
		e.live[uid] = ls
	}
	ls.phase = phase
	ls.exitCode = exitCode
}

// ProductionAssembler returns an Assembler backed by
// sandboxboot.BuildBundleConfig. Threading the deps through here
// keeps the wiring in one place — `daft-sandboxd serve`
// constructs one and hands it to New.
func ProductionAssembler(deps sandboxboot.Deps) Assembler {
	return func(ctx context.Context, ref string, opts sandboxboot.BootOptions) (sandbox.Config, func(), error) {
		return sandboxboot.BuildBundleConfig(ctx, ref, opts, deps)
	}
}

// HandleSandboxEvent is the SandboxEventHandler entrypoint. ADDED
// events trigger a VM boot for the embedded Sandbox.spec; other
// types are ignored for now (MODIFIED is unused in v1 since Sandbox
// specs are immutable; DELETED teardown wiring lands in B5).
//
// When spec.image_ref is non-empty, the executor delegates to its
// Assembler to pull the image and build the bootable sandbox.Config,
// then runs the VM with that config. When image_ref is empty (or no
// Assembler is configured), the executor falls back to the bare
// baseline Hello — the smoke-test path.
//
// Spec fields that don't yet have an executor consumer (mounts[],
// env, ports, gpu_request, isolation) are recorded as span
// attributes but otherwise ignored. Wiring them lands in later
// slices alongside Workspace + ingress.
//
// v1 invokes the runner synchronously, which blocks the
// nodectrlclient receive loop for the duration of the VM boot
// (~hundreds of ms warm, multiple seconds cold). Becomes
// non-blocking once a real concurrency cap and the heartbeat-based
// reconciler (B4d/B5) land.
func (e *Executor) HandleSandboxEvent(
	ctx context.Context,
	evt *nodectrlv1.SandboxWatchEvent,
) error {
	assert.True(evt != nil,
		"SandboxEventHandler must never be called with a nil event — nodectrlclient filters them upstream")
	if evt.GetType() != sandboxv1.EventType_EVENT_TYPE_ADDED {
		// MODIFIED/DELETED handling lands with the reconciler.
		return nil
	}
	sb := evt.GetSandbox()
	assert.True(sb != nil,
		"SandboxWatchEvent ADDED without a Sandbox payload is an orchestrator bug — the wire schema makes the field optional, but every ADDED code path sets it")
	uid := sb.GetMetadata().GetUid()
	spec := sb.GetSpec()
	assert.True(spec != nil,
		"Sandbox without a Spec is an orchestrator bug — Launch always composes one before publishing")
	ctx, span := trace.Start(ctx, "sandboxexec.sandbox_added",
		trace.WithAttributes(
			trace.KV{Key: "sandbox.uid", Value: uid},
			trace.KV{Key: "spec.image_ref", Value: spec.GetImageRef()},
			trace.KV{Key: "spec.workspace_uuid", Value: spec.GetWorkspaceUuid()},
		))
	defer span.End()

	_, _ = fmt.Fprintf(e.stdout,
		"daft-sandboxd: sandbox %s spec.workspace=%s image=%s mounts=%d\n",
		uid, spec.GetWorkspaceUuid(), spec.GetImageRef(), len(spec.GetMounts()))

	e.markPhase(uid, phaseRunning, 0)

	cfg, unlock, err := e.resolveBootConfig(ctx, uid, spec)
	if err != nil {
		e.markPhase(uid, phaseKilled, 0)
		return trace.RecordError(ctx, fmt.Errorf("sandboxexec: sandbox %s: %w", uid, err))
	}
	defer unlock()
	defer e.teardownMounts(uid, spec)

	unregisterPorts := e.registerPortRoutes(cfg, spec.GetWorkspaceUuid())
	defer unregisterPorts()
	unregisterAttach := e.registerAttachRoute(cfg, uid)
	defer unregisterAttach()
	unregisterObserver := e.registerObserverRoute(cfg, uid)
	defer unregisterObserver()
	unregisterSummary := e.registerSummaryRoute(cfg, uid)
	defer unregisterSummary()

	if err := e.run(ctx, e.stdout, cfg); err != nil {
		e.markPhase(uid, phaseKilled, 0)
		return trace.RecordError(ctx, fmt.Errorf("sandboxexec: sandbox %s: %w", uid, err))
	}
	e.markPhase(uid, phaseExited, 0)
	return nil
}

// registerPortRoutes adds one Bridger route per PortForward on cfg,
// each backed by a DialGuestVsock closure that targets this VM's
// HostSocket + vsock port. Returns an unregister func — the caller
// defers it so the routes vanish on dispatch exit (whether the VM
// booted, errored, or never started).
//
// No-op when no Bridger is configured or when the dispatch carries
// no workspace_uuid (the orchestrator's smoke-test path).
func (e *Executor) registerPortRoutes(cfg sandbox.Config, workspaceUUID string) func() {
	if e.bridger == nil || workspaceUUID == "" || cfg.Vsock == nil {
		return func() {}
	}
	hostSocket := cfg.Vsock.HostSocket
	registered := make([]uint32, 0, len(cfg.PortForwards))
	for _, pf := range cfg.PortForwards {
		vsockPort := pf.VsockPort
		dial := func(ctx context.Context) (net.Conn, error) {
			return sandbox.DialGuestVsock(ctx, hostSocket, vsockPort)
		}
		if err := e.bridger.Register(workspaceUUID, pf.ContainerPort, dial); err != nil {
			// Register failures are executor-bug-class
			// (duplicate (ws,port) tuple). Surface them on
			// stdout so the operator sees them, then unregister
			// whatever did succeed so the table isn't left with
			// stragglers.
			_, _ = fmt.Fprintf(e.stdout, "sandboxexec: register port route: %v\n", err)
			for _, cp := range registered {
				e.bridger.Unregister(workspaceUUID, cp)
			}
			return func() {}
		}
		registered = append(registered, pf.ContainerPort)
	}
	return func() {
		for _, cp := range registered {
			e.bridger.Unregister(workspaceUUID, cp)
		}
	}
}

// registerAttachRoute adds one AttachRegistry entry for this
// dispatch's sandbox_uid, backed by a closure that dials the
// agent's attach vsock through the per-Sandbox hybrid-vsock UDS
// and runs sandboxattach.OpenSession to wrap the framed conn as
// an AttachSession. Returns an unregister func — the caller
// defers it so the route vanishes on dispatch exit.
//
// No-op when no AttachRegistry is configured or when cfg.Vsock /
// the attach feature is unset (resolveBootConfig only enables it when
// AttachRegistry != nil; the guard mirrors registerPortRoutes'
// defensive style). The dial targets the fixed DefaultAttachVsockPort.
func (e *Executor) registerAttachRoute(cfg sandbox.Config, uid string) func() {
	if e.attachRegistry == nil || cfg.Vsock == nil || !cfg.Vsock.Attach || uid == "" {
		return func() {}
	}
	hostSocket := cfg.Vsock.HostSocket
	attachPort := agent.DefaultAttachVsockPort
	dial := func(ctx context.Context, req sandboxgateway.AttachRequest) (sandboxgateway.AttachSession, error) {
		conn, err := sandbox.DialGuestVsock(ctx, hostSocket, attachPort)
		if err != nil {
			return nil, fmt.Errorf("sandboxexec: dial attach vsock: %w", err)
		}
		return sandboxattach.OpenSession(ctx, conn, req)
	}
	if err := e.attachRegistry.Register(uid, dial); err != nil {
		// Same executor-bug class as duplicate port routes —
		// surface it on stdout and leave the table clean.
		_, _ = fmt.Fprintf(e.stdout, "sandboxexec: register attach route: %v\n", err)
		return func() {}
	}
	return func() { e.attachRegistry.Unregister(uid) }
}

// registerObserverRoute adds one ObserverRegistry entry mapping this
// dispatch's sandbox_uid to its host-side observer-log path so the
// orchestrator-driven ObserverStream handler can tail it. Returns an
// unregister func the caller defers so the route vanishes on dispatch
// exit. No-op when no ObserverRegistry is configured or the dispatch
// has no observer log (ObserverLogPath unset).
func (e *Executor) registerObserverRoute(cfg sandbox.Config, uid string) func() {
	if e.observerRegistry == nil || cfg.ObserverLogPath == "" || uid == "" {
		return func() {}
	}
	if err := e.observerRegistry.Register(uid, cfg.ObserverLogPath); err != nil {
		// Same executor-bug class as duplicate attach routes —
		// surface it on stdout and leave the table clean.
		_, _ = fmt.Fprintf(e.stdout, "sandboxexec: register observer route: %v\n", err)
		return func() {}
	}
	return func() { e.observerRegistry.Unregister(uid) }
}

// resolveBootConfig picks between the baseline Hello config and
// the per-dispatch bundled config, then layers in the C1 port-
// forward plan derived from spec.ports plus the per-Sandbox Vsock
// device the C1.3c ingress path will dial through. unlock is
// always non-nil so callers can defer it unconditionally.
func (e *Executor) resolveBootConfig(ctx context.Context, uid string, spec *sandboxv1.SandboxSpec) (sandbox.Config, func(), error) {
	cfg, unlock, err := e.baseBootConfig(ctx, uid, spec)
	if err != nil {
		return sandbox.Config{}, func() {}, err
	}
	applyResourceSizing(&cfg, spec)
	cfg.PortForwards = portForwardsFromSpec(spec.GetPorts())
	// Vsock is always-on: every dispatch gets a hybrid-vsock device so the
	// in-VM agent fetches its config (and streams status events) over the
	// control channel instead of the world-readable kernel cmdline. The
	// only gate is a configured VsockDir — production sandboxd always sets
	// it (the Bridger/AttachRegistry/NetEnabled paths require it in New),
	// so this is unconditional there; a bare unit-test executor with no
	// VsockDir uses a fake runner that never boots a real VM, so it needs
	// no device. The per-feature ports stay gated on their feature flags.
	if e.vsockDir != "" {
		cfg.Vsock = e.assignVsock(uid)
		if e.attachRegistry != nil {
			cfg.Vsock.Attach = true
		}
		if e.netEnabled {
			cfg.Vsock.Net = true
			// With an egress proxy configured, serve it in-stack on the
			// gateway and lock the netstack to default-deny: every
			// legitimate egress is injected to route through the proxy
			// (J6), so anything dialing elsewhere is a leak we want dropped
			// + logged rather than silently sent.
			if e.egressActive() {
				cfg.DenyEgress = true
				cfg.ProxyHandler = e.egressProxy.Handler
				cfg.ProxyPort = e.proxyPort()
			}
		}
	}
	e.applyObserverLog(&cfg, uid)
	e.applyObserveSummary(&cfg, uid)
	return cfg, unlock, nil
}

// applyObserverLog defaults cfg.ObserverLogPath to a per-uid file
// under the executor's observer dir, mkdir'ing the leaf so the
// observer can open it. It is best-effort: an unset observerDir, an
// already-set path (baseline override wins), an empty uid, or a mkdir
// failure all leave the config untouched and the dispatch proceeds —
// observability is a debugging surface, never a launch precondition.
func (e *Executor) applyObserverLog(cfg *sandbox.Config, uid string) {
	if e.observerDir == "" || cfg.ObserverLogPath != "" || uid == "" {
		return
	}
	path, err := observability.DefaultPath(e.observerDir, uid)
	if err != nil {
		_, _ = fmt.Fprintf(e.stdout, "sandboxexec: observer log disabled for %s: %v\n", uid, err)
		return
	}
	cfg.ObserverLogPath = path
}

// Observe share layout. The Tier-2 gadgets (open-snoop) and their
// build-progress summary are guest-written, so the executor exports a
// small writable virtiofs share — host-backed by <observerDir>/<uid>/observe
// — and points the guest paths into its mountpoint. The host reads the
// summary back from the same backing dir; it is kept separate from the
// host-written observer.jsonl so the guest can only touch its own files.
const (
	observeShareTag       = "daftobserve"
	observeMount          = "/run/daft/observe"
	observeSubdir         = "observe"
	observeOpensnoopName  = "opensnoop.log"
	observeExecsnoopName  = "execsnoop.log"
	observeSleepsnoopName = "sleepsnoop.log"
	observeIosnoopName    = "iosnoop.log"
	observeSyscallsName   = "syscalls.txt"
	observeSummaryName    = "progress.json"
)

// observeHostDir is the host backing dir of a dispatch's observe share.
func (e *Executor) observeHostDir(uid string) string {
	return filepath.Join(e.observerDir, uid, observeSubdir)
}

// applyObserveSummary wires the Tier-2 observe share: it mkdir's the host
// backing dir, exports it as a writable virtiofs share, and points the
// guest's open-snoop + exec-snoop + sleep-snoop + io-snoop traces and the
// build-progress summary into the mountpoint so the gadgets run and the output lands
// somewhere the host can read. Gated
// on the same observerDir opt-in as the observer log and equally
// best-effort: an unset observerDir, already-set paths, an empty uid, or a
// mkdir failure all leave the config untouched and the dispatch proceeds.
func (e *Executor) applyObserveSummary(cfg *sandbox.Config, uid string) {
	if !e.observeSummary || e.observerDir == "" || cfg.ObserveSummaryPath != "" || uid == "" {
		return
	}
	hostDir := e.observeHostDir(uid)
	if err := os.MkdirAll(hostDir, 0o700); err != nil {
		_, _ = fmt.Fprintf(e.stdout, "sandboxexec: observe summary disabled for %s: %v\n", uid, err)
		return
	}
	// Clone before appending: cfg may alias e.base (the no-image baseline
	// path returns a copy whose Shares slice shares e.base's backing array),
	// and appending in place could corrupt the baseline across dispatches.
	observe := &sandbox.ShareConfig{HostDir: hostDir, Tag: observeShareTag, Mountpoint: observeMount}
	cfg.Shares = append(append([]*sandbox.ShareConfig(nil), cfg.Shares...), observe)
	cfg.OpensnoopLogPath = filepath.Join(observeMount, observeOpensnoopName)
	cfg.ExecsnoopLogPath = filepath.Join(observeMount, observeExecsnoopName)
	cfg.SleepsnoopLogPath = filepath.Join(observeMount, observeSleepsnoopName)
	cfg.IosnoopLogPath = filepath.Join(observeMount, observeIosnoopName)
	cfg.SyscallListPath = filepath.Join(observeMount, observeSyscallsName)
	cfg.ObserveSummaryPath = filepath.Join(observeMount, observeSummaryName)
}

// registerSummaryRoute adds one SummaryRegistry entry mapping this
// dispatch's uid to the host path of its build-progress summary so the
// orchestrator-driven ObserveSummary handler can read it. Returns an
// unregister func the caller defers. No-op when no SummaryRegistry is
// configured or the observe share wasn't set up (ObserveSummaryPath unset).
func (e *Executor) registerSummaryRoute(cfg sandbox.Config, uid string) func() {
	if e.summaryRegistry == nil || cfg.ObserveSummaryPath == "" || uid == "" {
		return func() {}
	}
	hostPath := filepath.Join(e.observeHostDir(uid), observeSummaryName)
	if err := e.summaryRegistry.Register(uid, hostPath); err != nil {
		_, _ = fmt.Fprintf(e.stdout, "sandboxexec: register summary route: %v\n", err)
		return func() {}
	}
	return func() { e.summaryRegistry.Unregister(uid) }
}

// assignVsock mints a per-Sandbox hybrid-vsock device: a fresh
// GuestCID via the executor's atomic counter and an absolute UDS
// path under VsockDir named after the Sandbox uid. Caller
// guarantees Bridger != nil ⇒ VsockDir != "" (checked in New), so
// no further validation is needed here.
func (e *Executor) assignVsock(uid string) *sandbox.VsockConfig {
	cid := e.nextCID.Add(1)
	assert.True(cid >= 3,
		"vsock CID counter wrapped past uint32 — this Executor has dispatched 2^32 Sandboxes, well beyond any realistic operator scenario")
	return &sandbox.VsockConfig{
		HostSocket: filepath.Join(e.vsockDir, uid+".sock"),
		GuestCID:   cid,
	}
}

// baseBootConfig is the image-or-fallback half of resolveBootConfig:
// when the spec carries an image_ref we ask the assembler to bundle
// it; otherwise we fall back to the smoke-test baseline. Split out
// so port-forward layering above stays a one-liner regardless of
// which branch produced the cfg.
func (e *Executor) baseBootConfig(ctx context.Context, uid string, spec *sandboxv1.SandboxSpec) (sandbox.Config, func(), error) {
	imageRef := spec.GetImageRef()
	if imageRef == "" || e.assembler == nil {
		return e.base, func() {}, nil
	}
	mounts, err := e.resolveMounts(ctx, uid, spec)
	if err != nil {
		return sandbox.Config{}, func() {}, err
	}
	opts := sandboxboot.BootOptions{
		Mounts:               mounts,
		Env:                  envFromSpec(spec.GetEnv()),
		Nested:               spec.GetNestedVirtualization().GetEnable(),
		PrivilegedFuseMounts: spec.GetPrivilegedFuseMounts().GetEnable(),
	}
	// Layer the egress-proxy env after the workspace's own env so the
	// proxy vars win on a key clash (a workload can't unset the proxy
	// the node mandates), and ship the proxy CA into the rootfs.
	if e.egressActive() {
		opts.Env = append(opts.Env, proxyEnv(e.proxyGuestURL())...)
		opts.CACert = e.egressProxy.CACertPEM
		opts.CACertGuestPath = egressCAGuestPath
	}
	return e.assembler(ctx, imageRef, opts)
}

// egressActive reports whether a usable egress proxy is configured.
// Both proxy-env injection and the DenyEgress decision key off this
// single predicate: turning on default-deny without serving a
// reachable proxy lane would black-hole all guest egress.
func (e *Executor) egressActive() bool {
	return e.egressProxy != nil && e.egressProxy.Handler != nil
}

// proxyPort is the gateway port the in-stack proxy listens on,
// defaulting to sandboxnet's when the config leaves it zero.
func (e *Executor) proxyPort() uint16 {
	if e.egressProxy.Port != 0 {
		return e.egressProxy.Port
	}
	return sandboxnet.DefaultProxyPort
}

// proxyGuestURL is the fixed address the guest reaches the in-stack
// proxy at: the netstack gateway plus the proxy port (like the DNS
// server's gateway:53). No VIP, no NAT, no port discovery.
func (e *Executor) proxyGuestURL() string {
	return "http://" + net.JoinHostPort(sandboxnet.DefaultGatewayIP, strconv.Itoa(int(e.proxyPort())))
}

// egressCAGuestPath is where the proxy CA lands inside the container;
// the injected NIX_SSL_CERT_FILE / SSL_CERT_FILE point at it.
const egressCAGuestPath = "/etc/daft-egress-ca.pem"

// envFromSpec flattens the spec's EnvVars into OCI "NAME=VALUE"
// entries. Returns nil for an empty spec so the bundle keeps the
// image's env untouched.
func envFromSpec(in []*sandboxv1.EnvVar) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, e := range in {
		out = append(out, e.GetName()+"="+e.GetValue())
	}
	return out
}

// proxyEnv is the set of variables that route a container's HTTP(S)
// egress through the proxy at guestURL and make its TLS clients trust
// the proxy CA. Both lower- and upper-case proxy vars are set because
// tools disagree on which they honour, and the CA path is exported
// under every common per-tool name (nix, curl, git, python-requests,
// node) so a default-deny workload's HTTPS clients validate the MITM
// leaf out of the box. no_proxy keeps loopback traffic off the proxy.
func proxyEnv(guestURL string) []string {
	return []string{
		"http_proxy=" + guestURL,
		"https_proxy=" + guestURL,
		"HTTP_PROXY=" + guestURL,
		"HTTPS_PROXY=" + guestURL,
		"no_proxy=localhost,127.0.0.1",
		"NO_PROXY=localhost,127.0.0.1",
		"NIX_SSL_CERT_FILE=" + egressCAGuestPath,
		"SSL_CERT_FILE=" + egressCAGuestPath,
		"CURL_CA_BUNDLE=" + egressCAGuestPath,
		"GIT_SSL_CAINFO=" + egressCAGuestPath,
		"REQUESTS_CA_BUNDLE=" + egressCAGuestPath,
		"NODE_EXTRA_CA_CERTS=" + egressCAGuestPath,
	}
}

// applyResourceSizing layers the SandboxSpec's guest-RAM and vCPU
// requests onto the boot config. A zero memory request leaves
// cfg.Memory as the assembler produced it (sandbox.Config then
// applies its 256 MiB default). A zero CPU request on an
// image-backed dispatch falls back to half the host's logical
// cores (min 1) — the workspace VM shares cores with the host
// rather than owning them, and a single vCPU starves CPU-heavy
// workloads like `nix develop`'s nixpkgs evaluation. The
// imageRef guard keeps the smoke-test fallback path (which boots
// e.base) on its own CPU setting.
func applyResourceSizing(cfg *sandbox.Config, spec *sandboxv1.SandboxSpec) {
	if b := spec.GetMemoryBytes(); b > 0 {
		cfg.Memory = memoryArgFromBytes(b)
	}
	switch {
	case spec.GetCpuCores() > 0:
		cfg.CPUs = int(spec.GetCpuCores())
	case spec.GetImageRef() != "" && cfg.CPUs == 0:
		cfg.CPUs = defaultWorkspaceCPUs()
	}
}

// defaultWorkspaceCPUs returns half the host's logical cores,
// floored at 1.
func defaultWorkspaceCPUs() int {
	if n := runtime.NumCPU() / 2; n > 0 {
		return n
	}
	return 1
}

// memoryArgFromBytes renders a byte count as the MiB-suffixed
// string sandbox.Config.Memory expects (it becomes cloud-
// hypervisor's `--memory size=`). Rounds up so a sub-MiB request
// never collapses to size=0.
func memoryArgFromBytes(b uint64) string {
	const mib = 1 << 20
	return fmt.Sprintf("%dM", (b+mib-1)/mib)
}

// resolveMounts turns the spec's mounts into sandboxboot.Mount
// entries whose Source is an absolute host path virtiofsd can export.
//
// Source determines how each mount resolves:
//   - An absolute path is a direct host bind (the advanced `mount add
//     --source /abs` form, and the pre-S3 behaviour): it's exported
//     as-is, no CAS involved.
//   - Anything else is a logical identifier — a workspace UUID for the
//     dir-local push/pull mounts — so the executor materialises the
//     tree pushed under SyncKey(workspace_uuid, target) from the node's
//     persistent CAS into a per-Sandbox backing dir (Slice S3) and
//     points the share at that dir. An unpushed mount materialises as
//     an empty read-write tree.
//
// Without a materializer wired (unit tests, smoke path) every mount
// falls back to the passthrough so those callers don't need a CAS.
func (e *Executor) resolveMounts(ctx context.Context, uid string, spec *sandboxv1.SandboxSpec) ([]sandboxboot.Mount, error) {
	in := spec.GetMounts()
	out := make([]sandboxboot.Mount, 0, len(in))
	for i, m := range in {
		if e.mountMaterializer == nil || filepath.IsAbs(m.GetSource()) {
			out = append(out, sandboxboot.Mount{
				Target:   m.GetTarget(),
				Source:   m.GetSource(),
				ReadOnly: m.GetReadOnly(),
			})
			continue
		}
		dest := e.mountDir(uid, i)
		key := workspace.SyncKey(spec.GetWorkspaceUuid(), m.GetTarget())
		applied, err := e.mountMaterializer.MaterializeMount(ctx, key, dest)
		if err != nil {
			return nil, fmt.Errorf("materialize mount %s: %w", m.GetTarget(), err)
		}
		if !applied {
			_, _ = fmt.Fprintf(e.stdout,
				"daft-sandboxd: mount %s has no pushed data; starting empty (run `daft workspace push`)\n",
				m.GetTarget())
		}
		// Register the backing dir as live so a pull against this
		// running VM re-manifests it (Slice S4). Read-only mounts are
		// left out: the VM can't have changed them, so the last-pushed
		// snapshot is already authoritative. Cleared in teardownMounts.
		if !m.GetReadOnly() {
			e.mountMaterializer.SetLiveMount(key, dest)
		}
		out = append(out, sandboxboot.Mount{
			Target:   m.GetTarget(),
			Source:   dest,
			ReadOnly: m.GetReadOnly(),
		})
	}
	return out, nil
}

// mountDir is the absolute per-Sandbox, per-mount backing directory
// virtiofsd exports. Keyed by Sandbox uid (unique per launch) so
// concurrent dispatches never share a tree, and by mount index so the
// dirs line up with the share tags the assembler mints.
func (e *Executor) mountDir(uid string, i int) string {
	return filepath.Join(e.mountBackingDir, uid, fmt.Sprintf("mnt%d", i))
}

// teardownMounts ends the dispatch's mount lifetime: it clears every
// live-mount registration (so a post-stop pull falls back to the
// last-pushed snapshot, Slice S4) and then removes the per-Sandbox
// backing tree. Order matters — unregister before removing, so a
// concurrent pull can't re-manifest a half-deleted dir. Materialised
// mounts are reconstructed fresh from the node CAS on every launch, so
// leaving the tree around only leaks disk.
func (e *Executor) teardownMounts(uid string, spec *sandboxv1.SandboxSpec) {
	if e.mountMaterializer == nil || e.mountBackingDir == "" {
		return
	}
	workspaceUUID := spec.GetWorkspaceUuid()
	for _, m := range spec.GetMounts() {
		if filepath.IsAbs(m.GetSource()) || m.GetReadOnly() {
			continue
		}
		e.mountMaterializer.ClearLiveMount(workspace.SyncKey(workspaceUUID, m.GetTarget()))
	}
	if err := os.RemoveAll(filepath.Join(e.mountBackingDir, uid)); err != nil {
		_, _ = fmt.Fprintf(e.stdout, "sandboxexec: cleanup mount backing dir: %v\n", err)
	}
}

// BaseVsockPort is the first AF_VSOCK port the executor hands out
// for orchestrator-driven ingress. Sits well clear of CH's reserved
// low-numbered ports and the fixed control-channel ports (config 7000,
// trace 8000, attach 9000, net 9500) so a port-forward never collides
// with a control feature.
const BaseVsockPort = 10000

// portForwardsFromSpec assigns a unique guest-side AF_VSOCK port
// (starting at BaseVsockPort, incrementing per accepted entry) to
// each TCP PortMapping in declaration order. UDP entries and
// zero-valued container ports are skipped: the CH hybrid-vsock
// CONNECT proxy is stream-only and zero-port means "any" — which
// the orchestrator (not us) resolves before dispatch.
func portForwardsFromSpec(ports []*sandboxv1.PortMapping) []sandbox.PortForward {
	if len(ports) == 0 {
		return nil
	}
	out := make([]sandbox.PortForward, 0, len(ports))
	next := uint32(BaseVsockPort)
	for _, pm := range ports {
		if pm.GetProtocol() != sandboxv1.Protocol_PROTOCOL_TCP {
			continue
		}
		if pm.GetContainerPort() == 0 {
			continue
		}
		out = append(out, sandbox.PortForward{
			VsockPort:     next,
			ContainerPort: pm.GetContainerPort(),
		})
		next++
	}
	return out
}
