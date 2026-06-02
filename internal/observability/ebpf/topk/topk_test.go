package topk

import (
	"strconv"
	"testing"
)

// TestBoundedEviction drives the Space-Saving path: feed more distinct keys
// than the cap and confirm the tracker never exceeds the cap while a
// genuinely hot key — added far more than the churn — stays tracked with an
// accurate count.
func TestBoundedEviction(t *testing.T) {
	const limit = 4
	c := New(limit)
	// hot is added 20×; interleaved with 20 unique cold keys that thrash the
	// rest of the table.
	for i := range 20 {
		c.Add("hot")
		c.Add("cold-" + strconv.Itoa(i))
	}
	if c.Len() > limit {
		t.Fatalf("tracker holds %d keys, want ≤ %d", c.Len(), limit)
	}
	top := c.Top(limit)
	if top[0].Key != "hot" {
		t.Fatalf("hottest key = %q, want hot", top[0].Key)
	}
	if top[0].Count < 20 {
		t.Fatalf("hot count = %d, want ≥ 20 (heavy hitter must not be evicted)", top[0].Count)
	}
}

// TestExactUnderCap: while under the cap the counts are exact.
func TestExactUnderCap(t *testing.T) {
	c := New(8)
	c.Add("a")
	c.Add("a")
	c.Add("b")
	if top := c.Top(8); len(top) != 2 || top[0].Key != "a" || top[0].Count != 2 || top[1].Key != "b" {
		t.Fatalf("Top = %+v, want a=2 then b=1", top)
	}
}

// TestTieBreakAndTruncate covers the descending sort, the lexicographic
// tie-break, and the n-truncation in Top.
func TestTieBreakAndTruncate(t *testing.T) {
	c := New(8)
	c.Add("b")
	c.Add("a") // a and b tie at 1 → a sorts first (lexicographic)
	c.Add("c")
	c.Add("c") // c is hottest at 2
	top := c.Top(2)
	if len(top) != 2 {
		t.Fatalf("len = %d, want 2 (truncated)", len(top))
	}
	if top[0].Key != "c" || top[1].Key != "a" {
		t.Fatalf("ranking = %+v, want c then a", top)
	}
}

// TestEmpty: a fresh counter ranks empty (non-nil).
func TestEmpty(t *testing.T) {
	if top := New(4).Top(10); top == nil || len(top) != 0 {
		t.Fatalf("Top on empty = %+v, want empty non-nil", top)
	}
}
