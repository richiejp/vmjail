package nodectrl_test

import (
	"errors"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
)

func fp(b byte) identity.Fingerprint {
	var f identity.Fingerprint
	f[0] = b
	return f
}

func TestRegisterRecordsEntry(t *testing.T) {
	t0 := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	r := nodectrl.NewRegistry(func() time.Time { return t0 })

	got := r.Register(fp(1), "", "v0.1.0", nodectrl.Resources{CPUs: 8, MemoryBytes: 16 << 30})
	if !got.Equal(t0) {
		t.Errorf("Register accepted_at = %v, want %v", got, t0)
	}

	entry, ok := r.Lookup(fp(1))
	if !ok {
		t.Fatal("Lookup miss after Register")
	}
	if entry.Version != "v0.1.0" {
		t.Errorf("Version = %q, want %q", entry.Version, "v0.1.0")
	}
	if entry.Resources.CPUs != 8 {
		t.Errorf("Resources.CPUs = %d, want 8", entry.Resources.CPUs)
	}
	if !entry.RegisteredAt.Equal(t0) || !entry.LastSeenAt.Equal(t0) {
		t.Errorf("timestamps = %v / %v, want both %v", entry.RegisteredAt, entry.LastSeenAt, t0)
	}
}

func TestRegisterPreservesRegisteredAt(t *testing.T) {
	t0 := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	now := t0
	r := nodectrl.NewRegistry(func() time.Time { return now })

	r.Register(fp(1), "", "v0.1.0", nodectrl.Resources{CPUs: 4})
	now = t1
	r.Register(fp(1), "", "v0.2.0", nodectrl.Resources{CPUs: 16})

	entry, _ := r.Lookup(fp(1))
	if !entry.RegisteredAt.Equal(t0) {
		t.Errorf("RegisteredAt = %v, want %v (preserved across re-register)", entry.RegisteredAt, t0)
	}
	if !entry.LastSeenAt.Equal(t1) {
		t.Errorf("LastSeenAt = %v, want %v", entry.LastSeenAt, t1)
	}
	if entry.Version != "v0.2.0" || entry.Resources.CPUs != 16 {
		t.Errorf("re-register did not overwrite version/resources: %+v", entry)
	}
}

func TestHeartbeatRefreshesLastSeen(t *testing.T) {
	t0 := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	t1 := t0.Add(30 * time.Second)
	now := t0
	r := nodectrl.NewRegistry(func() time.Time { return now })

	r.Register(fp(1), "", "v0.1.0", nodectrl.Resources{CPUs: 4, MemoryBytes: 1 << 30})
	now = t1

	seen, err := r.Heartbeat(fp(1), nodectrl.Resources{CPUs: 4, MemoryBytes: 512 << 20})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !seen.Equal(t1) {
		t.Errorf("Heartbeat seen_at = %v, want %v", seen, t1)
	}

	entry, _ := r.Lookup(fp(1))
	if !entry.LastSeenAt.Equal(t1) {
		t.Errorf("LastSeenAt = %v, want %v", entry.LastSeenAt, t1)
	}
	if entry.Resources.MemoryBytes != 512<<20 {
		t.Errorf("Heartbeat resources not applied: %+v", entry.Resources)
	}
}

func TestHeartbeatZeroResourcesPreservesPrevious(t *testing.T) {
	r := nodectrl.NewRegistry(nil)
	r.Register(fp(1), "", "v0.1.0", nodectrl.Resources{CPUs: 4, MemoryBytes: 1 << 30})

	if _, err := r.Heartbeat(fp(1), nodectrl.Resources{}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	entry, _ := r.Lookup(fp(1))
	if entry.Resources.CPUs != 4 || entry.Resources.MemoryBytes != 1<<30 {
		t.Errorf("zero-resource heartbeat overwrote: %+v", entry.Resources)
	}
}

func TestHeartbeatUnknownSandbox(t *testing.T) {
	r := nodectrl.NewRegistry(nil)
	_, err := r.Heartbeat(fp(7), nodectrl.Resources{})
	if !errors.Is(err, nodectrl.ErrUnknownNode) {
		t.Errorf("err = %v, want ErrUnknownNode", err)
	}
}

func TestForgetReleasesEntry(t *testing.T) {
	r := nodectrl.NewRegistry(nil)
	r.Register(fp(1), "", "v0", nodectrl.Resources{CPUs: 1})
	r.Forget(fp(1))
	if _, ok := r.Lookup(fp(1)); ok {
		t.Error("entry still present after Forget")
	}
	// Idempotent — second Forget is a no-op.
	r.Forget(fp(1))
	// Heartbeat now fails.
	if _, err := r.Heartbeat(fp(1), nodectrl.Resources{}); !errors.Is(err, nodectrl.ErrUnknownNode) {
		t.Errorf("post-Forget Heartbeat err = %v, want ErrUnknownNode", err)
	}
}

func TestSnapshotIsSortedByFingerprint(t *testing.T) {
	r := nodectrl.NewRegistry(nil)
	r.Register(fp(3), "", "v0", nodectrl.Resources{CPUs: 3})
	r.Register(fp(1), "", "v0", nodectrl.Resources{CPUs: 1})
	r.Register(fp(2), "", "v0", nodectrl.Resources{CPUs: 2})

	snap := r.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot len = %d, want 3", len(snap))
	}
	for i, want := range []uint32{1, 2, 3} {
		if snap[i].Resources.CPUs != want {
			t.Errorf("snap[%d].CPUs = %d, want %d (unsorted)", i, snap[i].Resources.CPUs, want)
		}
	}
}

func TestRegisteredFingerprintsSortedAndForgets(t *testing.T) {
	r := nodectrl.NewRegistry(nil)
	if got := r.RegisteredFingerprints(); len(got) != 0 {
		t.Fatalf("empty registry: len = %d, want 0", len(got))
	}
	r.Register(fp(3), "", "v0", nodectrl.Resources{})
	r.Register(fp(1), "", "v0", nodectrl.Resources{})
	r.Register(fp(2), "", "v0", nodectrl.Resources{})
	got := r.RegisteredFingerprints()
	want := []identity.Fingerprint{fp(1), fp(2), fp(3)}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("fp[%d] = %s, want %s (unsorted)", i, got[i], want[i])
		}
	}
	r.Forget(fp(2))
	if got := r.RegisteredFingerprints(); len(got) != 2 || got[0] != fp(1) || got[1] != fp(3) {
		t.Errorf("after Forget(2): %v, want [fp1 fp3]", got)
	}
}

func TestNewRegistryDefaultsClock(t *testing.T) {
	r := nodectrl.NewRegistry(nil) // nil → time.Now
	got := r.Register(fp(1), "", "v0", nodectrl.Resources{})
	if got.IsZero() {
		t.Error("Register returned zero time with default clock")
	}
}
