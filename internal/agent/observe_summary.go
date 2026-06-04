package agent

import (
	"context"
	"io"
	"time"

	agentv1 "github.com/localai-org/darkaifactory/internal/genpb/agent/v1"
	"github.com/localai-org/darkaifactory/internal/observability/ebpf/execsnoop"
	"github.com/localai-org/darkaifactory/internal/observability/ebpf/iosnoop"
	"github.com/localai-org/darkaifactory/internal/observability/ebpf/opensnoop"
	"github.com/localai-org/darkaifactory/internal/observability/ebpf/sleepsnoop"
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

// startSummaryWriter resolves the summary path (the GuestConfig's
// observe.summary, else the daft.observe.summary cmdline token) and, when
// set, spawns a
// goroutine that rewrites the summary file every d.summary.interval until ctx
// is cancelled (a final write captures the end state). Like the gadget start
// it reports one line and never fails the boot. osAgg/exAgg are the live
// gadget aggregators the summary folds together; either may be nil (that
// gadget not running). A nil d.summary.write (unit tests / non-wired callers)
// or both aggregators nil is the "summary not requested" path.
//
// Called from run() after the gadgets start, so the summary is a view over
// whichever gadgets came up rather than coupled to any single one.
func startSummaryWriter(ctx context.Context, w io.Writer, d runDeps, cmdline string, osAgg *opensnoop.Aggregator, exAgg *execsnoop.Aggregator, slAgg *sleepsnoop.Aggregator, ioAgg *iosnoop.Aggregator) {
	if d.summary.write == nil || (osAgg == nil && exAgg == nil && slAgg == nil && ioAgg == nil) {
		return
	}
	path, ok, err := resolveObservePath(d.cfg, (*agentv1.GuestConfig_Observe).GetSummary, ParseSummaryCmdline, cmdline)
	switch {
	case err != nil:
		writeReport(w, SummaryReportPrefix, "cmdline: "+err.Error())
		return
	case !ok:
		return
	}
	writeReport(w, SummaryReportPrefix, "writing → "+path)
	go runSummaryLoop(ctx, d.summary, path, osAgg, exAgg, slAgg, ioAgg)
}

// rateTracker turns a gadget's clock-free cumulative total into the
// time-derived summary fields: the per-window event rate and how long ago the
// total last advanced (the stuck signal). One tracker per gadget section, so
// each gadget's activity age is independent.
type rateTracker struct {
	lastTotal uint64    // total at the last advance
	lastAt    time.Time // when total last advanced; zero ⇒ no events yet
	prevTotal uint64    // total at the previous sample (rate numerator base)
	prevAt    time.Time // time of the previous sample (rate denominator base)
}

func newRateTracker(start time.Time) *rateTracker {
	return &rateTracker{prevAt: start}
}

// observe folds the cumulative total at time now and returns the rate
// (events/sec over the last write window) and the age in ms since the total
// last advanced (-1 if it never has).
func (r *rateTracker) observe(total uint64, now time.Time) (rate float64, lastAgeMs int64) {
	if total > r.lastTotal {
		r.lastTotal, r.lastAt = total, now
	}
	lastAgeMs = -1 // sentinel: no event observed yet
	if !r.lastAt.IsZero() {
		lastAgeMs = now.Sub(r.lastAt).Milliseconds()
	}
	if dt := now.Sub(r.prevAt).Seconds(); dt > 0 {
		rate = float64(total-r.prevTotal) / dt
	}
	r.prevTotal, r.prevAt = total, now
	return rate, lastAgeMs
}

// runSummaryLoop rewrites the summary every d.interval until ctx is
// cancelled, then writes one final snapshot so the on-disk file reflects the
// end state. Write errors are dropped: the summary is a best-effort
// debugging surface and the host reader tolerates a momentarily stale file.
func runSummaryLoop(ctx context.Context, d summaryDeps, path string, osAgg *opensnoop.Aggregator, exAgg *execsnoop.Aggregator, slAgg *sleepsnoop.Aggregator, ioAgg *iosnoop.Aggregator) {
	start := d.now()
	osRate := newRateTracker(start)
	exRate := newRateTracker(start)
	slRate := newRateTracker(start)
	ioRate := newRateTracker(start)
	// build folds each present gadget's clock-free snapshot into one summary,
	// filling its time-derived fields from a single clock read so the
	// sections and the file timestamp share one instant.
	build := func() progress.Summary {
		now := d.now()
		s := progress.Summary{TakenAtUnixMs: now.UnixMilli()}
		if osAgg != nil {
			op := osAgg.Snapshot()
			op.OpenatRate, op.LastEventAgeMs = osRate.observe(op.Total, now)
			s.Opensnoop = &op
		}
		if exAgg != nil {
			ex := exAgg.Snapshot()
			ex.ExecRate, ex.LastEventAgeMs = exRate.observe(ex.Total, now)
			s.ExecSnoop = &ex
		}
		if slAgg != nil {
			sl := slAgg.Snapshot()
			sl.SleepRate, sl.LastEventAgeMs = slRate.observe(sl.Total, now)
			s.SleepSnoop = &sl
		}
		if ioAgg != nil {
			iosn := ioAgg.Snapshot()
			iosn.IORate, iosn.LastEventAgeMs = ioRate.observe(iosn.Total, now)
			s.IOSnoop = &iosn
		}
		return s
	}
	// Write once immediately so the file exists for an early poller (and so
	// the host sees a valid summary even if the VM is torn down before the
	// first tick fires) rather than file-not-found.
	_ = d.write(path, build())
	t := time.NewTicker(d.interval)
	defer t.Stop()
	//daft:lint:disable=forloopbound reason="rewrites the summary once per tick for the agent's lifetime; ctx cancel is the termination signal (writes a final snapshot and returns)"
	for {
		select {
		case <-ctx.Done():
			_ = d.write(path, build())
			return
		case <-t.C:
			_ = d.write(path, build())
		}
	}
}
