package casindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newWorkerRig(t *testing.T, policy Policy, contents ...string) (*indexRig, *Worker, []ChunkRef) {
	t.Helper()
	rig, refs := newIndexRig(t, contents...)
	worker, err := NewWorker(WorkerConfig{Index: rig.index, Policy: policy, Now: func() time.Time { return rig.now }})
	if err != nil {
		t.Fatal(err)
	}
	return rig, worker, refs
}

func makeRecoverable(t *testing.T, rig *indexRig, ref ChunkRef, disposition Disposition, lastUse time.Time) {
	t.Helper()
	chunk, err := rig.index.Chunk(ref.Digest)
	if err != nil {
		t.Fatal(err)
	}
	chunk.Recoverability = RecoverabilityRecoverable
	chunk.Disposition = disposition
	chunk.LastUse = lastUse
	if err := rig.index.saveChunk(chunk); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerSoftCleanupDryRunAndStatus(t *testing.T) {
	policy := Policy{HardBytes: 20, SoftBytes: 15, TargetBytes: 10, Expiry: time.Hour}
	rig, worker, refs := newWorkerRig(t, policy, "123456", "abcdef", "protected")
	makeRecoverable(t, rig, refs[0], DispositionRejected, rig.now)
	makeRecoverable(t, rig, refs[1], DispositionSuperseded, rig.now)
	status, err := worker.Status()
	if err != nil || status.UsageBytes != 21 || status.ProtectedBytes != 9 || status.RecoverableBytes != 12 || status.ChunkCount != 3 {
		t.Fatalf("status = %#v, %v", status, err)
	}
	plan, err := worker.Prune(t.Context(), PruneSoft, 0, true)
	if err != nil || len(plan.Candidates) != 2 {
		t.Fatalf("dry plan = %#v, %v", plan, err)
	}
	if has, _ := rig.store.Has(refs[0].Digest); !has {
		t.Fatal("dry run deleted a chunk")
	}
	if _, err := worker.Prune(t.Context(), PruneSoft, 0, false); err != nil {
		t.Fatal(err)
	}
	status, err = worker.Status()
	if err != nil || status.UsageBytes != 9 || status.ProtectedBytes != 9 || status.QueuedCount != 0 {
		t.Fatalf("post-prune status = %#v, %v", status, err)
	}
}

func TestWorkerExpiryWithoutPressure(t *testing.T) {
	policy := Policy{HardBytes: 100, SoftBytes: 85, TargetBytes: 75, Expiry: time.Hour}
	rig, worker, refs := newWorkerRig(t, policy, "expired", "fresh")
	makeRecoverable(t, rig, refs[0], DispositionActive, rig.now.Add(-policy.Expiry))
	makeRecoverable(t, rig, refs[1], DispositionRejected, rig.now)
	plan, err := worker.Prune(t.Context(), PruneSoft, 0, false)
	if err != nil || len(plan.Candidates) != 1 || plan.Candidates[0].Digest != refs[0].Digest {
		t.Fatalf("expiry plan = %#v, %v", plan, err)
	}
	if has, _ := rig.store.Has(refs[0].Digest); has {
		t.Fatal("expired chunk retained")
	}
	if has, _ := rig.store.Has(refs[1].Digest); !has {
		t.Fatal("fresh chunk deleted")
	}
}

func TestWorkerHardAdmissionAndProtectedRefusal(t *testing.T) {
	policy := Policy{HardBytes: 10, SoftBytes: 8, TargetBytes: 7, Expiry: time.Hour}
	rig, worker, refs := newWorkerRig(t, policy, "123456", "abcdef")
	makeRecoverable(t, rig, refs[0], DispositionActive, rig.now)
	plan, err := worker.AdmitWrite(t.Context(), 3)
	if err != nil || len(plan.Candidates) != 1 || plan.UsageAfter+3 > policy.HardBytes {
		t.Fatalf("hard admission = %#v, %v", plan, err)
	}
	if _, err := worker.AdmitWrite(t.Context(), 5); !errors.Is(err, ErrInsufficientReclaim) {
		t.Fatalf("protected refusal = %v", err)
	}
}

func TestWorkerRunUsesInjectedTicks(t *testing.T) {
	policy := Policy{HardBytes: 100, SoftBytes: 85, TargetBytes: 75, Expiry: time.Hour}
	rig, _, refs := newWorkerRig(t, policy, "expired")
	makeRecoverable(t, rig, refs[0], DispositionRejected, rig.now.Add(-policy.Expiry))
	ticks := make(chan time.Time, 1)
	worker, err := NewWorker(WorkerConfig{Index: rig.index, Policy: policy, Now: func() time.Time { return rig.now }, Ticks: ticks})
	if err != nil {
		t.Fatal(err)
	}
	ticks <- rig.now
	close(ticks)
	if err := worker.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if has, _ := rig.store.Has(refs[0].Digest); has {
		t.Fatal("tick did not run cleanup")
	}
}

func TestWorkerConstructionAndCancellation(t *testing.T) {
	if _, err := NewWorker(WorkerConfig{}); err == nil {
		t.Fatal("nil index accepted")
	}
	rig, _, _ := newWorkerRig(t, DefaultPolicy())
	if _, err := NewWorker(WorkerConfig{Index: rig.index, Policy: Policy{HardBytes: 1}}); err == nil {
		t.Fatal("invalid policy accepted")
	}
	worker, err := NewWorker(WorkerConfig{Index: rig.index})
	if err != nil || worker.interval != DefaultCleanupInterval || worker.policy != DefaultPolicy() {
		t.Fatalf("default worker = %#v, %v", worker, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := worker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled worker = %v", err)
	}
}

func TestWorkerPruneCancellationLeavesDurableQueue(t *testing.T) {
	policy := Policy{HardBytes: 100, SoftBytes: 85, TargetBytes: 75, Expiry: time.Hour}
	rig, worker, refs := newWorkerRig(t, policy, "expired")
	makeRecoverable(t, rig, refs[0], DispositionRejected, rig.now.Add(-policy.Expiry))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := worker.Prune(ctx, PruneSoft, 0, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled prune = %v", err)
	}
	status, err := worker.Status()
	if err != nil || status.QueuedCount != 1 || status.QueuedBytes != refs[0].Size {
		t.Fatalf("queued status = %#v, %v", status, err)
	}
}

func TestWorkerPropagatesQueuedUnlinkFailure(t *testing.T) {
	policy := Policy{HardBytes: 100, SoftBytes: 85, TargetBytes: 75, Expiry: time.Hour}
	rig, worker, refs := newWorkerRig(t, policy, "expired")
	makeRecoverable(t, rig, refs[0], DispositionRejected, rig.now.Add(-policy.Expiry))
	path := filepath.Join(rig.root, "sha256", refs[0].Digest[:2], refs[0].Digest[2:4], refs[0].Digest)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "child"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Prune(t.Context(), PruneSoft, 0, false); err == nil {
		t.Fatal("unlink failure ignored")
	}
}

func TestWorkerPropagatesClosedIndexErrors(t *testing.T) {
	rig, worker, _ := newWorkerRig(t, DefaultPolicy())
	if err := rig.index.db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Status(); err == nil {
		t.Fatal("closed-index status succeeded")
	}
	if _, err := worker.Prune(t.Context(), PruneSoft, 0, false); err == nil {
		t.Fatal("closed-index prune succeeded")
	}
	ticks := make(chan time.Time, 1)
	worker.ticks = ticks
	ticks <- time.Now()
	close(ticks)
	if err := worker.Run(t.Context()); err == nil {
		t.Fatal("closed-index tick succeeded")
	}
}
