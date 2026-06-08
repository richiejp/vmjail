package workspacecatalog

import (
	"errors"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/identity"
)

const (
	uuidA = "11111111-1111-4111-8111-111111111111"
	uuidB = "22222222-2222-4222-8222-222222222222"

	missingTarget = "target"
)

// fixedClock returns a deterministic now() so timestamp assertions
// don't depend on wall-clock drift. Each tick advances by one
// second, which is more than the test needs but keeps the
// CreatedAt < UpdatedAt invariant easy to read.
func fixedClock(t *testing.T) func() time.Time {
	t.Helper()
	start := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	var n int64
	return func() time.Time {
		out := start.Add(time.Duration(n) * time.Second)
		n++
		return out
	}
}

func mustOwner(t *testing.T) identity.Fingerprint {
	t.Helper()
	id, err := identity.Generate(identity.RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return id.Fingerprint()
}

func TestNewStoreDefaultsClock(t *testing.T) {
	s := NewStore(nil)
	if s == nil {
		t.Fatal("NewStore(nil) returned nil")
	}
	// A no-op sanity Create exercises the default time.Now path.
	ws, err := s.Create(CreateInput{UUID: uuidA, Name: "ws"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ws.Metadata.CreatedAt.IsZero() {
		t.Error("default clock did not produce a non-zero CreatedAt")
	}
}

func TestMissingFieldsDevcontainerSatisfiesImageAndMount(t *testing.T) {
	// A devcontainer provides the image and the workspace-content mount,
	// so a spec with only DevcontainerPath + target is complete; the node
	// resolves the actual image/mount at dispatch.
	dc := Spec{DevcontainerPath: ".devcontainer/devcontainer.json", TargetSandboxFingerprint: "fp"}
	if got := missingFields(dc); len(got) != 0 {
		t.Errorf("missingFields(devcontainer spec) = %v, want none", got)
	}
	// Without a target it's still incomplete — only the target check fires.
	dcNoTarget := Spec{DevcontainerPath: ".devcontainer/devcontainer.json"}
	if got := missingFields(dcNoTarget); len(got) != 1 || got[0] != missingTarget {
		t.Errorf("missingFields(no target) = %v, want [target]", got)
	}
	// No devcontainer, no image/mount: the original behaviour is intact.
	bare := Spec{}
	if got := missingFields(bare); len(got) != 3 {
		t.Errorf("missingFields(bare) = %v, want mount/image/target", got)
	}
}

func TestCreateInsertsNotReadyWithMissingFields(t *testing.T) {
	s := NewStore(fixedClock(t))
	owner := mustOwner(t)
	ws, err := s.Create(CreateInput{UUID: uuidA, Name: "ws", OwnerFingerprint: owner})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ws.Status.IsTrue(ConditionReady) {
		t.Errorf("fresh workspace should not be Ready, conditions=%+v", ws.Status.Conditions)
	}
	if ws.Metadata.Generation != 1 {
		t.Errorf("Generation = %d, want 1 on Create", ws.Metadata.Generation)
	}
	want := []string{"mount", "image", "target"}
	if len(ws.Status.Missing) != len(want) {
		t.Fatalf("Missing = %v, want %v", ws.Status.Missing, want)
	}
	for i, m := range ws.Status.Missing {
		if m != want[i] {
			t.Errorf("Missing[%d] = %q, want %q", i, m, want[i])
		}
	}
	if !ws.Metadata.CreatedAt.Equal(ws.Metadata.UpdatedAt) {
		t.Errorf("CreatedAt %v != UpdatedAt %v on fresh workspace",
			ws.Metadata.CreatedAt, ws.Metadata.UpdatedAt)
	}
}

func TestCreateRejectsDuplicateAliasPerOwner(t *testing.T) {
	s := NewStore(fixedClock(t))
	owner := mustOwner(t)
	if _, err := s.Create(CreateInput{UUID: uuidA, Name: "ws", OwnerFingerprint: owner}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := s.Create(CreateInput{UUID: uuidB, Name: "ws", OwnerFingerprint: owner})
	if !errors.Is(err, ErrAliasInUse) {
		t.Errorf("err = %v, want ErrAliasInUse", err)
	}
}

// TestCreateAllowsSameNameAcrossOwners pins that the alias index
// is per-owner. Two different user-hosts can each call their
// workspace "myapp"; only an intra-owner collision is refused.
func TestCreateAllowsSameNameAcrossOwners(t *testing.T) {
	s := NewStore(fixedClock(t))
	ownerA := mustOwner(t)
	ownerB := mustOwner(t)
	if _, err := s.Create(CreateInput{UUID: uuidA, Name: "myapp", OwnerFingerprint: ownerA}); err != nil {
		t.Fatalf("ownerA Create: %v", err)
	}
	if _, err := s.Create(CreateInput{UUID: uuidB, Name: "myapp", OwnerFingerprint: ownerB}); err != nil {
		t.Errorf("ownerB Create: %v, want nil (alias index is per-owner)", err)
	}
}

func TestCreateRejectsDuplicateUUID(t *testing.T) {
	s := NewStore(fixedClock(t))
	if _, err := s.Create(CreateInput{UUID: uuidA, Name: "a"}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if _, err := s.Create(CreateInput{UUID: uuidA, Name: "b"}); err == nil {
		t.Error("expected duplicate-UUID rejection, got nil")
	}
}

func TestGetReturnsErrNotFoundForUnknownUUID(t *testing.T) {
	s := NewStore(fixedClock(t))
	_, err := s.Get(uuidA)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestLookupByAliasResolvesToTheSameRecord(t *testing.T) {
	s := NewStore(fixedClock(t))
	owner := mustOwner(t)
	created, err := s.Create(CreateInput{UUID: uuidA, Name: "ws", OwnerFingerprint: owner})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.LookupByAlias(owner, "ws")
	if err != nil {
		t.Fatalf("LookupByAlias: %v", err)
	}
	if got.Metadata.UUID != created.Metadata.UUID {
		t.Errorf("LookupByAlias.UUID = %q, want %q", got.Metadata.UUID, created.Metadata.UUID)
	}
	if _, err := s.LookupByAlias(owner, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing alias: err = %v, want ErrNotFound", err)
	}
}

// TestSpecUpdatePopulatesMissingAndBumpsGeneration pins the
// Missing-recompute path: an Update that adds an image / mount /
// target empties Missing[] one entry at a time. Generation
// increments per MutateSpec call; the Ready condition flips True
// once Missing is empty.
func TestSpecUpdatePopulatesMissingAndBumpsGeneration(t *testing.T) {
	s := NewStore(fixedClock(t))
	if _, err := s.Create(CreateInput{UUID: uuidA, Name: "ws"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	after, err := s.Update(uuidA, MutateSpec, func(ws *Workspace) error {
		ws.Spec.Mounts = []Mount{{Target: "/workspace", Source: uuidA}}
		ws.Spec.ImageRef = "alpine:3"
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(after.Status.Missing) != 1 || after.Status.Missing[0] != "target" {
		t.Errorf("Missing = %v, want [target]", after.Status.Missing)
	}
	if after.Metadata.Generation != 2 {
		t.Errorf("Generation = %d, want 2 after spec edit", after.Metadata.Generation)
	}
	last, err := s.Update(uuidA, MutateSpec, func(ws *Workspace) error {
		ws.Spec.TargetSandboxFingerprint = "sha256:abc"
		return nil
	})
	if err != nil {
		t.Fatalf("Update (target): %v", err)
	}
	if len(last.Status.Missing) != 0 {
		t.Errorf("Missing = %v, want empty", last.Status.Missing)
	}
	if !last.Status.IsTrue(ConditionReady) {
		t.Errorf("Ready not True after all fields populated, conditions=%+v", last.Status.Conditions)
	}
	if last.Metadata.Generation != 3 {
		t.Errorf("Generation = %d, want 3 after two spec edits", last.Metadata.Generation)
	}
}

// TestStatusUpdateDoesNotBumpGeneration pins the MutateStatus
// branch: Launch/Stop touch Status only and must NOT increment
// generation (otherwise the running Sandbox's spec snapshot would
// be flagged as stale on every dispatch tick).
func TestStatusUpdateDoesNotBumpGeneration(t *testing.T) {
	s := NewStore(fixedClock(t))
	created, err := s.Create(CreateInput{UUID: uuidA, Name: "ws"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gen := created.Metadata.Generation
	now := created.Metadata.CreatedAt
	after, err := s.Update(uuidA, MutateStatus, func(ws *Workspace) error {
		ws.Status.SetCondition(ConditionDispatched, ConditionStatusTrue, "Launched", "", now)
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if after.Metadata.Generation != gen {
		t.Errorf("Generation moved %d→%d on MutateStatus", gen, after.Metadata.Generation)
	}
	if !after.Status.IsTrue(ConditionDispatched) {
		t.Errorf("Dispatched not True after SetCondition, conditions=%+v", after.Status.Conditions)
	}
}

func TestUpdateRefusesIdentityMutation(t *testing.T) {
	s := NewStore(fixedClock(t))
	if _, err := s.Create(CreateInput{UUID: uuidA, Name: "ws"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err := s.Update(uuidA, MutateSpec, func(ws *Workspace) error {
		ws.Metadata.UUID = "tampered"
		return nil
	})
	if !errors.Is(err, ErrImmutableField) {
		t.Errorf("UUID mutation: err = %v, want ErrImmutableField", err)
	}
}

// TestUpdateRenameRefusesCollision pins that renaming into another
// owned workspace's slot is rejected and rolls back. Without this
// you could squat someone else's alias by Update.
func TestUpdateRenameRefusesCollision(t *testing.T) {
	s := NewStore(fixedClock(t))
	owner := mustOwner(t)
	if _, err := s.Create(CreateInput{UUID: uuidA, Name: "a", OwnerFingerprint: owner}); err != nil {
		t.Fatalf("Create a: %v", err)
	}
	if _, err := s.Create(CreateInput{UUID: uuidB, Name: "b", OwnerFingerprint: owner}); err != nil {
		t.Fatalf("Create b: %v", err)
	}
	_, err := s.Update(uuidB, MutateMetadata, func(ws *Workspace) error {
		ws.Metadata.Name = "a"
		return nil
	})
	if !errors.Is(err, ErrAliasInUse) {
		t.Errorf("err = %v, want ErrAliasInUse", err)
	}
	// Original aliases must still resolve cleanly.
	if got, err := s.LookupByAlias(owner, "a"); err != nil || got.Metadata.UUID != uuidA {
		t.Errorf("alias 'a' after failed rename: got=%+v err=%v", got, err)
	}
	if got, err := s.LookupByAlias(owner, "b"); err != nil || got.Metadata.UUID != uuidB {
		t.Errorf("alias 'b' after failed rename: got=%+v err=%v", got, err)
	}
}

func TestUpdateRenameAdjustsAliasIndex(t *testing.T) {
	s := NewStore(fixedClock(t))
	owner := mustOwner(t)
	if _, err := s.Create(CreateInput{UUID: uuidA, Name: "old", OwnerFingerprint: owner}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Update(uuidA, MutateMetadata, func(ws *Workspace) error {
		ws.Metadata.Name = "new"
		return nil
	}); err != nil {
		t.Fatalf("Update rename: %v", err)
	}
	if _, err := s.LookupByAlias(owner, "old"); !errors.Is(err, ErrNotFound) {
		t.Errorf("old alias should be gone, got err=%v", err)
	}
	if _, err := s.LookupByAlias(owner, "new"); err != nil {
		t.Errorf("new alias should resolve, got err=%v", err)
	}
}

func TestUpdatePropagatesFnError(t *testing.T) {
	s := NewStore(fixedClock(t))
	if _, err := s.Create(CreateInput{UUID: uuidA, Name: "ws"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := errors.New("synthetic")
	_, err := s.Update(uuidA, MutateSpec, func(*Workspace) error { return want })
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrapping %v", err, want)
	}
}

// TestUpdateOnMissingUUIDReportsNotFound pins the no-such-workspace
// branch separately from the alias / immutable-field branches so a
// future refactor can't conflate them.
func TestUpdateOnMissingUUIDReportsNotFound(t *testing.T) {
	s := NewStore(fixedClock(t))
	_, err := s.Update(uuidA, MutateSpec, func(*Workspace) error { return nil })
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestUpdateNilFnTouchesUpdatedAt covers the lock-renewal / touch
// use case explicitly so future readers don't think nil fn is
// dead code.
func TestUpdateNilFnTouchesUpdatedAt(t *testing.T) {
	s := NewStore(fixedClock(t))
	ws, err := s.Create(CreateInput{UUID: uuidA, Name: "ws"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	before := ws.Metadata.UpdatedAt
	after, err := s.Update(uuidA, MutateMetadata, nil)
	if err != nil {
		t.Fatalf("Update(nil): %v", err)
	}
	if !after.Metadata.UpdatedAt.After(before) {
		t.Errorf("UpdatedAt %v not advanced past %v", after.Metadata.UpdatedAt, before)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	s := NewStore(fixedClock(t))
	owner := mustOwner(t)
	if _, err := s.Create(CreateInput{UUID: uuidA, Name: "ws", OwnerFingerprint: owner}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.Delete(uuidA)
	s.Delete(uuidA) // second call is a no-op
	if _, err := s.Get(uuidA); !errors.Is(err, ErrNotFound) {
		t.Errorf("after Delete: err = %v, want ErrNotFound", err)
	}
	if _, err := s.LookupByAlias(owner, "ws"); !errors.Is(err, ErrNotFound) {
		t.Errorf("alias should be cleared, got err=%v", err)
	}
}

func TestListReturnsSortedSnapshot(t *testing.T) {
	s := NewStore(fixedClock(t))
	owner := mustOwner(t)
	if _, err := s.Create(CreateInput{UUID: uuidB, Name: "z", OwnerFingerprint: owner}); err != nil {
		t.Fatalf("Create B: %v", err)
	}
	if _, err := s.Create(CreateInput{UUID: uuidA, Name: "a", OwnerFingerprint: owner}); err != nil {
		t.Fatalf("Create A: %v", err)
	}
	list := s.List(nil)
	if len(list) != 2 || list[0].Metadata.UUID != uuidA || list[1].Metadata.UUID != uuidB {
		t.Errorf("List order = [%s, %s], want [%s, %s]",
			list[0].Metadata.UUID, list[1].Metadata.UUID, uuidA, uuidB)
	}
}

// TestConditionStatusStringCoversAllValues pins the diagnostic
// rendering of ConditionStatus so tests can match on it without
// duplicating the literal table.
func TestConditionStatusStringCoversAllValues(t *testing.T) {
	cases := []struct {
		in   ConditionStatus
		want string
	}{
		{ConditionStatusTrue, "True"},
		{ConditionStatusFalse, "False"},
		{ConditionStatusUnknown, "Unknown"},
		{ConditionStatusUnspecified, conditionStatusUnspecifiedStr},
		{ConditionStatus(99), conditionStatusUnspecifiedStr},
	}
	for _, tc := range cases {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("ConditionStatus(%d).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSetConditionPreservesTransitionTimeOnNoOp pins that an
// SetCondition that doesn't actually flip the status carries the
// prior LastTransitionTime forward. K8s readers depend on this
// to detect real edges instead of churn from periodic re-sets.
func TestSetConditionPreservesTransitionTimeOnNoOp(t *testing.T) {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Minute)
	status := Status{}
	status.SetCondition(ConditionDispatched, ConditionStatusTrue, "First", "", now)
	status.SetCondition(ConditionDispatched, ConditionStatusTrue, "Second", "", later)
	c := status.Condition(ConditionDispatched)
	if !c.LastTransitionTime.Equal(now) {
		t.Errorf("LastTransitionTime = %v, want %v (preserved across no-op set)", c.LastTransitionTime, now)
	}
	if c.Reason != "Second" {
		t.Errorf("Reason = %q, want %q (latest reason overwrites)", c.Reason, "Second")
	}
}

// TestLockSemantics pins the four Lock helpers (IsHeld /
// IsExpired / HeldBy / HeldByOther) used by the orchestrator's
// guardLock + Acquire/Release. Built as a table because all four
// share the same {owner, expires_at, now, caller} dimensions —
// keeping them in one place stops the helpers from drifting
// apart.
func TestLockSemantics(t *testing.T) {
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)
	owner := mustOwner(t)
	other := mustOwner(t)
	cases := []struct {
		name           string
		lock           Lock
		caller         identity.Fingerprint
		wantHeld       bool
		wantExpired    bool
		wantHeldBy     bool
		wantHeldByOthr bool
	}{
		{"unheld", Lock{}, owner, false, false, false, false},
		{"held-self-fresh", Lock{OwnerFingerprint: owner, ExpiresAt: now.Add(time.Hour)}, owner, true, false, true, false},
		{"held-self-no-expiry", Lock{OwnerFingerprint: owner}, owner, true, false, true, false},
		{"held-other-fresh", Lock{OwnerFingerprint: owner, ExpiresAt: now.Add(time.Hour)}, other, true, false, false, true},
		{"held-self-expired", Lock{OwnerFingerprint: owner, ExpiresAt: now.Add(-time.Hour)}, owner, true, true, false, false},
		{"held-other-expired", Lock{OwnerFingerprint: owner, ExpiresAt: now.Add(-time.Hour)}, other, true, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.lock.IsHeld(); got != tc.wantHeld {
				t.Errorf("IsHeld = %v, want %v", got, tc.wantHeld)
			}
			if got := tc.lock.IsExpired(now); got != tc.wantExpired {
				t.Errorf("IsExpired(now) = %v, want %v", got, tc.wantExpired)
			}
			if got := tc.lock.HeldBy(tc.caller, now); got != tc.wantHeldBy {
				t.Errorf("HeldBy(caller, now) = %v, want %v", got, tc.wantHeldBy)
			}
			if got := tc.lock.HeldByOther(tc.caller, now); got != tc.wantHeldByOthr {
				t.Errorf("HeldByOther(caller, now) = %v, want %v", got, tc.wantHeldByOthr)
			}
		})
	}
}
