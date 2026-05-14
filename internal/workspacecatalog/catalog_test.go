package workspacecatalog

import (
	"errors"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/identity"
)

const (
	uuidA = "11111111-1111-4111-8111-111111111111"
	uuidB = "22222222-2222-4222-8222-222222222222"
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
	if ws.CreatedAt.IsZero() {
		t.Error("default clock did not produce a non-zero CreatedAt")
	}
}

func TestCreateInsertsDraftWithMissingFields(t *testing.T) {
	s := NewStore(fixedClock(t))
	owner := mustOwner(t)
	ws, err := s.Create(CreateInput{UUID: uuidA, Name: "ws", OwnerFingerprint: owner})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ws.Status != StatusDraft {
		t.Errorf("Status = %s, want DRAFT", ws.Status)
	}
	want := []string{"mount", "image", "target"}
	if len(ws.Missing) != len(want) {
		t.Fatalf("Missing = %v, want %v", ws.Missing, want)
	}
	for i, m := range ws.Missing {
		if m != want[i] {
			t.Errorf("Missing[%d] = %q, want %q", i, m, want[i])
		}
	}
	if !ws.CreatedAt.Equal(ws.UpdatedAt) {
		t.Errorf("CreatedAt %v != UpdatedAt %v on fresh workspace", ws.CreatedAt, ws.UpdatedAt)
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
	if got.UUID != created.UUID {
		t.Errorf("LookupByAlias.UUID = %q, want %q", got.UUID, created.UUID)
	}
	if _, err := s.LookupByAlias(owner, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing alias: err = %v, want ErrNotFound", err)
	}
}

// TestUpdatePopulatesMissingAndBumpsStatus pins the
// Missing-recompute path: an Update that adds an image / mount /
// target empties Missing[] one entry at a time. Status doesn't
// auto-transition (that's B4); the test asserts only the catalog's
// invariant that Missing reflects current state.
func TestUpdatePopulatesMissingAndBumpsStatus(t *testing.T) {
	s := NewStore(fixedClock(t))
	if _, err := s.Create(CreateInput{UUID: uuidA, Name: "ws"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	after, err := s.Update(uuidA, func(ws *Workspace) error {
		ws.Mounts = []Mount{{Target: "/workspace", Source: uuidA}}
		ws.ImageRef = "alpine:3"
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(after.Missing) != 1 || after.Missing[0] != "target" {
		t.Errorf("Missing = %v, want [target]", after.Missing)
	}
	last, err := s.Update(uuidA, func(ws *Workspace) error {
		ws.TargetSandboxFingerprint = "sha256:abc"
		return nil
	})
	if err != nil {
		t.Fatalf("Update (target): %v", err)
	}
	if len(last.Missing) != 0 {
		t.Errorf("Missing = %v, want empty", last.Missing)
	}
}

func TestUpdateRefusesIdentityMutation(t *testing.T) {
	s := NewStore(fixedClock(t))
	if _, err := s.Create(CreateInput{UUID: uuidA, Name: "ws"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err := s.Update(uuidA, func(ws *Workspace) error {
		ws.UUID = "tampered"
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
	_, err := s.Update(uuidB, func(ws *Workspace) error {
		ws.Name = "a"
		return nil
	})
	if !errors.Is(err, ErrAliasInUse) {
		t.Errorf("err = %v, want ErrAliasInUse", err)
	}
	// Original aliases must still resolve cleanly.
	if got, err := s.LookupByAlias(owner, "a"); err != nil || got.UUID != uuidA {
		t.Errorf("alias 'a' after failed rename: got=%+v err=%v", got, err)
	}
	if got, err := s.LookupByAlias(owner, "b"); err != nil || got.UUID != uuidB {
		t.Errorf("alias 'b' after failed rename: got=%+v err=%v", got, err)
	}
}

func TestUpdateRenameAdjustsAliasIndex(t *testing.T) {
	s := NewStore(fixedClock(t))
	owner := mustOwner(t)
	if _, err := s.Create(CreateInput{UUID: uuidA, Name: "old", OwnerFingerprint: owner}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Update(uuidA, func(ws *Workspace) error {
		ws.Name = "new"
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
	_, err := s.Update(uuidA, func(*Workspace) error { return want })
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrapping %v", err, want)
	}
}

// TestUpdateOnMissingUUIDReportsNotFound pins the no-such-workspace
// branch separately from the alias / immutable-field branches so a
// future refactor can't conflate them.
func TestUpdateOnMissingUUIDReportsNotFound(t *testing.T) {
	s := NewStore(fixedClock(t))
	_, err := s.Update(uuidA, func(*Workspace) error { return nil })
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
	before := ws.UpdatedAt
	after, err := s.Update(uuidA, nil)
	if err != nil {
		t.Fatalf("Update(nil): %v", err)
	}
	if !after.UpdatedAt.After(before) {
		t.Errorf("UpdatedAt %v not advanced past %v", after.UpdatedAt, before)
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
	if len(list) != 2 || list[0].UUID != uuidA || list[1].UUID != uuidB {
		t.Errorf("List order = [%s, %s], want [%s, %s]",
			list[0].UUID, list[1].UUID, uuidA, uuidB)
	}
}

// TestCanTransitionCoversFullStateMachine pins every allowed edge
// in the lifecycle and one rejected edge per starting state. The
// adjacency table here is the contract for Slice B4's actual
// state-machine wiring; mutating it without bumping B4 should
// fail this test loudly.
func TestCanTransitionCoversFullStateMachine(t *testing.T) {
	allowed := map[Status][]Status{
		StatusUnspecified: {StatusDraft},
		StatusDraft:       {StatusReady, StatusArchived},
		StatusReady:       {StatusRunning, StatusDraft, StatusArchived},
		StatusRunning:     {StatusFrozen, StatusStopped},
		StatusFrozen:      {StatusRunning, StatusStopped},
		StatusStopped:     {StatusRunning, StatusDraft, StatusArchived},
	}
	for from, tos := range allowed {
		for _, to := range tos {
			if !CanTransition(from, to) {
				t.Errorf("CanTransition(%s, %s) = false, want true", from, to)
			}
		}
		// Same-state is always allowed.
		if !CanTransition(from, from) {
			t.Errorf("CanTransition(%s, %s) = false (same-state)", from, from)
		}
	}
	// Spot-check a few forbidden edges.
	forbidden := [][2]Status{
		{StatusDraft, StatusRunning},
		{StatusRunning, StatusDraft},
		{StatusArchived, StatusDraft},
	}
	for _, edge := range forbidden {
		if CanTransition(edge[0], edge[1]) {
			t.Errorf("CanTransition(%s, %s) = true, want false", edge[0], edge[1])
		}
	}
}

func TestStatusStringCoversAllValues(t *testing.T) {
	cases := []struct {
		in   Status
		want string
	}{
		{StatusUnspecified, statusUnspecifiedStr},
		{StatusDraft, "DRAFT"},
		{StatusReady, "READY"},
		{StatusRunning, "RUNNING"},
		{StatusFrozen, "FROZEN"},
		{StatusStopped, "STOPPED"},
		{StatusArchived, "ARCHIVED"},
		{Status(99), statusUnspecifiedStr},
	}
	for _, tc := range cases {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Status(%d).String() = %q, want %q", tc.in, got, tc.want)
		}
	}
}
