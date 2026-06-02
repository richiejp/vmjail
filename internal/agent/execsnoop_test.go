package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/localai-org/darkaifactory/internal/observability/ebpf/execsnoop"
)

// execsnoopTestDeps builds a runDeps wiring the exec-snoop gadget seam with a
// no-op tracefs mount and a fixed self-PID-namespace (container scope).
func execsnoopTestDeps(openLog func(string) (io.WriteCloser, error), run func(context.Context, io.Writer, *execsnoop.Aggregator, uint32) error) runDeps {
	return runDeps{execsnoop: execsnoopDeps{
		mountTracefs: func() error { return nil },
		openLog:      openLog,
		selfPIDNS:    func() (uint32, error) { return 4026531836, nil },
		run:          run,
	}}
}

func TestStartExecsnoopDisabledWhenRunNil(t *testing.T) {
	called := false
	openLog := func(string) (io.WriteCloser, error) { called = true; return newFakeLog(), nil }
	var console bytes.Buffer
	agg := startExecsnoopFromCmdline(context.Background(), &console,
		runDeps{execsnoop: execsnoopDeps{openLog: openLog, run: nil}},
		"daft.observe.execsnoop=/share/x.log")
	if agg != nil || called || console.Len() != 0 {
		t.Fatalf("disabled gadget did something: agg=%v called=%v console=%q", agg, called, console.String())
	}
}

func TestStartExecsnoopAbsentToken(t *testing.T) {
	run := func(context.Context, io.Writer, *execsnoop.Aggregator, uint32) error { return nil }
	var console bytes.Buffer
	if agg := startExecsnoopFromCmdline(context.Background(), &console,
		execsnoopTestDeps(func(string) (io.WriteCloser, error) { return newFakeLog(), nil }, run),
		"console=hvc0"); agg != nil || console.Len() != 0 {
		t.Fatalf("absent token started the gadget: agg=%v console=%q", agg, console.String())
	}
}

func TestStartExecsnoopReportsCmdlineError(t *testing.T) {
	run := func(context.Context, io.Writer, *execsnoop.Aggregator, uint32) error { return nil }
	var console bytes.Buffer
	startExecsnoopFromCmdline(context.Background(), &console,
		execsnoopTestDeps(func(string) (io.WriteCloser, error) { return newFakeLog(), nil }, run),
		"daft.observe.execsnoop=rel/path")
	if !strings.Contains(console.String(), ExecsnoopReportPrefix+"cmdline:") {
		t.Fatalf("missing cmdline error report: %q", console.String())
	}
}

func TestStartExecsnoopReportsTracefsError(t *testing.T) {
	run := func(context.Context, io.Writer, *execsnoop.Aggregator, uint32) error { return nil }
	var console bytes.Buffer
	d := runDeps{execsnoop: execsnoopDeps{
		mountTracefs: func() error { return errors.New("boom-tracefs") },
		openLog:      func(string) (io.WriteCloser, error) { return newFakeLog(), nil },
		run:          run,
	}}
	if agg := startExecsnoopFromCmdline(context.Background(), &console, d, "daft.observe.execsnoop=/share/x.log"); agg != nil {
		t.Fatal("gadget started despite tracefs failure")
	}
	if !strings.Contains(console.String(), ExecsnoopReportPrefix+"boom-tracefs") {
		t.Fatalf("missing tracefs error report: %q", console.String())
	}
}

func TestStartExecsnoopReportsOpenError(t *testing.T) {
	openLog := func(string) (io.WriteCloser, error) { return nil, errors.New("boom-open") }
	run := func(context.Context, io.Writer, *execsnoop.Aggregator, uint32) error {
		t.Fatal("run called despite open error")
		return nil
	}
	var console bytes.Buffer
	startExecsnoopFromCmdline(context.Background(), &console,
		execsnoopTestDeps(openLog, run), "daft.observe.execsnoop=/share/x.log")
	if !strings.Contains(console.String(), ExecsnoopReportPrefix+"boom-open") {
		t.Fatalf("missing open error report: %q", console.String())
	}
}

// TestStartExecsnoopHappyPath: the gadget opens the file, reports the
// container scope (selfPIDNS resolved), threads the namespace into run, and
// returns a live aggregator; ctx cancel closes the file.
func TestStartExecsnoopHappyPath(t *testing.T) {
	log := newFakeLog()
	var opened string
	openLog := func(path string) (io.WriteCloser, error) { opened = path; return log, nil }
	gotNS := make(chan uint32, 1)
	run := func(ctx context.Context, w io.Writer, agg *execsnoop.Aggregator, selfPidns uint32) error {
		gotNS <- selfPidns
		<-ctx.Done()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var console bytes.Buffer
	agg := startExecsnoopFromCmdline(ctx, &console, execsnoopTestDeps(openLog, run),
		"daft.observe.execsnoop=/share/execve.log")
	if agg == nil {
		t.Fatal("happy path returned a nil aggregator")
	}
	if ns := <-gotNS; ns != 4026531836 {
		t.Errorf("run received selfPidns = %d, want 4026531836", ns)
	}
	if opened != "/share/execve.log" {
		t.Fatalf("openLog path = %q", opened)
	}
	if !strings.Contains(console.String(), ExecsnoopReportPrefix+"tracing container execve → /share/execve.log") {
		t.Fatalf("missing container-scope report: %q", console.String())
	}
	cancel()
	<-log.closed
}

// TestStartExecsnoopReportsPIDNSError: a namespace read failure is reported
// and the gadget still starts unfiltered (selfPidns 0).
//
//nolint:dupl // parallel to the open-snoop pidns-error test by design.
func TestStartExecsnoopReportsPIDNSError(t *testing.T) {
	log := newFakeLog()
	gotNS := make(chan uint32, 1)
	d := runDeps{execsnoop: execsnoopDeps{
		mountTracefs: func() error { return nil },
		openLog:      func(string) (io.WriteCloser, error) { return log, nil },
		selfPIDNS:    func() (uint32, error) { return 0, errors.New("boom-pidns") },
		run: func(ctx context.Context, _ io.Writer, _ *execsnoop.Aggregator, selfPidns uint32) error {
			gotNS <- selfPidns
			<-ctx.Done()
			return nil
		},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	var console bytes.Buffer
	startExecsnoopFromCmdline(ctx, &console, d, "daft.observe.execsnoop=/share/x.log")
	if ns := <-gotNS; ns != 0 {
		t.Errorf("run received selfPidns = %d, want 0 (unfiltered fallback)", ns)
	}
	out := console.String()
	if !strings.Contains(out, ExecsnoopReportPrefix+"pidns: boom-pidns") ||
		!strings.Contains(out, ExecsnoopReportPrefix+"tracing all execve → ") {
		t.Fatalf("missing pidns-error fallback report: %q", out)
	}
	cancel()
	<-log.closed
}

// TestStartExecsnoopReportsRunError: a real mid-run failure (ctx still live)
// is surfaced by the drain goroutine.
func TestStartExecsnoopReportsRunError(t *testing.T) {
	log := newFakeLog()
	run := func(context.Context, io.Writer, *execsnoop.Aggregator, uint32) error { return errors.New("boom-run") }
	var console bytes.Buffer
	startExecsnoopFromCmdline(context.Background(), &console,
		execsnoopTestDeps(func(string) (io.WriteCloser, error) { return log, nil }, run),
		"daft.observe.execsnoop=/share/x.log")
	<-log.closed
	if !strings.Contains(console.String(), ExecsnoopReportPrefix+"boom-run") {
		t.Fatalf("missing run error report: %q", console.String())
	}
}

// TestDefaultExecsnoopDeps covers the production wiring.
func TestDefaultExecsnoopDeps(t *testing.T) {
	d := defaultExecsnoopDeps()
	if d.run == nil || d.mountTracefs == nil || d.openLog == nil || d.selfPIDNS == nil {
		t.Fatal("defaultExecsnoopDeps left a nil primitive")
	}
	if ns, err := d.selfPIDNS(); err != nil || ns == 0 {
		t.Fatalf("selfPIDNS() = (%d, %v), want a non-zero inode and no error", ns, err)
	}
}
