package execsnoop

import (
	"sync"
	"testing"
)

func exec(ret int32, argv ...string) Event {
	return Event{Ret: ret, Comm: "sh", Argv: argv}
}

// TestAggregatorCountsAndRankings: total/failed counts plus the top-command
// and top-failed rankings, on a small exact (sub-cap) stream.
func TestAggregatorCountsAndRankings(t *testing.T) {
	a := NewAggregator()
	a.Observe(exec(0, "/bin/cc", "-c", "a.c"))
	a.Observe(exec(0, "/bin/cc", "-c", "b.c"))
	a.Observe(exec(0, "/bin/ld", "out"))
	a.Observe(exec(-2, "/usr/bin/missing-tool")) // ENOENT
	a.Observe(exec(-2, "/usr/bin/missing-tool")) // again

	s := a.Snapshot()
	if s.Total != 5 {
		t.Fatalf("Total = %d, want 5", s.Total)
	}
	if s.Failed != 2 {
		t.Fatalf("Failed = %d, want 2", s.Failed)
	}
	// /bin/cc exec'd twice → hottest command.
	if s.TopCommands[0].Key != "/bin/cc" || s.TopCommands[0].Count != 2 {
		t.Fatalf("TopCommands[0] = %+v, want /bin/cc=2", s.TopCommands[0])
	}
	// The missing tool is the only failure, twice.
	if len(s.TopFailed) != 1 || s.TopFailed[0].Key != "/usr/bin/missing-tool" || s.TopFailed[0].Count != 2 {
		t.Fatalf("TopFailed = %+v, want missing-tool=2", s.TopFailed)
	}
}

// TestProgramFallsBackToComm: an exec with an empty argv (pathological)
// still counts, keyed by the caller's comm rather than vanishing.
func TestProgramFallsBackToComm(t *testing.T) {
	a := NewAggregator()
	a.Observe(Event{Comm: "weird", Argv: nil})
	s := a.Snapshot()
	if s.Total != 1 || len(s.TopCommands) != 1 || s.TopCommands[0].Key != "weird" {
		t.Fatalf("empty-argv exec not bucketed under comm: %+v", s)
	}
}

// TestSnapshotEmpty: a fresh aggregator reports zero and empty (non-nil)
// rankings, so the summary serialises to empty arrays not null.
func TestSnapshotEmpty(t *testing.T) {
	s := NewAggregator().Snapshot()
	if s.Total != 0 || s.Failed != 0 {
		t.Fatalf("Total/Failed = %d/%d, want 0/0", s.Total, s.Failed)
	}
	if s.TopCommands == nil || s.TopFailed == nil {
		t.Fatalf("rankings should be empty non-nil slices: %+v", s)
	}
	if len(s.TopCommands) != 0 || len(s.TopFailed) != 0 {
		t.Fatalf("rankings should be empty: %+v", s)
	}
}

// TestAggregatorConcurrent runs Observe and Snapshot from separate goroutines
// so the race detector can prove the locking is sound.
func TestAggregatorConcurrent(t *testing.T) {
	a := NewAggregator()
	const n = 1000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range n {
			a.Observe(exec(0, "/bin/true"))
		}
	}()
	go func() {
		defer wg.Done()
		for range n {
			_ = a.Snapshot()
		}
	}()
	wg.Wait()
	if s := a.Snapshot(); s.Total != n {
		t.Fatalf("Total = %d, want %d", s.Total, n)
	}
}
