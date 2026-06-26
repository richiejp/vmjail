//nolint:dupl // deliberately parallel to sleepsnoop_test.go / execsnoop_test.go / opensnoop_test.go: the per-gadget agent-wiring tests differ only by gadget type. Kept side-by-side for review.
package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
	"github.com/richiejp/darkaifactory/internal/observability/ebpf/iosnoop"
)

// iosnoopCfg builds a GuestConfig carrying the io-snoop observe path.
func iosnoopCfg(path string) *agentv1.GuestConfig {
	return &agentv1.GuestConfig{Observe: &agentv1.GuestConfig_Observe{Iosnoop: path}}
}

// iosnoopTestDeps builds a runDeps wiring the io-snoop gadget seam with a no-op
// tracefs mount and a fixed self-PID-namespace (container scope).
func iosnoopTestDeps(openLog func(string) (io.WriteCloser, error), run func(context.Context, io.Writer, *iosnoop.Aggregator, uint32, func()) error) runDeps {
	return runDeps{iosnoop: iosnoopDeps{
		mountTracefs: func() error { return nil },
		openLog:      openLog,
		selfPIDNS:    func() (uint32, error) { return 4026531836, nil },
		run:          run,
	}}
}

func TestStartIosnoopDisabledWhenRunNil(t *testing.T) {
	called := false
	openLog := func(string) (io.WriteCloser, error) { called = true; return newFakeLog(), nil }
	var console bytes.Buffer
	agg, _ := startIosnoop(context.Background(), &console,
		runDeps{cfg: iosnoopCfg("/share/x.log"), iosnoop: iosnoopDeps{openLog: openLog, run: nil}})
	if agg != nil || called || console.Len() != 0 {
		t.Fatalf("disabled gadget did something: agg=%v called=%v console=%q", agg, called, console.String())
	}
}

func TestStartIosnoopAbsentToken(t *testing.T) {
	run := func(context.Context, io.Writer, *iosnoop.Aggregator, uint32, func()) error { return nil }
	var console bytes.Buffer
	if agg, _ := startIosnoop(context.Background(), &console,
		iosnoopTestDeps(func(string) (io.WriteCloser, error) { return newFakeLog(), nil }, run)); agg != nil || console.Len() != 0 {
		t.Fatalf("absent token started the gadget: agg=%v console=%q", agg, console.String())
	}
}

func TestStartIosnoopReportsTracefsError(t *testing.T) {
	run := func(context.Context, io.Writer, *iosnoop.Aggregator, uint32, func()) error { return nil }
	var console bytes.Buffer
	d := runDeps{cfg: iosnoopCfg("/share/x.log"), iosnoop: iosnoopDeps{
		mountTracefs: func() error { return errors.New("boom-tracefs") },
		openLog:      func(string) (io.WriteCloser, error) { return newFakeLog(), nil },
		run:          run,
	}}
	if agg, _ := startIosnoop(context.Background(), &console, d); agg != nil {
		t.Fatal("gadget started despite tracefs failure")
	}
	if !strings.Contains(console.String(), IosnoopReportPrefix+"boom-tracefs") {
		t.Fatalf("missing tracefs error report: %q", console.String())
	}
}

func TestStartIosnoopReportsOpenError(t *testing.T) {
	openLog := func(string) (io.WriteCloser, error) { return nil, errors.New("boom-open") }
	run := func(context.Context, io.Writer, *iosnoop.Aggregator, uint32, func()) error {
		t.Fatal("run called despite open error")
		return nil
	}
	var console bytes.Buffer
	d := iosnoopTestDeps(openLog, run)
	d.cfg = iosnoopCfg("/share/x.log")
	startIosnoop(context.Background(), &console, d)
	if !strings.Contains(console.String(), IosnoopReportPrefix+"boom-open") {
		t.Fatalf("missing open error report: %q", console.String())
	}
}

// TestStartIosnoopHappyPath: the gadget opens the file, reports the container
// scope (selfPIDNS resolved), threads the namespace into run, and returns a
// live aggregator; ctx cancel closes the file.
func TestStartIosnoopHappyPath(t *testing.T) {
	log := newFakeLog()
	var opened string
	openLog := func(path string) (io.WriteCloser, error) { opened = path; return log, nil }
	gotNS := make(chan uint32, 1)
	run := func(ctx context.Context, w io.Writer, agg *iosnoop.Aggregator, selfPidns uint32, _ func()) error {
		gotNS <- selfPidns
		<-ctx.Done()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var console bytes.Buffer
	d := iosnoopTestDeps(openLog, run)
	d.cfg = iosnoopCfg("/share/io.log")
	agg, _ := startIosnoop(ctx, &console, d)
	if agg == nil {
		t.Fatal("happy path returned a nil aggregator")
	}
	if ns := <-gotNS; ns != 4026531836 {
		t.Errorf("run received selfPidns = %d, want 4026531836", ns)
	}
	if opened != "/share/io.log" {
		t.Fatalf("openLog path = %q", opened)
	}
	if !strings.Contains(console.String(), IosnoopReportPrefix+"tracing container fd I/O → /share/io.log") {
		t.Fatalf("missing container-scope report: %q", console.String())
	}
	cancel()
	<-log.closed
}

// TestStartIosnoopReportsPIDNSError: a namespace read failure is reported and
// the gadget still starts unfiltered (selfPidns 0).
//
//nolint:dupl // parallel to the open-snoop / sleep-snoop pidns-error tests by design.
func TestStartIosnoopReportsPIDNSError(t *testing.T) {
	log := newFakeLog()
	gotNS := make(chan uint32, 1)
	d := runDeps{cfg: iosnoopCfg("/share/x.log"), iosnoop: iosnoopDeps{
		mountTracefs: func() error { return nil },
		openLog:      func(string) (io.WriteCloser, error) { return log, nil },
		selfPIDNS:    func() (uint32, error) { return 0, errors.New("boom-pidns") },
		run: func(ctx context.Context, _ io.Writer, _ *iosnoop.Aggregator, selfPidns uint32, _ func()) error {
			gotNS <- selfPidns
			<-ctx.Done()
			return nil
		},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	var console bytes.Buffer
	startIosnoop(ctx, &console, d)
	if ns := <-gotNS; ns != 0 {
		t.Errorf("run received selfPidns = %d, want 0 (unfiltered fallback)", ns)
	}
	out := console.String()
	if !strings.Contains(out, IosnoopReportPrefix+"pidns: boom-pidns") ||
		!strings.Contains(out, IosnoopReportPrefix+"tracing all fd I/O → ") {
		t.Fatalf("missing pidns-error fallback report: %q", out)
	}
	cancel()
	<-log.closed
}

// TestStartIosnoopReportsRunError: a real mid-run failure (ctx still live) is
// surfaced by the drain goroutine.
func TestStartIosnoopReportsRunError(t *testing.T) {
	log := newFakeLog()
	run := func(context.Context, io.Writer, *iosnoop.Aggregator, uint32, func()) error {
		return errors.New("boom-run")
	}
	var console bytes.Buffer
	d := iosnoopTestDeps(func(string) (io.WriteCloser, error) { return log, nil }, run)
	d.cfg = iosnoopCfg("/share/x.log")
	startIosnoop(context.Background(), &console, d)
	<-log.closed
	if !strings.Contains(console.String(), IosnoopReportPrefix+"boom-run") {
		t.Fatalf("missing run error report: %q", console.String())
	}
}

// TestDefaultIosnoopDeps covers the production wiring.
func TestDefaultIosnoopDeps(t *testing.T) {
	d := defaultIosnoopDeps()
	if d.run == nil || d.mountTracefs == nil || d.openLog == nil || d.selfPIDNS == nil {
		t.Fatal("defaultIosnoopDeps left a nil primitive")
	}
	if ns, err := d.selfPIDNS(); err != nil || ns == 0 {
		t.Fatalf("selfPIDNS() = (%d, %v), want a non-zero inode and no error", ns, err)
	}
}
