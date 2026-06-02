package opensnoop

import (
	"strconv"
	"sync"
	"testing"
)

// ev is a terse Event constructor for the aggregator tests — only the
// fields the aggregator reads (Comm, Filename) matter here. Ret defaults to
// 0 (a successful open); evRet sets it for the error-path tests.
func ev(comm, filename string) Event {
	return Event{Comm: comm, Filename: filename}
}

func evRet(comm, filename string, ret int32) Event {
	return Event{Comm: comm, Filename: filename, Ret: ret}
}

// TestAggregatorErrorsAndMissing covers the exit-hook payoff: a negative
// return bumps Errors, and an ENOENT additionally ranks the missing path,
// while a non-ENOENT failure counts as an error but is not a "missing" path.
func TestAggregatorErrorsAndMissing(t *testing.T) {
	a := NewAggregator()
	a.Observe(evRet("cc1", "/usr/include/zlib.h", 5))  // success (fd)
	a.Observe(evRet("cc1", "/nix/store/miss/a.h", -2)) // ENOENT
	a.Observe(evRet("cc1", "/nix/store/miss/a.h", -2)) // ENOENT again
	a.Observe(evRet("cc1", "/nix/store/miss/b.h", -2)) // ENOENT
	a.Observe(evRet("sh", "/root/secret", -13))        // EACCES — error, not missing

	s := a.Snapshot()
	if s.Total != 5 {
		t.Fatalf("Total = %d, want 5", s.Total)
	}
	if s.Errors != 4 {
		t.Fatalf("Errors = %d, want 4 (3 ENOENT + 1 EACCES)", s.Errors)
	}
	// a.h missed twice → hottest missing path; the EACCES path is absent.
	if len(s.TopMissing) != 2 {
		t.Fatalf("TopMissing = %+v, want 2 distinct ENOENT paths", s.TopMissing)
	}
	if s.TopMissing[0].Key != "/nix/store/miss/a.h" || s.TopMissing[0].Count != 2 {
		t.Fatalf("TopMissing[0] = %+v, want a.h=2", s.TopMissing[0])
	}
	for _, m := range s.TopMissing {
		if m.Key == "/root/secret" {
			t.Errorf("EACCES path leaked into TopMissing: %+v", s.TopMissing)
		}
	}
}

// TestAggregatorCountsAndTopN exercises the exact total alongside the
// per-process and per-path/dir rankings on a small, sub-cap stream where the
// heavy-hitter counter is exact.
func TestAggregatorCountsAndTopN(t *testing.T) {
	a := NewAggregator()
	for range 3 {
		a.Observe(ev("cc1", "/nix/store/abc/lib/x.h"))
	}
	a.Observe(ev("cc1", "/nix/store/abc/lib/y.h"))
	a.Observe(ev("ld", "/nix/store/def/bin/ld"))

	s := a.Snapshot()
	if s.Total != 5 {
		t.Fatalf("Total = %d, want 5", s.Total)
	}
	// cc1 opened four files, ld one.
	if len(s.TopComms) != 2 || s.TopComms[0].Key != "cc1" || s.TopComms[0].Count != 4 {
		t.Fatalf("TopComms = %+v, want cc1=4 first", s.TopComms)
	}
	// The hottest single path is x.h (3 opens).
	if s.TopPaths[0].Key != "/nix/store/abc/lib/x.h" || s.TopPaths[0].Count != 3 {
		t.Fatalf("TopPaths[0] = %+v, want x.h=3", s.TopPaths[0])
	}
	// x.h and y.h share a directory → that dir is hottest with 4.
	if s.TopDirs[0].Key != "/nix/store/abc/lib" || s.TopDirs[0].Count != 4 {
		t.Fatalf("TopDirs[0] = %+v, want /nix/store/abc/lib=4", s.TopDirs[0])
	}
}

// TestSnapshotEmpty: a fresh aggregator reports zero and empty (non-nil)
// rankings, so the summary serialises to empty arrays not null.
func TestSnapshotEmpty(t *testing.T) {
	s := NewAggregator().Snapshot()
	if s.Total != 0 || s.Errors != 0 {
		t.Fatalf("Total/Errors = %d/%d, want 0/0", s.Total, s.Errors)
	}
	if s.TopComms == nil || s.TopPaths == nil || s.TopDirs == nil || s.TopMissing == nil {
		t.Fatalf("rankings should be empty non-nil slices: %+v", s)
	}
	if len(s.TopComms) != 0 || len(s.TopPaths) != 0 || len(s.TopDirs) != 0 || len(s.TopMissing) != 0 {
		t.Fatalf("rankings should be empty: %+v", s)
	}
}

// TestTopKBoundedEviction drives the Space-Saving path: feed more distinct
// keys than the cap and confirm the tracker never exceeds the cap while a
// genuinely hot key — opened far more than the churn — stays tracked with an
// accurate count.
func TestTopKBoundedEviction(t *testing.T) {
	const limit = 4
	tk := newTopK(limit)
	// hot is opened 20×; interleaved with 20 unique cold keys that thrash
	// the rest of the table.
	for i := range 20 {
		tk.add("hot")
		tk.add("cold-" + strconv.Itoa(i))
	}
	if len(tk.counts) > limit {
		t.Fatalf("tracker holds %d keys, want ≤ %d", len(tk.counts), limit)
	}
	top := tk.top(limit)
	if top[0].Key != "hot" {
		t.Fatalf("hottest key = %q, want hot", top[0].Key)
	}
	if top[0].Count < 20 {
		t.Fatalf("hot count = %d, want ≥ 20 (heavy hitter must not be evicted)", top[0].Count)
	}
}

// TestTopKTieBreakAndTruncate covers the descending sort, the lexicographic
// tie-break, and the n-truncation in top().
func TestTopKTieBreakAndTruncate(t *testing.T) {
	tk := newTopK(8)
	tk.add("b")
	tk.add("a") // a and b tie at 1 → a sorts first (lexicographic)
	tk.add("c")
	tk.add("c") // c is hottest at 2
	top := tk.top(2)
	if len(top) != 2 {
		t.Fatalf("len = %d, want 2 (truncated)", len(top))
	}
	if top[0].Key != "c" || top[1].Key != "a" {
		t.Fatalf("ranking = %+v, want c then a", top)
	}
}

// TestDirOf covers the path-to-directory bucketing, including the empty /
// relative dirfd-relative case that collapses to ".".
func TestDirOf(t *testing.T) {
	cases := map[string]string{
		"/nix/store/abc/lib/x.h": "/nix/store/abc/lib",
		"relative.txt":           ".",
		"":                       ".",
		"/etc/passwd":            "/etc",
	}
	for in, want := range cases {
		if got := dirOf(in); got != want {
			t.Fatalf("dirOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAggregatorConcurrent runs Observe and Snapshot from separate
// goroutines so the race detector (make check) can prove the locking is
// sound; it also confirms no observation is lost.
func TestAggregatorConcurrent(t *testing.T) {
	a := NewAggregator()
	const n = 1000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range n {
			a.Observe(ev("worker", "/tmp/f"))
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
