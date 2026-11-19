package sleepsnoop

import (
	"sync"

	"github.com/richiejp/vmjail/internal/observability/ebpf/topk"
	"github.com/richiejp/vmjail/internal/observability/progress"
)

// Aggregation caps. A build sleeps from a bounded set of distinct comms; the
// rankings are bounded approximate heavy-hitter counters (see topk) sized
// comfortably above the surfaced top-N.
const (
	commTrackCap = 256
	defaultTopN  = 10
	nsPerMs      = 1_000_000
)

// Aggregator rolls a stream of Events into a progress.SleepSnoop. It is safe
// for one goroutine to Observe while another takes a Snapshot — the intended
// use is the gadget drain loop feeding it while the summary writer polls it
// ~1 Hz.
type Aggregator struct {
	mu         sync.Mutex
	total      uint64
	totalReqNs uint64
	callers    *topk.Counter // comm → sleep-call count
	sleepers   *topk.Counter // comm → cumulative requested sleep (ms)
}

// NewAggregator returns an empty Aggregator sized for a build-scale stream.
func NewAggregator() *Aggregator {
	return &Aggregator{
		callers:  topk.New(commTrackCap),
		sleepers: topk.New(commTrackCap),
	}
}

// Observe folds one sleep observation into the running aggregate. The comm is
// the ranking key: it ranks once by call count (catching tight poll loops of
// many short sleeps) and once weighted by the requested duration in ms
// (catching whoever gives up the most wall-clock). A sub-millisecond sleep
// contributes 0 ms, so it counts toward TopCallers but not TopSleepers.
func (a *Aggregator) Observe(ev Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.total++
	a.totalReqNs += ev.ReqNs
	a.callers.Add(ev.Comm)
	a.sleepers.AddN(ev.Comm, ev.ReqNs/nsPerMs)
}

// Snapshot returns the current aggregate as a progress.SleepSnoop section. The
// returned value is a copy and does not alias the Aggregator's internal state.
// The time-derived fields (SleepRate, LastEventAgeMs) are left zero — this
// aggregate is clock-free; the summary writer fills them in.
func (a *Aggregator) Snapshot() progress.SleepSnoop {
	a.mu.Lock()
	defer a.mu.Unlock()
	return progress.SleepSnoop{
		Total:        a.total,
		TotalSleepMs: a.totalReqNs / nsPerMs,
		TopCallers:   a.callers.Top(defaultTopN),
		TopSleepers:  a.sleepers.Top(defaultTopN),
	}
}
