package opensnoop

import (
	"path"
	"sync"

	"github.com/localai-org/darkaifactory/internal/observability/ebpf/topk"
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
	comms   *topk.Counter
	paths   *topk.Counter
	dirs    *topk.Counter
	missing *topk.Counter
}

// NewAggregator returns an empty Aggregator sized for a build-scale stream.
func NewAggregator() *Aggregator {
	return &Aggregator{
		comms:   topk.New(commTrackCap),
		paths:   topk.New(pathTrackCap),
		dirs:    topk.New(pathTrackCap),
		missing: topk.New(pathTrackCap),
	}
}

// Observe folds one openat observation into the running aggregate. A
// failed open (negative ret) bumps the error count; an ENOENT additionally
// ranks the missing path so the summary surfaces what the build can't find.
func (a *Aggregator) Observe(ev Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.total++
	a.comms.Add(ev.Comm)
	a.paths.Add(ev.Filename)
	a.dirs.Add(dirOf(ev.Filename))
	if ev.Ret < 0 {
		a.errors++
		if ev.Ret == -enoent {
			a.missing.Add(ev.Filename)
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
		TopComms:   a.comms.Top(defaultTopN),
		TopPaths:   a.paths.Top(defaultTopN),
		TopDirs:    a.dirs.Top(defaultTopN),
		TopMissing: a.missing.Top(defaultTopN),
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
