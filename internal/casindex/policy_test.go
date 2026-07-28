package casindex

import (
	"errors"
	"testing"
	"time"
)

func TestBuildPlanWeightedOrderingAndSoftTarget(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	policy := Policy{HardBytes: 1000, SoftBytes: 850, TargetBytes: 750, Expiry: 100 * time.Hour}
	chunks := []Chunk{
		{Digest: "active", Size: 100, LastUse: now.Add(-90 * time.Hour), Recoverability: RecoverabilityRecoverable, Disposition: DispositionActive},
		{Digest: "superseded", Size: 100, LastUse: now.Add(-50 * time.Hour), Recoverability: RecoverabilityRecoverable, Disposition: DispositionSuperseded},
		{Digest: "rejected", Size: 100, LastUse: now.Add(-time.Hour), Recoverability: RecoverabilityRecoverable, Disposition: DispositionRejected},
		{Digest: "protected", Size: 500, LastUse: now.Add(-200 * time.Hour), Recoverability: RecoverabilityProtected, Disposition: DispositionRejected},
		{Digest: "referenced", Size: 500, LastUse: now.Add(-200 * time.Hour), Recoverability: RecoverabilityRecoverable, Disposition: DispositionRejected, References: []Reference{{SyncKey: "ws"}}},
	}
	plan, err := BuildPlan(chunks, 950, 0, now, policy, PruneSoft)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 2 || plan.Candidates[0].Digest != "superseded" || plan.Candidates[1].Digest != "rejected" {
		t.Fatalf("candidate ordering = %#v", plan.Candidates)
	}
	if plan.UsageAfter != policy.TargetBytes {
		t.Fatalf("usage after = %d, want %d", plan.UsageAfter, policy.TargetBytes)
	}
}

func TestBuildPlanExpiryAndDeterministicTieBreak(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	policy := Policy{HardBytes: 1000, SoftBytes: 850, TargetBytes: 750, Expiry: 100 * time.Hour}
	chunks := []Chunk{
		{Digest: "b", Size: 10, LastUse: now.Add(-policy.Expiry), Recoverability: RecoverabilityRecoverable, Disposition: DispositionActive},
		{Digest: "a", Size: 10, LastUse: now.Add(-policy.Expiry), Recoverability: RecoverabilityRecoverable, Disposition: DispositionActive},
		{Digest: "fresh", Size: 10, LastUse: now, Recoverability: RecoverabilityRecoverable, Disposition: DispositionRejected},
	}
	plan, err := BuildPlan(chunks, 100, 0, now, policy, PruneSoft)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 2 || plan.Candidates[0].Digest != "a" || plan.Candidates[1].Digest != "b" {
		t.Fatalf("expiry candidates = %#v", plan.Candidates)
	}
}

func TestBuildPlanHardLimitAndProtectedRejection(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	policy := Policy{HardBytes: 1000, SoftBytes: 850, TargetBytes: 750, Expiry: 100 * time.Hour}
	chunks := []Chunk{
		{Digest: "eligible", Size: 200, LastUse: now, Recoverability: RecoverabilityRecoverable, Disposition: DispositionActive},
		{Digest: "protected", Size: 900, LastUse: now, Recoverability: RecoverabilityProtected, Disposition: DispositionActive},
	}
	plan, err := BuildPlan(chunks, 950, 200, now, policy, PruneHard)
	if err != nil || len(plan.Candidates) != 1 || plan.UsageAfter+plan.IncomingBytes > policy.HardBytes {
		t.Fatalf("hard plan = %#v, %v", plan, err)
	}
	_, err = BuildPlan(chunks[1:], 950, 200, now, policy, PruneHard)
	if !errors.Is(err, ErrInsufficientReclaim) {
		t.Fatalf("protected pressure error = %v", err)
	}
}

func TestBuildPlanValidatesPolicyAndSaturatesPressure(t *testing.T) {
	if _, err := BuildPlan(nil, 0, 0, time.Now(), Policy{}, PruneSoft); err == nil {
		t.Fatal("invalid policy accepted")
	}
	policy := Policy{HardBytes: 100, SoftBytes: 90, TargetBytes: 80, Expiry: time.Hour}
	chunk := Chunk{Digest: "x", Size: 1, LastUse: time.Time{}, Recoverability: RecoverabilityRecoverable, Disposition: DispositionRejected}
	plan, err := BuildPlan([]Chunk{chunk}, ^uint64(0), 1, time.Now(), policy, PruneHard)
	if err == nil || len(plan.Candidates) != 1 || plan.Candidates[0].Score > 1 {
		t.Fatalf("saturated plan = %#v, %v", plan, err)
	}
}

func TestBuildPlanLargeExpiredChunkSaturatesUsageAtZero(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	policy := Policy{HardBytes: 1000, SoftBytes: 850, TargetBytes: 750, Expiry: time.Hour}
	chunk := Chunk{
		Digest: "large", Size: 200, LastUse: now.Add(-policy.Expiry),
		Recoverability: RecoverabilityRecoverable, Disposition: DispositionDeleted,
	}
	plan, err := BuildPlan([]Chunk{chunk}, 100, 0, now, policy, PruneSoft)
	if err != nil || plan.UsageAfter != 0 || len(plan.Candidates) != 1 {
		t.Fatalf("large expiry plan = %#v, %v", plan, err)
	}
}

func TestDefaultPolicy(t *testing.T) {
	policy := DefaultPolicy()
	if policy.HardBytes != 100<<30 || policy.SoftBytes != 85<<30 || policy.TargetBytes != 75<<30 || policy.Expiry != 180*24*time.Hour {
		t.Fatalf("default policy = %#v", policy)
	}
}
