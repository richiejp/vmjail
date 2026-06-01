package agent

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/observability/ebpf/opensnoop"
	"github.com/localai-org/darkaifactory/internal/observability/progress"
)

// fakeClock is a deterministic clock for the time-derived summary fields.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TestProgressTrackerSample walks the three states the time-derived fields
// must distinguish: no activity yet, a fresh advance, and a stall.
func TestProgressTrackerSample(t *testing.T) {
	clock := newFakeClock()
	tr := newProgressTracker(clock.now)

	// No openat observed yet: age is the -1 sentinel, rate 0.
	s := tr.sample(progress.Opensnoop{Total: 0})
	if s.Opensnoop == nil || s.Opensnoop.LastEventAgeMs != -1 || s.Opensnoop.OpenatRate != 0 {
		t.Fatalf("initial sample = %+v, want age -1 rate 0", s.Opensnoop)
	}
	if s.TakenAtUnixMs != clock.now().UnixMilli() {
		t.Fatalf("TakenAtUnixMs = %d, want %d", s.TakenAtUnixMs, clock.now().UnixMilli())
	}

	// 10 opens over the last 1s: rate 10/s, age 0 (just advanced).
	clock.advance(time.Second)
	s = tr.sample(progress.Opensnoop{Total: 10})
	if s.Opensnoop.OpenatRate != 10 || s.Opensnoop.LastEventAgeMs != 0 {
		t.Fatalf("advance sample = %+v, want rate 10 age 0", s.Opensnoop)
	}

	// Stalled: Total frozen for 2s → rate 0, age 2000ms.
	clock.advance(2 * time.Second)
	s = tr.sample(progress.Opensnoop{Total: 10})
	if s.Opensnoop.OpenatRate != 0 || s.Opensnoop.LastEventAgeMs != 2000 {
		t.Fatalf("stall sample = %+v, want rate 0 age 2000", s.Opensnoop)
	}
}

// TestStartSummaryWriterDisabled: a nil write disables the writer — no
// console line, no goroutine.
func TestStartSummaryWriterDisabled(t *testing.T) {
	var console bytes.Buffer
	startSummaryWriter(context.Background(), &console,
		runDeps{summary: summaryDeps{write: nil}},
		"daft.observe.summary=/share/p.json", opensnoop.NewAggregator())
	if console.Len() != 0 {
		t.Fatalf("unexpected console output: %q", console.String())
	}
}

// TestStartSummaryWriterAbsentToken: writer wired but no cmdline token →
// silent skip.
func TestStartSummaryWriterAbsentToken(t *testing.T) {
	wrote := false
	d := runDeps{summary: summaryDeps{
		now: time.Now, interval: time.Millisecond,
		write: func(string, progress.Summary) error { wrote = true; return nil },
	}}
	var console bytes.Buffer
	startSummaryWriter(context.Background(), &console, d, "console=hvc0", opensnoop.NewAggregator())
	time.Sleep(5 * time.Millisecond) // a started loop would have written by now
	if wrote || console.Len() != 0 {
		t.Fatalf("summary started despite absent token: wrote=%v console=%q", wrote, console.String())
	}
}

// TestStartSummaryWriterReportsCmdlineError: a malformed (relative) path is
// reported and no loop starts.
func TestStartSummaryWriterReportsCmdlineError(t *testing.T) {
	d := runDeps{summary: summaryDeps{
		now: time.Now, interval: time.Millisecond,
		write: func(string, progress.Summary) error { return nil },
	}}
	var console bytes.Buffer
	startSummaryWriter(context.Background(), &console, d, "daft.observe.summary=rel/path", opensnoop.NewAggregator())
	if !bytes.Contains(console.Bytes(), []byte(SummaryReportPrefix+"cmdline:")) {
		t.Fatalf("missing cmdline error report: %q", console.String())
	}
}

// TestStartSummaryWriterHappyPath: the writer reports the path, ticks the
// summary from the live aggregator, and writes a final snapshot on cancel.
func TestStartSummaryWriterHappyPath(t *testing.T) {
	clock := newFakeClock()
	writes := make(chan progress.Summary, 16)
	d := runDeps{summary: summaryDeps{
		now:      clock.now,
		interval: time.Millisecond,
		write: func(path string, s progress.Summary) error {
			if path != testSummaryPath {
				t.Errorf("write path = %q, want %q", path, testSummaryPath)
			}
			select {
			case writes <- s:
			default:
			}
			return nil
		},
	}}
	agg := opensnoop.NewAggregator()
	agg.Observe(opensnoop.Event{Comm: "cc1", Filename: "/nix/store/abc/x.h"})

	ctx, cancel := context.WithCancel(context.Background())
	var console bytes.Buffer
	startSummaryWriter(ctx, &console, d, "daft.observe.summary="+testSummaryPath, agg)

	got := <-writes // the immediate initial write
	cancel()
	if got.Opensnoop == nil || got.Opensnoop.Total != 1 || got.Opensnoop.TopPaths[0].Key != "/nix/store/abc/x.h" {
		t.Fatalf("summary did not reflect the aggregator: %+v", got.Opensnoop)
	}
	if !bytes.Contains(console.Bytes(), []byte(SummaryReportPrefix+"writing → "+testSummaryPath)) {
		t.Fatalf("missing writing report: %q", console.String())
	}
}

// TestDefaultSummaryDeps covers the production wiring.
func TestDefaultSummaryDeps(t *testing.T) {
	d := defaultSummaryDeps()
	if d.now == nil || d.write == nil || d.interval <= 0 {
		t.Fatalf("defaultSummaryDeps incomplete: %+v", d)
	}
}
