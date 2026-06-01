package agent

import (
	"context"
	"io"
	"time"

	"github.com/localai-org/darkaifactory/internal/observability/ebpf/opensnoop"
	"github.com/localai-org/darkaifactory/internal/observability/progress"
)

// SummaryReportPrefix labels the build-progress summary writer's console
// status line, mirroring the other per-channel report prefixes.
const SummaryReportPrefix = "observe-summary: "

// defaultSummaryInterval is how often the summary file is rewritten — ~1 Hz:
// frequent enough to watch a build advance, cheap enough to ignore.
const defaultSummaryInterval = time.Second

// summaryDeps is the test-substitutable surface for the build-progress
// summary writer: the clock that drives the time-derived fields, the rewrite
// cadence, and the atomic file write. A zero value (write == nil) disables
// the writer, so unit tests of runWith that don't wire it skip the loop.
type summaryDeps struct {
	now      func() time.Time
	interval time.Duration
	write    func(path string, s progress.Summary) error
}

// defaultSummaryDeps wires the real clock + ~1 Hz cadence + atomic write.
func defaultSummaryDeps() summaryDeps {
	return summaryDeps{
		now:      time.Now,
		interval: defaultSummaryInterval,
		write:    progress.WriteAtomic,
	}
}

// startSummaryWriter parses daft.observe.summary and, when set, spawns a
// goroutine that rewrites the summary file from agg every d.summary.interval
// until ctx is cancelled (a final write captures the end state). Like the
// gadget start it reports one line and never fails the boot. agg is the live
// aggregator the open-snoop drain feeds; a nil d.summary.write (unit tests /
// non-wired callers) is the "summary not requested" path.
//
// Called from startOpensnoopFromCmdline with that gadget's aggregator: the
// summary is a view over the gadget, so it shares its lifetime and is only
// reachable when the gadget itself is running.
func startSummaryWriter(ctx context.Context, w io.Writer, d runDeps, cmdline string, agg *opensnoop.Aggregator) {
	if d.summary.write == nil {
		return
	}
	path, ok, err := ParseSummaryCmdline(cmdline)
	switch {
	case err != nil:
		writeReport(w, SummaryReportPrefix, "cmdline: "+err.Error())
		return
	case !ok:
		return
	}
	writeReport(w, SummaryReportPrefix, "writing → "+path)
	go runSummaryLoop(ctx, d.summary, path, agg)
}

// progressTracker turns the clock-free aggregate into the time-derived
// fields the summary carries: the openat rate over the last write window and
// how long ago openat activity last advanced (the stuck signal).
type progressTracker struct {
	now       func() time.Time
	lastTotal uint64    // Total at the last advance
	lastAt    time.Time // when Total last advanced; zero ⇒ no events yet
	prevTotal uint64    // Total at the previous sample (rate numerator base)
	prevAt    time.Time // time of the previous sample (rate denominator base)
}

func newProgressTracker(now func() time.Time) *progressTracker {
	return &progressTracker{now: now, prevAt: now()}
}

// sample folds the latest aggregate into a full progress.Summary and
// advances the tracker's rolling state. op arrives with its time-derived
// fields zero (the aggregator is clock-free); this fills them in.
func (p *progressTracker) sample(op progress.Opensnoop) progress.Summary {
	now := p.now()
	if op.Total > p.lastTotal {
		p.lastTotal, p.lastAt = op.Total, now
	}
	op.LastEventAgeMs = -1 // sentinel: no openat observed yet
	if !p.lastAt.IsZero() {
		op.LastEventAgeMs = now.Sub(p.lastAt).Milliseconds()
	}
	if dt := now.Sub(p.prevAt).Seconds(); dt > 0 {
		op.OpenatRate = float64(op.Total-p.prevTotal) / dt
	}
	p.prevTotal, p.prevAt = op.Total, now
	return progress.Summary{TakenAtUnixMs: now.UnixMilli(), Opensnoop: &op}
}

// runSummaryLoop rewrites the summary every d.interval until ctx is
// cancelled, then writes one final snapshot so the on-disk file reflects the
// end state. Write errors are dropped: the summary is a best-effort
// debugging surface and the host reader tolerates a momentarily stale file.
func runSummaryLoop(ctx context.Context, d summaryDeps, path string, agg *opensnoop.Aggregator) {
	tracker := newProgressTracker(d.now)
	// Write once immediately so the file exists for an early poller (and so
	// the host sees a valid summary even if the VM is torn down before the
	// first tick fires) rather than file-not-found.
	_ = d.write(path, tracker.sample(agg.Snapshot()))
	t := time.NewTicker(d.interval)
	defer t.Stop()
	//daft:lint:disable=forloopbound reason="rewrites the summary once per tick for the agent's lifetime; ctx cancel is the termination signal (writes a final snapshot and returns)"
	for {
		select {
		case <-ctx.Done():
			_ = d.write(path, tracker.sample(agg.Snapshot()))
			return
		case <-t.C:
			_ = d.write(path, tracker.sample(agg.Snapshot()))
		}
	}
}
