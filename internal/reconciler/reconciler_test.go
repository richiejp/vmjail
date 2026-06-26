package reconciler

import (
	"context"
	"testing"
	"time"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

const (
	testWorkspaceUUID = "11111111-1111-4111-8111-111111111111"
	testSandboxUID    = "ffffffff-aaaa-4bbb-8ccc-dddddddddddd"
)

// fixedClock returns a monotone clock that advances by one second
// per call. Used so condition LastTransitionTime assertions stay
// deterministic.
func fixedClock(t *testing.T) func() time.Time {
	t.Helper()
	start := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	var n int64
	return func() time.Time {
		out := start.Add(time.Duration(n) * time.Second)
		n++
		return out
	}
}

// readyDispatchedWorkspace plants a workspace with a Sandbox bound
// to nodeFP — the precondition every reconcile-test path needs.
// Touches the store directly because the catalog's wire-side
// Launch/Stop don't live in this package's import graph.
func readyDispatchedWorkspace(t *testing.T, cat *workspacecatalog.Store, nodeFP identity.Fingerprint, now time.Time) workspacecatalog.Workspace {
	t.Helper()
	ws, err := cat.Create(workspacecatalog.CreateInput{
		UUID: testWorkspaceUUID,
		Name: "ws",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	updated, err := cat.Update(ws.Metadata.UUID, workspacecatalog.MutateStatus, func(w *workspacecatalog.Workspace) error {
		w.Status.CurrentSandbox = &workspacecatalog.SandboxRef{
			UID:             testSandboxUID,
			NodeFingerprint: nodeFP,
		}
		w.Status.SetCondition(workspacecatalog.ConditionDispatched,
			workspacecatalog.ConditionStatusTrue, "Launched", "", now)
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	return updated
}

func mustFingerprint(t *testing.T) identity.Fingerprint {
	t.Helper()
	id, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return id.Fingerprint()
}

// TestReconcileRunningAdvancesObservedGeneration pins the
// "expected uid reported as Running" branch.
func TestReconcileRunningAdvancesObservedGeneration(t *testing.T) {
	clock := fixedClock(t)
	cat := workspacecatalog.NewStore(clock)
	nodes := nodectrl.NewRegistry(clock)
	nodeFP := mustFingerprint(t)
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	readyDispatchedWorkspace(t, cat, nodeFP, now)
	r := New(Config{Catalog: cat, Nodes: nodes, Now: clock})

	r.ReconcileLiveSandboxes(nodeFP, []*nodectrlv1.LiveSandbox{
		{Uid: testSandboxUID, Phase: phaseRunning},
	})

	got, _ := cat.Get(testWorkspaceUUID)
	if got.Status.ObservedGeneration != got.Metadata.Generation {
		t.Errorf("ObservedGeneration = %d, want %d",
			got.Status.ObservedGeneration, got.Metadata.Generation)
	}
	if !got.Status.IsTrue(workspacecatalog.ConditionReconciled) {
		t.Errorf("Reconciled not True; conditions=%+v", got.Status.Conditions)
	}
}

// TestReconcileExitedClearsCurrentSandbox pins the terminal-phase
// branch.
func TestReconcileExitedClearsCurrentSandbox(t *testing.T) {
	clock := fixedClock(t)
	cat := workspacecatalog.NewStore(clock)
	nodes := nodectrl.NewRegistry(clock)
	nodeFP := mustFingerprint(t)
	readyDispatchedWorkspace(t, cat, nodeFP, time.Now())
	r := New(Config{Catalog: cat, Nodes: nodes, Now: clock})

	r.ReconcileLiveSandboxes(nodeFP, []*nodectrlv1.LiveSandbox{
		{Uid: testSandboxUID, Phase: phaseExited},
	})

	got, _ := cat.Get(testWorkspaceUUID)
	if got.Status.CurrentSandbox != nil {
		t.Errorf("CurrentSandbox = %+v, want nil after Exited", got.Status.CurrentSandbox)
	}
	if got.Status.IsTrue(workspacecatalog.ConditionDispatched) {
		t.Errorf("Dispatched should be False after Exited; conditions=%+v", got.Status.Conditions)
	}
}

// TestReconcileMissingTripsThreshold pins the missing-heartbeat
// counter: MissThreshold consecutive heartbeats without the
// expected uid promote the workspace to Dispatched=False with
// reason SandboxLost. Earlier misses leave Dispatched alone.
func TestReconcileMissingTripsThreshold(t *testing.T) {
	clock := fixedClock(t)
	cat := workspacecatalog.NewStore(clock)
	nodes := nodectrl.NewRegistry(clock)
	nodeFP := mustFingerprint(t)
	readyDispatchedWorkspace(t, cat, nodeFP, time.Now())
	r := New(Config{Catalog: cat, Nodes: nodes, MissThreshold: 3, Now: clock})

	// Two empty heartbeats: still Dispatched.
	for i := 0; i < 2; i++ {
		r.ReconcileLiveSandboxes(nodeFP, nil)
		got, _ := cat.Get(testWorkspaceUUID)
		if !got.Status.IsTrue(workspacecatalog.ConditionDispatched) {
			t.Errorf("after %d miss: Dispatched flipped early; conditions=%+v",
				i+1, got.Status.Conditions)
		}
	}
	// Third miss → threshold crossed.
	r.ReconcileLiveSandboxes(nodeFP, nil)
	got, _ := cat.Get(testWorkspaceUUID)
	if got.Status.IsTrue(workspacecatalog.ConditionDispatched) {
		t.Errorf("Dispatched not cleared on 3rd miss; conditions=%+v", got.Status.Conditions)
	}
	if reason := got.Status.Condition(workspacecatalog.ConditionDispatched).Reason; reason != reasonSandboxLost {
		t.Errorf("Dispatched.reason = %q, want SandboxLost", reason)
	}
	if got.Status.CurrentSandbox != nil {
		t.Errorf("CurrentSandbox = %+v, want nil after SandboxLost", got.Status.CurrentSandbox)
	}
}

// TestReconcileMissCounterResetsOnReport pins that a single live
// observation between misses resets the threshold counter.
func TestReconcileMissCounterResetsOnReport(t *testing.T) {
	clock := fixedClock(t)
	cat := workspacecatalog.NewStore(clock)
	nodes := nodectrl.NewRegistry(clock)
	nodeFP := mustFingerprint(t)
	readyDispatchedWorkspace(t, cat, nodeFP, time.Now())
	r := New(Config{Catalog: cat, Nodes: nodes, MissThreshold: 2, Now: clock})

	r.ReconcileLiveSandboxes(nodeFP, nil)
	r.ReconcileLiveSandboxes(nodeFP, []*nodectrlv1.LiveSandbox{
		{Uid: testSandboxUID, Phase: phaseRunning},
	})
	// One more miss → if the counter reset, Dispatched still True.
	r.ReconcileLiveSandboxes(nodeFP, nil)
	got, _ := cat.Get(testWorkspaceUUID)
	if !got.Status.IsTrue(workspacecatalog.ConditionDispatched) {
		t.Errorf("counter didn't reset on live report; Dispatched flipped early")
	}
}

// TestSweepFlipsUnreachableNodes pins the periodic sweep: a Node
// whose LastSeenAt is older than NodeGrace gets its workspaces
// flipped to Dispatched=False with reason NodeUnreachable.
func TestSweepFlipsUnreachableNodes(t *testing.T) {
	clock := fixedClock(t)
	cat := workspacecatalog.NewStore(clock)
	nodes := nodectrl.NewRegistry(clock)
	nodeFP := mustFingerprint(t)
	// Register the Node "long ago" so it's already stale by the
	// time the reconciler queries it.
	nodes.Register(nodeFP, "", "v0", nodectrl.Resources{})
	readyDispatchedWorkspace(t, cat, nodeFP, time.Now())

	// Now() is fixedClock's monotone advance; nodeGrace=1ms so the
	// Node is already stale on the first tick.
	r := New(Config{Catalog: cat, Nodes: nodes, NodeGrace: time.Microsecond, Now: clock})
	r.sweepOnce()

	got, _ := cat.Get(testWorkspaceUUID)
	if got.Status.IsTrue(workspacecatalog.ConditionDispatched) {
		t.Errorf("Dispatched should be False after sweep; conditions=%+v", got.Status.Conditions)
	}
	if reason := got.Status.Condition(workspacecatalog.ConditionDispatched).Reason; reason != reasonNodeUnreachable {
		t.Errorf("Dispatched.reason = %q, want NodeUnreachable", reason)
	}
	if got.Status.CurrentSandbox != nil {
		t.Errorf("CurrentSandbox = %+v, want nil after NodeUnreachable", got.Status.CurrentSandbox)
	}
}

// TestSweepIgnoresFreshNodes pins the negative half: a Node that
// heartbeated recently is left alone.
func TestSweepIgnoresFreshNodes(t *testing.T) {
	clock := fixedClock(t)
	cat := workspacecatalog.NewStore(clock)
	nodes := nodectrl.NewRegistry(clock)
	nodeFP := mustFingerprint(t)
	nodes.Register(nodeFP, "", "v0", nodectrl.Resources{})
	readyDispatchedWorkspace(t, cat, nodeFP, time.Now())

	r := New(Config{Catalog: cat, Nodes: nodes, NodeGrace: time.Hour, Now: clock})
	r.sweepOnce()

	got, _ := cat.Get(testWorkspaceUUID)
	if !got.Status.IsTrue(workspacecatalog.ConditionDispatched) {
		t.Errorf("Dispatched should still be True for fresh Node; conditions=%+v", got.Status.Conditions)
	}
}

// TestReconcileBeforeAppliesPendingThreshold pins the pre-action
// hook: when the miss counter is already at the threshold,
// ReconcileBefore applies the SandboxLost transition synchronously.
// In the steady-state path recordMiss applies inline (so the
// counter map drains itself); plant a stale count directly to
// exercise the "found a pending decision" branch this hook exists
// to cover.
func TestReconcileBeforeAppliesPendingThreshold(t *testing.T) {
	clock := fixedClock(t)
	cat := workspacecatalog.NewStore(clock)
	nodes := nodectrl.NewRegistry(clock)
	nodeFP := mustFingerprint(t)
	readyDispatchedWorkspace(t, cat, nodeFP, time.Now())
	r := New(Config{Catalog: cat, Nodes: nodes, MissThreshold: 3, Now: clock})

	// Plant a stale miss count past threshold — simulates the
	// future B6 race where the heartbeat handler observed the
	// miss before a Launch reads status.
	r.mu.Lock()
	r.misses[testWorkspaceUUID] = 3
	r.mu.Unlock()

	r.ReconcileBefore(testWorkspaceUUID)
	got, _ := cat.Get(testWorkspaceUUID)
	if got.Status.IsTrue(workspacecatalog.ConditionDispatched) {
		t.Errorf("Dispatched not cleared; conditions=%+v", got.Status.Conditions)
	}
	if reason := got.Status.Condition(workspacecatalog.ConditionDispatched).Reason; reason != reasonSandboxLost {
		t.Errorf("Dispatched.reason = %q, want SandboxLost", reason)
	}
}

// TestReconcileBeforeNoOpWhenUnderThreshold pins the early-return
// branch: a uuid with no pending decision (under threshold) is a
// no-op call.
func TestReconcileBeforeNoOpWhenUnderThreshold(t *testing.T) {
	clock := fixedClock(t)
	cat := workspacecatalog.NewStore(clock)
	nodes := nodectrl.NewRegistry(clock)
	nodeFP := mustFingerprint(t)
	readyDispatchedWorkspace(t, cat, nodeFP, time.Now())
	r := New(Config{Catalog: cat, Nodes: nodes, MissThreshold: 3, Now: clock})

	r.ReconcileBefore(testWorkspaceUUID)
	got, _ := cat.Get(testWorkspaceUUID)
	if !got.Status.IsTrue(workspacecatalog.ConditionDispatched) {
		t.Errorf("ReconcileBefore mutated status under threshold; conditions=%+v", got.Status.Conditions)
	}
}

// TestRunExitsOnContextCancel pins that the sweep loop terminates
// cleanly when its context is cancelled — the Server's Shutdown
// path relies on this.
func TestRunExitsOnContextCancel(t *testing.T) {
	clock := fixedClock(t)
	cat := workspacecatalog.NewStore(clock)
	nodes := nodectrl.NewRegistry(clock)
	r := New(Config{Catalog: cat, Nodes: nodes, SweepInterval: 50 * time.Millisecond, Now: clock})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx)
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil on ctx cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s after ctx cancel")
	}
}

// TestReconcileIgnoresOtherNodes pins the per-Node filter: a
// heartbeat from a different Node fingerprint must not touch the
// workspace, even if the uid happens to collide.
func TestReconcileIgnoresOtherNodes(t *testing.T) {
	clock := fixedClock(t)
	cat := workspacecatalog.NewStore(clock)
	nodes := nodectrl.NewRegistry(clock)
	nodeFP := mustFingerprint(t)
	otherFP := mustFingerprint(t)
	readyDispatchedWorkspace(t, cat, nodeFP, time.Now())
	r := New(Config{Catalog: cat, Nodes: nodes, MissThreshold: 1, Now: clock})

	r.ReconcileLiveSandboxes(otherFP, []*nodectrlv1.LiveSandbox{
		{Uid: testSandboxUID, Phase: phaseExited},
	})

	got, _ := cat.Get(testWorkspaceUUID)
	if got.Status.CurrentSandbox == nil {
		t.Error("CurrentSandbox cleared by a heartbeat from a different Node")
	}
}
