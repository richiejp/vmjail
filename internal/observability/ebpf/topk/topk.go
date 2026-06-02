// Package topk is a bounded approximate heavy-hitters counter (Space-Saving)
// shared by the Tier-2 eBPF gadget aggregators. A single `nix build` opens
// hundreds of thousands of distinct files and execs thousands of commands,
// so a gadget cannot keep an exact per-key map without unbounded guest
// memory; this tracks at most cap keys in O(cap) memory while keeping the
// true heavy hitters — the only keys a summary surfaces — accurate.
package topk

import (
	"sort"

	"github.com/localai-org/darkaifactory/internal/observability/progress"
)

// Counter tracks at most cap keys. While under cap it counts exactly; once
// full, a new key evicts the current minimum and inherits its count+1, so a
// key that turns hot climbs out of the noise floor instead of being evicted
// at 1 on every appearance. Not safe for concurrent use — callers (the
// gadget aggregators) hold their own lock around Add/Top.
type Counter struct {
	cap    int
	counts map[string]uint64
}

// New returns an empty Counter holding at most capacity keys.
func New(capacity int) *Counter {
	return &Counter{cap: capacity, counts: make(map[string]uint64, capacity)}
}

// Add folds one observation of key into the counter.
func (c *Counter) Add(key string) {
	if n, ok := c.counts[key]; ok {
		c.counts[key] = n + 1
		return
	}
	if len(c.counts) < c.cap {
		c.counts[key] = 1
		return
	}
	// Full: evict the current minimum; the newcomer inherits its count+1.
	// Map iteration order is random, but the minimum it finds — and thus the
	// resulting state — is deterministic regardless of visitation order.
	var minKey string
	var minCount uint64
	first := true
	for k, n := range c.counts {
		if first || n < minCount {
			minKey, minCount, first = k, n, false
		}
	}
	delete(c.counts, minKey)
	c.counts[key] = minCount + 1
}

// Top returns the n highest-count keys, descending, with a lexicographic
// tie-break so the ranking is deterministic for a stable summary file.
func (c *Counter) Top(n int) []progress.Counted {
	out := make([]progress.Counted, 0, len(c.counts))
	for k, count := range c.counts {
		out = append(out, progress.Counted{Key: k, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	if n < len(out) {
		out = out[:n]
	}
	return out
}

// Len reports how many distinct keys are currently tracked (≤ cap). Used by
// tests asserting the bound holds.
func (c *Counter) Len() int { return len(c.counts) }
