package agent

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
	"github.com/richiejp/vmjail/internal/observability/ebpf/execsnoop"
	"github.com/richiejp/vmjail/internal/observability/ebpf/iosnoop"
	"github.com/richiejp/vmjail/internal/observability/ebpf/opensnoop"
	"github.com/richiejp/vmjail/internal/observability/ebpf/sleepsnoop"
	"github.com/richiejp/vmjail/internal/observability/progress"
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

// TestRateTrackerObserve walks the three states the time-derived fields must
// distinguish: no activity yet, a fresh advance, and a stall.
func TestRateTrackerObserve(t *testing.T) {
	clock := newFakeClock()
	tr := newRateTracker(clock.now())

	// No event observed yet: age is the -1 sentinel, rate 0.
	rate, age := tr.observe(0, clock.now())
	if age != -1 || rate != 0 {
		t.Fatalf("initial observe = (rate %v, age %d), want rate 0 age -1", rate, age)
	}

	// 10 events over the last 1s: rate 10/s, age 0 (just advanced).
	clock.advance(time.Second)
	rate, age = tr.observe(10, clock.now())
	if rate != 10 || age != 0 {
		t.Fatalf("advance observe = (rate %v, age %d), want rate 10 age 0", rate, age)
	}

	// Stalled: total frozen for 2s → rate 0, age 2000ms.
	clock.advance(2 * time.Second)
	rate, age = tr.observe(10, clock.now())
	if rate != 0 || age != 2000 {
		t.Fatalf("stall observe = (rate %v, age %d), want rate 0 age 2000", rate, age)
	}
}

// testSummaryPath is the summary output path injected via the GuestConfig.
const testSummaryPath = "/share/p.json"

// summaryCfg builds a runDeps cfg carrying the given summary observe path.
func summaryCfg(path string) *agentv1.GuestConfig {
	return &agentv1.GuestConfig{Observe: &agentv1.GuestConfig_Observe{Summary: path}}
}

// TestStartSummaryWriterDisabled: a nil write disables the writer — no
// console line, no goroutine.
func TestStartSummaryWriterDisabled(t *testing.T) {
	var console bytes.Buffer
	startSummaryWriter(context.Background(), &console,
		runDeps{cfg: summaryCfg(testSummaryPath), summary: summaryDeps{write: nil}},
		opensnoop.NewAggregator(), nil, nil, nil)
	if console.Len() != 0 {
		t.Fatalf("unexpected console output: %q", console.String())
	}
}

// TestStartSummaryWriterNoGadgets: writer wired but both aggregators nil →
// nothing to summarise, so no loop and no console line.
func TestStartSummaryWriterNoGadgets(t *testing.T) {
	wrote := false
	d := runDeps{cfg: summaryCfg(testSummaryPath), summary: summaryDeps{
		now: time.Now, interval: time.Millisecond,
		write: func(string, progress.Summary) error { wrote = true; return nil },
	}}
	var console bytes.Buffer
	startSummaryWriter(context.Background(), &console, d,
		nil, nil, nil, nil)
	time.Sleep(5 * time.Millisecond)
	if wrote || console.Len() != 0 {
		t.Fatalf("summary started with no gadgets: wrote=%v console=%q", wrote, console.String())
	}
}

// TestStartSummaryWriterAbsentToken: writer wired but no summary path in the
// config → silent skip.
func TestStartSummaryWriterAbsentToken(t *testing.T) {
	wrote := false
	d := runDeps{summary: summaryDeps{
		now: time.Now, interval: time.Millisecond,
		write: func(string, progress.Summary) error { wrote = true; return nil },
	}}
	var console bytes.Buffer
	startSummaryWriter(context.Background(), &console, d, opensnoop.NewAggregator(), nil, nil, nil)
	time.Sleep(5 * time.Millisecond) // a started loop would have written by now
	if wrote || console.Len() != 0 {
		t.Fatalf("summary started despite absent token: wrote=%v console=%q", wrote, console.String())
	}
}

// TestStartSummaryWriterHappyPath: the writer reports the path, ticks the
// summary from the live aggregator, and writes a final snapshot on cancel.
func TestStartSummaryWriterHappyPath(t *testing.T) {
	clock := newFakeClock()
	writes := make(chan progress.Summary, 16)
	d := runDeps{cfg: summaryCfg(testSummaryPath), summary: summaryDeps{
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
	exAgg := execsnoop.NewAggregator()
	exAgg.Observe(execsnoop.Event{Comm: "sh", Argv: []string{"/bin/cc"}})
	slAgg := sleepsnoop.NewAggregator()
	slAgg.Observe(sleepsnoop.Event{Comm: "poller", ReqNs: uint64(100 * time.Millisecond)})
	ioAgg := iosnoop.NewAggregator()
	ioAgg.Update(iosnoop.Totals{Calls: 1, WrBytes: 4096},
		[]iosnoop.Resolved{{Name: "/work/out.o", Stat: iosnoop.FDStat{WrBytes: 4096, Calls: 1}}})

	ctx, cancel := context.WithCancel(context.Background())
	var console bytes.Buffer
	startSummaryWriter(ctx, &console, d, agg, exAgg, slAgg, ioAgg)

	got := <-writes // the immediate initial write
	cancel()
	if got.Opensnoop == nil || got.Opensnoop.Total != 1 || got.Opensnoop.TopPaths[0].Key != "/nix/store/abc/x.h" {
		t.Fatalf("summary did not reflect the open-snoop aggregator: %+v", got.Opensnoop)
	}
	if got.ExecSnoop == nil || got.ExecSnoop.Total != 1 || got.ExecSnoop.TopCommands[0].Key != "/bin/cc" {
		t.Fatalf("summary did not reflect the exec-snoop aggregator: %+v", got.ExecSnoop)
	}
	if got.SleepSnoop == nil || got.SleepSnoop.Total != 1 || got.SleepSnoop.TopSleepers[0].Key != "poller" {
		t.Fatalf("summary did not reflect the sleep-snoop aggregator: %+v", got.SleepSnoop)
	}
	if got.IOSnoop == nil || got.IOSnoop.Total != 1 || got.IOSnoop.TopWriters[0].Key != "/work/out.o" {
		t.Fatalf("summary did not reflect the io-snoop aggregator: %+v", got.IOSnoop)
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
