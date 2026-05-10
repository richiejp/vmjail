package imagestore

import (
	"errors"
	"testing"
	"time"
)

func TestAllocateNoLimitReturnsEmptyPlan(t *testing.T) {
	plan, err := Allocate(nil, 1<<40, 0)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if len(plan.Evict) != 0 || plan.Freed != 0 {
		t.Errorf("plan = %+v, want empty", plan)
	}
}

func TestAllocateRejectsImageLargerThanQuota(t *testing.T) {
	_, err := Allocate(nil, 100, 50)
	if !errors.Is(err, ErrImageExceedsQuota) {
		t.Fatalf("err = %v, want ErrImageExceedsQuota", err)
	}
}

func TestAllocateRejectsNegativeIncoming(t *testing.T) {
	_, err := Allocate(nil, -1, 100)
	if err == nil {
		t.Fatal("expected error for negative incoming size")
	}
}

func TestAllocateNoEvictionNeededWhenRoomAvailable(t *testing.T) {
	entries := []Entry{
		{Digest: "sha256:a", Size: 30, LastUsed: time.Unix(1, 0)},
		{Digest: "sha256:b", Size: 20, LastUsed: time.Unix(2, 0)},
	}
	plan, err := Allocate(entries, 30, 100)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if len(plan.Evict) != 0 {
		t.Errorf("evict = %v, want none", plan.Evict)
	}
}

func TestAllocateEvictsLRUFirst(t *testing.T) {
	entries := []Entry{
		{Digest: "newest", Size: 30, LastUsed: time.Unix(300, 0)},
		{Digest: "oldest", Size: 30, LastUsed: time.Unix(100, 0)},
		{Digest: "middle", Size: 30, LastUsed: time.Unix(200, 0)},
	}
	// Quota 100, current 90, want 30 → must evict 20 → oldest (30) is enough.
	plan, err := Allocate(entries, 30, 100)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if len(plan.Evict) != 1 || plan.Evict[0] != "oldest" {
		t.Errorf("evict = %v, want [oldest]", plan.Evict)
	}
	if plan.Freed != 30 {
		t.Errorf("freed = %d, want 30", plan.Freed)
	}
}

func TestAllocateEvictsMultipleInLRUOrder(t *testing.T) {
	entries := []Entry{
		{Digest: "a", Size: 40, LastUsed: time.Unix(100, 0)},
		{Digest: "b", Size: 40, LastUsed: time.Unix(200, 0)},
		{Digest: "c", Size: 40, LastUsed: time.Unix(300, 0)},
	}
	// Quota 100, current 120, want 40 → over by 60 → evict a (40) + b (40) = 80 freed.
	plan, err := Allocate(entries, 40, 100)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	want := []string{"a", "b"}
	if len(plan.Evict) != 2 || plan.Evict[0] != want[0] || plan.Evict[1] != want[1] {
		t.Errorf("evict = %v, want %v", plan.Evict, want)
	}
}

func TestAllocateSkipsLockedEntries(t *testing.T) {
	entries := []Entry{
		{Digest: "locked-old", Size: 40, LastUsed: time.Unix(100, 0), Locked: true},
		{Digest: "free-newer", Size: 40, LastUsed: time.Unix(200, 0)},
	}
	// Quota 50, current 80, want 10 → over by 40 → must evict 40 worth.
	// locked-old is skipped, free-newer (40) covers it.
	plan, err := Allocate(entries, 10, 50)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if len(plan.Evict) != 1 || plan.Evict[0] != "free-newer" {
		t.Errorf("evict = %v, want [free-newer]", plan.Evict)
	}
}

func TestAllocateErrorsWhenLockedEntriesBlockEviction(t *testing.T) {
	entries := []Entry{
		{Digest: "locked", Size: 90, LastUsed: time.Unix(100, 0), Locked: true},
	}
	// Quota 100, current 90 (locked), want 20 → can't free anything.
	_, err := Allocate(entries, 20, 100)
	if !errors.Is(err, ErrCannotFreeEnough) {
		t.Fatalf("err = %v, want ErrCannotFreeEnough", err)
	}
}

func TestAllocateExactQuotaFitNoEviction(t *testing.T) {
	entries := []Entry{
		{Digest: "a", Size: 50, LastUsed: time.Unix(1, 0)},
	}
	plan, err := Allocate(entries, 50, 100)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if len(plan.Evict) != 0 {
		t.Errorf("evict = %v, want none — exactly at quota is fine", plan.Evict)
	}
}

func TestAllocateZeroIncomingAcceptedEvenWhenFull(t *testing.T) {
	entries := []Entry{
		{Digest: "a", Size: 100, LastUsed: time.Unix(1, 0)},
	}
	// Pulling a "0-byte" image (entirely shared layers we already have)
	// against a full cache should just succeed — nothing to fit.
	plan, err := Allocate(entries, 0, 100)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if len(plan.Evict) != 0 {
		t.Errorf("evict = %v, want none for zero-byte alloc", plan.Evict)
	}
}

func TestFilterOlderThan(t *testing.T) {
	now := time.Unix(1000, 0)
	entries := []Entry{
		{Digest: "old-locked", LastUsed: time.Unix(100, 0), Locked: true},
		{Digest: "old-free", LastUsed: time.Unix(200, 0)},
		{Digest: "recent", LastUsed: time.Unix(900, 0)},
	}
	// cutoff 500s → anything older than 500s before now (i.e. < t=500) is stale.
	stale := FilterOlderThan(entries, 500*time.Second, now)
	if len(stale) != 1 || stale[0] != "old-free" {
		t.Errorf("stale = %v, want [old-free]", stale)
	}
}

func TestFilterOlderThanNonPositiveCutoffReturnsNil(t *testing.T) {
	if got := FilterOlderThan(nil, 0, time.Now()); got != nil {
		t.Errorf("got %v, want nil for zero cutoff", got)
	}
	if got := FilterOlderThan(nil, -time.Hour, time.Now()); got != nil {
		t.Errorf("got %v, want nil for negative cutoff", got)
	}
}
