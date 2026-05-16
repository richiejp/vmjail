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
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/richiejp/darkaifactory/internal/assert"
	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"
	"github.com/richiejp/darkaifactory/internal/sandbox"
	"github.com/richiejp/darkaifactory/internal/sandboxboot"
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
	"github.com/richiejp/darkaifactory/internal/trace"
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
// image reference into the bootable sandbox.Config + a deferred
// unlock for the bundle lock. Production wires
// sandboxboot.BuildBundleConfig; tests pass fakes that don't
// touch the imagestore.
type Assembler func(ctx context.Context, ref string) (sandbox.Config, func(), error)

// Runner is the function the Executor calls to boot a VM. The
// default in production is sandbox.Hello; tests inject a fake so
// they don't need /dev/kvm.
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
// the VM — production wiring uses sandbox.Hello.
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
	// VsockDir is the absolute parent directory where per-Sandbox
	// hybrid-vsock UDS files live (one per running VM, named after
	// the Sandbox uid). Required when Bridger is non-nil; ignored
	// otherwise. Typically `/var/run/daft-sandboxd` in production.
	VsockDir string
}

// New builds an Executor. Stdout is required; Runner defaults to
// sandbox.Hello when nil; Assembler defaults to a wrapper around
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
		cfg.Runner = sandbox.Hello
	}
	if cfg.Bridger != nil {
		if cfg.VsockDir == "" {
			return nil, errors.New("sandboxexec: VsockDir required when Bridger is set")
		}
		if !filepath.IsAbs(cfg.VsockDir) {
			return nil, fmt.Errorf("sandboxexec: VsockDir %q must be absolute", cfg.VsockDir)
		}
	}
	e := &Executor{
		base:      cfg.Base,
		stdout:    cfg.Stdout,
		run:       cfg.Runner,
		assembler: cfg.Assembler,
		bridger:   cfg.Bridger,
		vsockDir:  cfg.VsockDir,
		live:      map[string]*liveSandbox{},
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
	return func(ctx context.Context, ref string) (sandbox.Config, func(), error) {
		return sandboxboot.BuildBundleConfig(ctx, ref, deps)
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
		"daft-sandboxd: sandbox %s spec.workspace=%s image=%s\n",
		uid, spec.GetWorkspaceUuid(), spec.GetImageRef())

	e.markPhase(uid, phaseRunning, 0)

	cfg, unlock, err := e.resolveBootConfig(ctx, uid, spec)
	if err != nil {
		e.markPhase(uid, phaseKilled, 0)
		return trace.RecordError(ctx, fmt.Errorf("sandboxexec: sandbox %s: %w", uid, err))
	}
	defer unlock()

	unregister := e.registerPortRoutes(cfg, spec.GetWorkspaceUuid())
	defer unregister()

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

// resolveBootConfig picks between the baseline Hello config and
// the per-dispatch bundled config, then layers in the C1 port-
// forward plan derived from spec.ports plus the per-Sandbox Vsock
// device the C1.3c ingress path will dial through. unlock is
// always non-nil so callers can defer it unconditionally.
func (e *Executor) resolveBootConfig(ctx context.Context, uid string, spec *sandboxv1.SandboxSpec) (sandbox.Config, func(), error) {
	cfg, unlock, err := e.baseBootConfig(ctx, spec.GetImageRef())
	if err != nil {
		return sandbox.Config{}, func() {}, err
	}
	cfg.PortForwards = portForwardsFromSpec(spec.GetPorts())
	if e.bridger != nil && len(cfg.PortForwards) > 0 {
		cfg.Vsock = e.assignVsock(uid)
	}
	return cfg, unlock, nil
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
func (e *Executor) baseBootConfig(ctx context.Context, imageRef string) (sandbox.Config, func(), error) {
	if imageRef == "" || e.assembler == nil {
		return e.base, func() {}, nil
	}
	return e.assembler(ctx, imageRef)
}

// BaseVsockPort is the first AF_VSOCK port the executor hands out
// for orchestrator-driven ingress. Sits well clear of CH's reserved
// low-numbered ports and the TracePort=8000 convention so a typo
// in one half doesn't collide with the other.
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
