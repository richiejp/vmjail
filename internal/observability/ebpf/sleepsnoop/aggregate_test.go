package sleepsnoop

import (
	"sync"
	"testing"
	"time"
)

func sleepEv(comm string, d time.Duration) Event {
	return Event{Comm: comm, ReqNs: uint64(d)} //nolint:gosec // G115: test durations are small and positive.
}

// TestAggregatorCountsAndRankings: total + cumulative ms, plus the call-count
// and duration rankings ordered differently on purpose — "poller" calls sleep
// most often (tight loop of short sleeps) while "waiter" sleeps the longest in
// aggregate.
func TestAggregatorCountsAndRankings(t *testing.T) {
	a := NewAggregator()
	for range 5 {
		a.Observe(sleepEv("poller", 10*time.Millisecond)) // 5 × 10ms = 50ms
	}
	a.Observe(sleepEv("waiter", 500*time.Millisecond)) // 1 × 500ms
	a.Observe(sleepEv("waiter", 500*time.Millisecond)) // total 1000ms

	s := a.Snapshot()
	if s.Total != 7 {
		t.Fatalf("Total = %d, want 7", s.Total)
	}
	if s.TotalSleepMs != 50+1000 {
		t.Fatalf("TotalSleepMs = %d, want 1050", s.TotalSleepMs)
	}
	// poller called sleep most often → hottest caller.
	if s.TopCallers[0].Key != "poller" || s.TopCallers[0].Count != 5 {
		t.Fatalf("TopCallers[0] = %+v, want poller=5", s.TopCallers[0])
	}
	// waiter slept the most wall-clock → hottest sleeper (count is ms here).
	if s.TopSleepers[0].Key != "waiter" || s.TopSleepers[0].Count != 1000 {
		t.Fatalf("TopSleepers[0] = %+v, want waiter=1000ms", s.TopSleepers[0])
	}
}

// TestSubMillisecondSleepCountsButNotInSleepers: a sub-ms sleep still bumps the
// call count, but contributes 0 ms so it never enters the duration ranking.
func TestSubMillisecondSleepCountsButNotInSleepers(t *testing.T) {
	a := NewAggregator()
	a.Observe(sleepEv("tight", 100*time.Microsecond)) // 0 ms
	s := a.Snapshot()
	if s.Total != 1 || s.TotalSleepMs != 0 {
		t.Fatalf("Total/TotalSleepMs = %d/%d, want 1/0", s.Total, s.TotalSleepMs)
	}
	if len(s.TopCallers) != 1 || s.TopCallers[0].Key != "tight" {
		t.Fatalf("sub-ms sleep not counted as a caller: %+v", s.TopCallers)
	}
	if len(s.TopSleepers) != 0 {
		t.Fatalf("sub-ms sleep should not enter the duration ranking: %+v", s.TopSleepers)
	}
}

// TestSnapshotEmpty: a fresh aggregator reports zero and empty (non-nil)
// rankings, so the summary serialises to empty arrays not null.
func TestSnapshotEmpty(t *testing.T) {
	s := NewAggregator().Snapshot()
	if s.Total != 0 || s.TotalSleepMs != 0 {
		t.Fatalf("Total/TotalSleepMs = %d/%d, want 0/0", s.Total, s.TotalSleepMs)
	}
	if s.TopCallers == nil || s.TopSleepers == nil {
		t.Fatalf("rankings should be empty non-nil slices: %+v", s)
	}
	if len(s.TopCallers) != 0 || len(s.TopSleepers) != 0 {
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
			a.Observe(sleepEv("x", time.Millisecond))
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
