package opensnoop

import (
	"path"
	"sort"
	"sync"

	"github.com/localai-org/darkaifactory/internal/observability/progress"
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
	// enoent is the errno openat(2) returns for a missing path. Negated in
	// the wire record (ev.Ret == -enoent), it is the cache-miss / missing-
	// dependency signal the exit hook exists to surface. ENOENT is 2 on
	// every Linux architecture.
	enoent = 2
)

// Aggregator rolls a stream of Events into a progress.Opensnoop. It is safe for one
// goroutine to Observe while another takes a Snapshot — the intended use is
// the gadget drain loop feeding it while the summary writer polls it ~1 Hz.
type Aggregator struct {
	mu      sync.Mutex
	total   uint64
	errors  uint64
	comms   *topK
	paths   *topK
	dirs    *topK
	missing *topK
}

// NewAggregator returns an empty Aggregator sized for a build-scale stream.
func NewAggregator() *Aggregator {
	return &Aggregator{
		comms:   newTopK(commTrackCap),
		paths:   newTopK(pathTrackCap),
		dirs:    newTopK(pathTrackCap),
		missing: newTopK(pathTrackCap),
	}
}

// Observe folds one openat observation into the running aggregate. A
// failed open (negative ret) bumps the error count; an ENOENT additionally
// ranks the missing path so the summary surfaces what the build can't find.
func (a *Aggregator) Observe(ev Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.total++
	a.comms.add(ev.Comm)
	a.paths.add(ev.Filename)
	a.dirs.add(dirOf(ev.Filename))
	if ev.Ret < 0 {
		a.errors++
		if ev.Ret == -enoent {
			a.missing.add(ev.Filename)
		}
	}
}

// Snapshot returns the current aggregate as a progress.Opensnoop section.
// The returned value is a copy and does not alias the Aggregator's internal
// state, so the caller may serialise it without holding any lock. The
// time-derived fields (OpenatRate, LastEventAgeMs) are left zero — this
// aggregate is clock-free; the summary writer fills them in.
func (a *Aggregator) Snapshot() progress.Opensnoop {
	a.mu.Lock()
	defer a.mu.Unlock()
	return progress.Opensnoop{
		Total:      a.total,
		Errors:     a.errors,
		TopComms:   a.comms.top(defaultTopN),
		TopPaths:   a.paths.top(defaultTopN),
		TopDirs:    a.dirs.top(defaultTopN),
		TopMissing: a.missing.top(defaultTopN),
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
func (t *topK) top(n int) []progress.Counted {
	out := make([]progress.Counted, 0, len(t.counts))
	for k, c := range t.counts {
		out = append(out, progress.Counted{Key: k, Count: c})
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
