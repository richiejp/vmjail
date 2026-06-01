//go:build linux

package opensnoop

import (
	"path"
	"sort"
	"sync"
)

// Aggregation caps. A single `nix build` opens hundreds of thousands of
// distinct files, so the per-path/per-dir trackers cannot keep every key
// without unbounded guest memory. They are bounded approximate heavy-hitter
// counters (see topK); the caps sit comfortably above the surfaced top-N so
// the reported hot keys are accurate. Process names (comm) are low
// cardinality but bounded the same way for uniformity and safety.
const (
	pathTrackCap = 256
	commTrackCap = 128
	defaultTopN  = 10
)

// CountedKey is one ranked entry — a key and its observed count. For paths
// and dirs the count is a heavy-hitter estimate (an upper bound on the true
// frequency); for the exact Total see Summary.Total.
type CountedKey struct {
	Key   string `json:"key"`
	Count uint64 `json:"count"`
}

// Summary is a point-in-time aggregate of openat(2) activity, serialised
// into the per-sandbox build-progress summary file. Total is exact and is
// the primary liveness signal (a build that has stopped opening files has a
// frozen Total); TopComms/TopPaths/TopDirs are bounded heavy-hitter
// rankings that hint at *what* the build is churning on so a viewer can drill
// down into the raw opensnoop.log. Time-derived signals — last-activity age
// and per-window rates — are layered on by the summary writer (roadmap #6b),
// which owns the wall-clock; this aggregate is clock-free and pure.
type Summary struct {
	Total    uint64       `json:"total"`
	TopComms []CountedKey `json:"top_comms"`
	TopPaths []CountedKey `json:"top_paths"`
	TopDirs  []CountedKey `json:"top_dirs"`
}

// Aggregator rolls a stream of Events into a Summary. It is safe for one
// goroutine to Observe while another takes a Snapshot — the intended use is
// the gadget drain loop feeding it while the summary writer polls it ~1 Hz.
type Aggregator struct {
	mu    sync.Mutex
	total uint64
	comms *topK
	paths *topK
	dirs  *topK
}

// NewAggregator returns an empty Aggregator sized for a build-scale stream.
func NewAggregator() *Aggregator {
	return &Aggregator{
		comms: newTopK(commTrackCap),
		paths: newTopK(pathTrackCap),
		dirs:  newTopK(pathTrackCap),
	}
}

// Observe folds one openat observation into the running aggregate.
func (a *Aggregator) Observe(ev Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.total++
	a.comms.add(ev.Comm)
	a.paths.add(ev.Filename)
	a.dirs.add(dirOf(ev.Filename))
}

// Snapshot returns the current aggregate. The returned Summary is a copy and
// does not alias the Aggregator's internal state, so the caller may serialise
// it without holding any lock.
func (a *Aggregator) Snapshot() Summary {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Summary{
		Total:    a.total,
		TopComms: a.comms.top(defaultTopN),
		TopPaths: a.paths.top(defaultTopN),
		TopDirs:  a.dirs.top(defaultTopN),
	}
}

// dirOf is the directory a path lives in, used to surface hot directories
// (e.g. a single /nix/store/<hash> output) rather than scattering the signal
// across thousands of individual files. An empty or relative filename — an
// openat against a dirfd — collapses to "." which is itself a useful bucket.
func dirOf(filename string) string {
	if filename == "" {
		return "."
	}
	return path.Dir(filename)
}

// topK is a bounded approximate heavy-hitters counter (Space-Saving): it
// tracks at most cap keys. While under cap it counts exactly; once full, a
// new key evicts the current minimum and inherits its count+1, so a key that
// turns hot climbs out of the noise floor instead of being evicted at 1 on
// every appearance. Memory is O(cap) regardless of stream cardinality, and
// the true heavy hitters — the only keys we surface — stay accurate.
type topK struct {
	cap    int
	counts map[string]uint64
}

func newTopK(capacity int) *topK {
	return &topK{cap: capacity, counts: make(map[string]uint64, capacity)}
}

func (t *topK) add(key string) {
	if c, ok := t.counts[key]; ok {
		t.counts[key] = c + 1
		return
	}
	if len(t.counts) < t.cap {
		t.counts[key] = 1
		return
	}
	// Full: evict the current minimum; the newcomer inherits its count+1.
	// Map iteration order is random, but the minimum it finds — and thus the
	// resulting state — is deterministic regardless of visitation order.
	var minKey string
	var minCount uint64
	first := true
	for k, c := range t.counts {
		if first || c < minCount {
			minKey, minCount, first = k, c, false
		}
	}
	delete(t.counts, minKey)
	t.counts[key] = minCount + 1
}

// top returns the n highest-count keys, descending, with a lexicographic
// tie-break so the ranking is deterministic for a stable summary file.
func (t *topK) top(n int) []CountedKey {
	out := make([]CountedKey, 0, len(t.counts))
	for k, c := range t.counts {
		out = append(out, CountedKey{Key: k, Count: c})
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
