// Package reconciler is the orchestrator-side level-triggered
// controller (PLAN.md Slice B5). It owns the four reconciliation
// points described in ARCHITECTURE.md:
//
//  1. **On heartbeat** (ReconcileLiveSandboxes): each Node's
//     reported live-sandbox set drives Workspace.status
//     transitions. Three consecutive heartbeats without the
//     expected uid promote a workspace's Dispatched condition
//     to False with reason SandboxLost.
//  2. **Periodic sweep** (Run): catches Nodes that have stopped
//     heartbeating entirely. After NodeGrace without a heartbeat,
//     every workspace bound to that Node is cleared
//     (Dispatched=False, reason NodeUnreachable).
//  3. **Pre-action** (ReconcileBefore): called by Workspace
//     Launch/Stop before status is read, so any pending
//     threshold-triggered decisions are applied first.
//  4. **Startup** (Run on a fresh orchestrator): runs the same
//     sweep loop. Once persistence (B6) lands, startup-derived
//     state and live observation can diverge; the sweep is the
//     glue that reconciles them.
//
// The Reconciler is the single writer for status-only mutations
// the catalog can derive from observation (Dispatched, Reconciled,
// CurrentSandbox); spec mutations stay in the WorkspaceCatalogHandler.
package reconciler

import (
	"context"
	"sync"
	"time"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

// PascalCase phase tokens the reconciler reads from
// HeartbeatRequest.live_sandboxes. Match the strings the sandboxd
// executor writes in internal/sandboxexec. See proto/sandbox/v1/
// sandbox.proto for the wire-side documentation.
const (
	phaseRunning = "Running"
	phaseExited  = "Exited"
	phaseKilled  = "Killed"
)

// Sandboxd-side condition reasons set by the reconciler. The CLI
// renders these verbatim so they need to be PascalCase machine
// tokens K8s-style.
const (
	reasonSandboxLost     = "SandboxLost"
	reasonNodeUnreachable = "NodeUnreachable"
)

// Default tunables. Override via Config for tests / operator
// preferences.
const (
	DefaultMissThreshold = 3
	DefaultSweepInterval = 30 * time.Second
	DefaultNodeGrace     = 30 * time.Second
)

// Config bundles the Reconciler's dependencies + tunables.
// Catalog and Nodes are required; everything else has a default.
type Config struct {
	Catalog *workspacecatalog.Store
	Nodes   *nodectrl.Registry
	// MissThreshold is the number of consecutive heartbeats where
	// a workspace's expected uid must be absent before the
	// workspace's Dispatched condition flips to False with reason
	// SandboxLost. Zero ⇒ DefaultMissThreshold.
	MissThreshold int
	// SweepInterval is how often Run wakes to check for Nodes that
	// have stopped heartbeating. Zero ⇒ DefaultSweepInterval.
	SweepInterval time.Duration
	// NodeGrace is how long after a Node's LastSeenAt the
	// reconciler waits before marking its workspaces
	// NodeUnreachable. Zero ⇒ DefaultNodeGrace.
	NodeGrace time.Duration
	// Now is the clock source. Zero ⇒ time.Now. Test injects a
	// stub so missing-heartbeat thresholds fire deterministically.
	Now func() time.Time
}

// Reconciler is the controller. Construct with New; drive
// per-heartbeat reconcile via ReconcileLiveSandboxes; run the
// periodic sweep in a goroutine via Run.
type Reconciler struct {
	catalog       *workspacecatalog.Store
	nodes         *nodectrl.Registry
	missThreshold int
	sweepInterval time.Duration
	nodeGrace     time.Duration
	now           func() time.Time

	mu     sync.Mutex
	misses map[string]int
}

// New builds a Reconciler. Catalog and Nodes are required.
func New(cfg Config) *Reconciler {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.MissThreshold == 0 {
		cfg.MissThreshold = DefaultMissThreshold
	}
	if cfg.SweepInterval == 0 {
		cfg.SweepInterval = DefaultSweepInterval
	}
	if cfg.NodeGrace == 0 {
		cfg.NodeGrace = DefaultNodeGrace
	}
	return &Reconciler{
		catalog:       cfg.Catalog,
		nodes:         cfg.Nodes,
		missThreshold: cfg.MissThreshold,
		sweepInterval: cfg.SweepInterval,
		nodeGrace:     cfg.NodeGrace,
		now:           cfg.Now,
		misses:        map[string]int{},
	}
}

// ReconcileLiveSandboxes is invoked on every Heartbeat by
// NodeControlHandler. Walks workspaces bound to nodeFP and applies
// observed phase to status conditions. Implements
// orchestrator.LivenessReconciler.
func (r *Reconciler) ReconcileLiveSandboxes(nodeFP identity.Fingerprint, live []*nodectrlv1.LiveSandbox) {
	byUID := make(map[string]*nodectrlv1.LiveSandbox, len(live))
	for _, ls := range live {
		if ls.GetUid() == "" {
			continue
		}
		byUID[ls.GetUid()] = ls
	}
	owned := r.catalog.List(func(w *workspacecatalog.Workspace) bool {
		if w.Status.CurrentSandbox == nil {
			return false
		}
		return w.Status.CurrentSandbox.NodeFingerprint == nodeFP
	})
	now := r.now()
	for i := range owned {
		ws := owned[i]
		uid := ws.Status.CurrentSandbox.UID
		ls, present := byUID[uid]
		if !present {
			r.recordMiss(ws.Metadata.UUID, now)
			continue
		}
		r.clearMiss(ws.Metadata.UUID)
		r.applyPhase(ws, ls, now)
	}
}

// recordMiss increments the consecutive-miss counter for uuid. If
// the threshold is crossed, the workspace's Dispatched condition
// is flipped to False with reason SandboxLost and the counter is
// reset.
func (r *Reconciler) recordMiss(uuid string, now time.Time) {
	r.mu.Lock()
	r.misses[uuid]++
	count := r.misses[uuid]
	if count < r.missThreshold {
		r.mu.Unlock()
		return
	}
	delete(r.misses, uuid)
	r.mu.Unlock()
	r.markLost(uuid, reasonSandboxLost, now)
}

// clearMiss resets the counter for uuid. Called when the Node
// reports the expected uid (whatever phase).
func (r *Reconciler) clearMiss(uuid string) {
	r.mu.Lock()
	delete(r.misses, uuid)
	r.mu.Unlock()
}

// applyPhase reflects the Node's reported phase onto the
// workspace's status. Mirrors what was in the prior
// WorkspaceCatalogHandler.ReconcileLiveSandboxes; consolidated here
// so all status-from-observation lives in one package.
func (r *Reconciler) applyPhase(ws workspacecatalog.Workspace, ls *nodectrlv1.LiveSandbox, now time.Time) {
	generation := ws.Metadata.Generation
	_, _ = r.catalog.Update(ws.Metadata.UUID, workspacecatalog.MutateStatus,
		func(w *workspacecatalog.Workspace) error {
			switch ls.GetPhase() {
			case phaseRunning:
				w.Status.ObservedGeneration = generation
				w.Status.SetCondition(workspacecatalog.ConditionReconciled,
					workspacecatalog.ConditionStatusTrue, phaseRunning, "", now)
			case phaseExited, phaseKilled:
				w.Status.LastTerminalSandbox = terminalObservation(
					w.Status.CurrentSandbox, ls.GetPhase(), ls.GetExitCode(), ls.GetMessage(), now,
				)
				w.Status.CurrentSandbox = nil
				w.Status.SetCondition(workspacecatalog.ConditionDispatched,
					workspacecatalog.ConditionStatusFalse, ls.GetPhase(), "", now)
			}
			return nil
		})
}

// markLost flips Dispatched=False with the given reason and clears
// CurrentSandbox. Used by both the per-heartbeat threshold path
// (SandboxLost) and the periodic sweep (NodeUnreachable).
func (r *Reconciler) markLost(uuid, reason string, now time.Time) {
	_, _ = r.catalog.Update(uuid, workspacecatalog.MutateStatus,
		func(w *workspacecatalog.Workspace) error {
			w.Status.LastTerminalSandbox = terminalObservation(
				w.Status.CurrentSandbox, "Unknown", 0, reason, now,
			)
			w.Status.CurrentSandbox = nil
			w.Status.SetCondition(workspacecatalog.ConditionDispatched,
				workspacecatalog.ConditionStatusFalse, reason, "", now)
			return nil
		})
}

func terminalObservation(
	current *workspacecatalog.SandboxRef,
	phase string,
	exitCode int32,
	message string,
	now time.Time,
) *workspacecatalog.TerminalSandbox {
	if current == nil {
		return nil
	}
	runes := []rune(message)
	if len(runes) > 512 {
		runes = runes[:512]
	}
	return &workspacecatalog.TerminalSandbox{
		UID: current.UID, NodeFingerprint: current.NodeFingerprint,
		WorkspaceGeneration: current.WorkspaceGeneration,
		Phase:               phase, ExitCode: exitCode, Message: string(runes), ObservedAt: now,
	}
}

// ReconcileBefore is called by Launch/Stop before they read
// workspace status. In B5 it's a thin guard that ensures any
// already-crossed miss threshold has been applied before the
// caller proceeds; the per-heartbeat path normally does this
// inline, but a fast-firing client could race the heartbeat
// handler. For workspaces with no pending decisions this is O(1).
func (r *Reconciler) ReconcileBefore(uuid string) {
	r.mu.Lock()
	count := r.misses[uuid]
	if count < r.missThreshold {
		r.mu.Unlock()
		return
	}
	delete(r.misses, uuid)
	r.mu.Unlock()
	r.markLost(uuid, reasonSandboxLost, r.now())
}

// Run drives the periodic sweep until ctx is cancelled. Wakes
// every SweepInterval and:
//   - walks the Node registry for entries where
//     LastSeenAt + NodeGrace < now;
//   - for each stale Node, walks the catalog for workspaces
//     bound to it and flips Dispatched=False with reason
//     NodeUnreachable.
//
// Returns nil when ctx is cancelled; never panics.
func (r *Reconciler) Run(ctx context.Context) error {
	t := time.NewTicker(r.sweepInterval)
	defer t.Stop()
	//daft:lint:disable=forloopbound reason="exits on ctx.Done; sweep cadence is bounded by SweepInterval"
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			r.sweepOnce()
		}
	}
}

// sweepOnce runs one pass of the periodic sweep. Exported via Run
// for production; tests invoke it directly to avoid the ticker.
func (r *Reconciler) sweepOnce() {
	now := r.now()
	dead := r.staleNodes(now)
	if len(dead) == 0 {
		return
	}
	deadSet := make(map[identity.Fingerprint]struct{}, len(dead))
	for _, fp := range dead {
		deadSet[fp] = struct{}{}
	}
	affected := r.catalog.List(func(w *workspacecatalog.Workspace) bool {
		if w.Status.CurrentSandbox == nil {
			return false
		}
		_, ok := deadSet[w.Status.CurrentSandbox.NodeFingerprint]
		return ok
	})
	for i := range affected {
		r.markLost(affected[i].Metadata.UUID, reasonNodeUnreachable, now)
	}
}

// staleNodes returns the fingerprints of Nodes whose LastSeenAt is
// older than now-NodeGrace. Snapshot taken under the registry's
// own lock; the slice is private to the caller.
func (r *Reconciler) staleNodes(now time.Time) []identity.Fingerprint {
	entries := r.nodes.Snapshot()
	cutoff := now.Add(-r.nodeGrace)
	var out []identity.Fingerprint
	for i := range entries {
		if entries[i].LastSeenAt.Before(cutoff) {
			out = append(out, entries[i].Fingerprint)
		}
	}
	return out
}
