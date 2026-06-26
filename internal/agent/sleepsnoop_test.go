//nolint:dupl // deliberately parallel to execsnoop_test.go / opensnoop_test.go: the per-gadget agent-wiring tests differ only by gadget type. Kept side-by-side for review.
package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
	"github.com/richiejp/darkaifactory/internal/observability/ebpf/sleepsnoop"
)

// sleepsnoopCfg builds a GuestConfig carrying the sleep-snoop observe path.
func sleepsnoopCfg(path string) *agentv1.GuestConfig {
	return &agentv1.GuestConfig{Observe: &agentv1.GuestConfig_Observe{Sleepsnoop: path}}
}

// sleepsnoopTestDeps builds a runDeps wiring the sleep-snoop gadget seam with a
// no-op tracefs mount and a fixed self-PID-namespace (container scope).
func sleepsnoopTestDeps(openLog func(string) (io.WriteCloser, error), run func(context.Context, io.Writer, *sleepsnoop.Aggregator, uint32, func()) error) runDeps {
	return runDeps{sleepsnoop: sleepsnoopDeps{
		mountTracefs: func() error { return nil },
		openLog:      openLog,
		selfPIDNS:    func() (uint32, error) { return 4026531836, nil },
		run:          run,
	}}
}

func TestStartSleepsnoopDisabledWhenRunNil(t *testing.T) {
	called := false
	openLog := func(string) (io.WriteCloser, error) { called = true; return newFakeLog(), nil }
	var console bytes.Buffer
	agg, _ := startSleepsnoop(context.Background(), &console,
		runDeps{cfg: sleepsnoopCfg("/share/x.log"), sleepsnoop: sleepsnoopDeps{openLog: openLog, run: nil}})
	if agg != nil || called || console.Len() != 0 {
		t.Fatalf("disabled gadget did something: agg=%v called=%v console=%q", agg, called, console.String())
	}
}

func TestStartSleepsnoopAbsentToken(t *testing.T) {
	run := func(context.Context, io.Writer, *sleepsnoop.Aggregator, uint32, func()) error { return nil }
	var console bytes.Buffer
	if agg, _ := startSleepsnoop(context.Background(), &console,
		sleepsnoopTestDeps(func(string) (io.WriteCloser, error) { return newFakeLog(), nil }, run)); agg != nil || console.Len() != 0 {
		t.Fatalf("absent token started the gadget: agg=%v console=%q", agg, console.String())
	}
}

func TestStartSleepsnoopReportsTracefsError(t *testing.T) {
	run := func(context.Context, io.Writer, *sleepsnoop.Aggregator, uint32, func()) error { return nil }
	var console bytes.Buffer
	d := runDeps{cfg: sleepsnoopCfg("/share/x.log"), sleepsnoop: sleepsnoopDeps{
		mountTracefs: func() error { return errors.New("boom-tracefs") },
		openLog:      func(string) (io.WriteCloser, error) { return newFakeLog(), nil },
		run:          run,
	}}
	if agg, _ := startSleepsnoop(context.Background(), &console, d); agg != nil {
		t.Fatal("gadget started despite tracefs failure")
	}
	if !strings.Contains(console.String(), SleepsnoopReportPrefix+"boom-tracefs") {
		t.Fatalf("missing tracefs error report: %q", console.String())
	}
}

func TestStartSleepsnoopReportsOpenError(t *testing.T) {
	openLog := func(string) (io.WriteCloser, error) { return nil, errors.New("boom-open") }
	run := func(context.Context, io.Writer, *sleepsnoop.Aggregator, uint32, func()) error {
		t.Fatal("run called despite open error")
		return nil
	}
	var console bytes.Buffer
	d := sleepsnoopTestDeps(openLog, run)
	d.cfg = sleepsnoopCfg("/share/x.log")
	startSleepsnoop(context.Background(), &console, d)
	if !strings.Contains(console.String(), SleepsnoopReportPrefix+"boom-open") {
		t.Fatalf("missing open error report: %q", console.String())
	}
}

// TestStartSleepsnoopHappyPath: the gadget opens the file, reports the
// container scope (selfPIDNS resolved), threads the namespace into run, and
// returns a live aggregator; ctx cancel closes the file.
func TestStartSleepsnoopHappyPath(t *testing.T) {
	log := newFakeLog()
	var opened string
	openLog := func(path string) (io.WriteCloser, error) { opened = path; return log, nil }
	gotNS := make(chan uint32, 1)
	run := func(ctx context.Context, w io.Writer, agg *sleepsnoop.Aggregator, selfPidns uint32, _ func()) error {
		gotNS <- selfPidns
		<-ctx.Done()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var console bytes.Buffer
	d := sleepsnoopTestDeps(openLog, run)
	d.cfg = sleepsnoopCfg("/share/sleep.log")
	agg, _ := startSleepsnoop(ctx, &console, d)
	if agg == nil {
		t.Fatal("happy path returned a nil aggregator")
	}
	if ns := <-gotNS; ns != 4026531836 {
		t.Errorf("run received selfPidns = %d, want 4026531836", ns)
	}
	if opened != "/share/sleep.log" {
		t.Fatalf("openLog path = %q", opened)
	}
	if !strings.Contains(console.String(), SleepsnoopReportPrefix+"tracing container nanosleep → /share/sleep.log") {
		t.Fatalf("missing container-scope report: %q", console.String())
	}
	cancel()
	<-log.closed
}

// TestStartSleepsnoopReportsPIDNSError: a namespace read failure is reported
// and the gadget still starts unfiltered (selfPidns 0).
//
//nolint:dupl // parallel to the open-snoop / exec-snoop pidns-error tests by design.
func TestStartSleepsnoopReportsPIDNSError(t *testing.T) {
	log := newFakeLog()
	gotNS := make(chan uint32, 1)
	d := runDeps{cfg: sleepsnoopCfg("/share/x.log"), sleepsnoop: sleepsnoopDeps{
		mountTracefs: func() error { return nil },
		openLog:      func(string) (io.WriteCloser, error) { return log, nil },
		selfPIDNS:    func() (uint32, error) { return 0, errors.New("boom-pidns") },
		run: func(ctx context.Context, _ io.Writer, _ *sleepsnoop.Aggregator, selfPidns uint32, _ func()) error {
			gotNS <- selfPidns
			<-ctx.Done()
			return nil
		},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	var console bytes.Buffer
	startSleepsnoop(ctx, &console, d)
	if ns := <-gotNS; ns != 0 {
		t.Errorf("run received selfPidns = %d, want 0 (unfiltered fallback)", ns)
	}
	out := console.String()
	if !strings.Contains(out, SleepsnoopReportPrefix+"pidns: boom-pidns") ||
		!strings.Contains(out, SleepsnoopReportPrefix+"tracing all nanosleep → ") {
		t.Fatalf("missing pidns-error fallback report: %q", out)
	}
	cancel()
	<-log.closed
}

// TestStartSleepsnoopReportsRunError: a real mid-run failure (ctx still live)
// is surfaced by the drain goroutine.
func TestStartSleepsnoopReportsRunError(t *testing.T) {
	log := newFakeLog()
	run := func(context.Context, io.Writer, *sleepsnoop.Aggregator, uint32, func()) error {
		return errors.New("boom-run")
	}
	var console bytes.Buffer
	d := sleepsnoopTestDeps(func(string) (io.WriteCloser, error) { return log, nil }, run)
	d.cfg = sleepsnoopCfg("/share/x.log")
	startSleepsnoop(context.Background(), &console, d)
	<-log.closed
	if !strings.Contains(console.String(), SleepsnoopReportPrefix+"boom-run") {
		t.Fatalf("missing run error report: %q", console.String())
	}
}

// TestDefaultSleepsnoopDeps covers the production wiring.
func TestDefaultSleepsnoopDeps(t *testing.T) {
	d := defaultSleepsnoopDeps()
	if d.run == nil || d.mountTracefs == nil || d.openLog == nil || d.selfPIDNS == nil {
		t.Fatal("defaultSleepsnoopDeps left a nil primitive")
	}
	if ns, err := d.selfPIDNS(); err != nil || ns == 0 {
		t.Fatalf("selfPIDNS() = (%d, %v), want a non-zero inode and no error", ns, err)
	}
}
