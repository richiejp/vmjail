package execsnoop

import (
	"sync"

	"github.com/richiejp/darkaifactory/internal/observability/ebpf/topk"
	"github.com/richiejp/darkaifactory/internal/observability/progress"
)

// Aggregation caps. A build execs thousands of distinct commands; the
// rankings are bounded approximate heavy-hitter counters (see topk) sized
// comfortably above the surfaced top-N so the reported hot keys are accurate.
const (
	cmdTrackCap = 256
	defaultTopN = 10
)

// Aggregator rolls a stream of Events into a progress.ExecSnoop. It is safe
// for one goroutine to Observe while another takes a Snapshot — the intended
// use is the gadget drain loop feeding it while the summary writer polls it
// ~1 Hz.
type Aggregator struct {
	mu       sync.Mutex
	total    uint64
	failed   uint64
	commands *topk.Counter
	failures *topk.Counter
}

// NewAggregator returns an empty Aggregator sized for a build-scale stream.
func NewAggregator() *Aggregator {
	return &Aggregator{
		commands: topk.New(cmdTrackCap),
		failures: topk.New(cmdTrackCap),
	}
}

// Observe folds one execve observation into the running aggregate. The
// program path (argv[0]) is the ranking key; a failed exec (non-zero ret)
// additionally ranks under failures so the summary surfaces what could not
// be run.
func (a *Aggregator) Observe(ev Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.total++
	prog := program(ev)
	a.commands.Add(prog)
	if ev.Ret != 0 {
		a.failed++
		a.failures.Add(prog)
	}
}

// Snapshot returns the current aggregate as a progress.ExecSnoop section.
// The returned value is a copy and does not alias the Aggregator's internal
// state. The time-derived fields (ExecRate, LastEventAgeMs) are left zero —
// this aggregate is clock-free; the summary writer fills them in.
func (a *Aggregator) Snapshot() progress.ExecSnoop {
	a.mu.Lock()
	defer a.mu.Unlock()
	return progress.ExecSnoop{
		Total:       a.total,
		Failed:      a.failed,
		TopCommands: a.commands.Top(defaultTopN),
		TopFailed:   a.failures.Top(defaultTopN),
	}
}

// program is the ranking key for an exec: the program path (argv[0]). An
// exec with an empty argv (pathological) collapses to the caller's comm so
// it still counts toward a bucket rather than vanishing.
func program(ev Event) string {
	if len(ev.Argv) > 0 && ev.Argv[0] != "" {
		return ev.Argv[0]
	}
	return ev.Comm
}
