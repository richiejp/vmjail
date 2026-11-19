package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
	"github.com/richiejp/vmjail/internal/observability/ebpf/opensnoop"
)

// opensnoopCfg builds a GuestConfig carrying the open-snoop observe path.
func opensnoopCfg(path string) *agentv1.GuestConfig {
	return &agentv1.GuestConfig{Observe: &agentv1.GuestConfig_Observe{Opensnoop: path}}
}

// fakeLog is a fake trace file: its Close signals on a channel so a test
// can wait for the drain goroutine to finish (the deferred Close runs
// after run returns, so observing it means the goroutine is done).
type fakeLog struct{ closed chan struct{} }

func newFakeLog() *fakeLog { return &fakeLog{closed: make(chan struct{})} }

func (f *fakeLog) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeLog) Close() error                { close(f.closed); return nil }

// opensnoopTestDeps builds a runDeps wiring the gadget seam with a
// no-op tracefs mount (the privileged mount is covered separately).
func opensnoopTestDeps(openLog func(string) (io.WriteCloser, error), run func(context.Context, io.Writer, *opensnoop.Aggregator, uint32, func()) error) runDeps {
	return runDeps{opensnoop: opensnoopDeps{
		mountTracefs: func() error { return nil },
		openLog:      openLog,
		run:          run,
	}}
}

// TestStartOpensnoopDisabledWhenRunNil pins the zero-value-disables
// contract: a runDeps that never wired the gadget (the common
// unit-test path) does nothing — no file opened, no console line.
func TestStartOpensnoopDisabledWhenRunNil(t *testing.T) {
	called := false
	openLog := func(string) (io.WriteCloser, error) { called = true; return newFakeLog(), nil }
	var console bytes.Buffer
	startOpensnoop(context.Background(), &console,
		runDeps{cfg: opensnoopCfg("/share/x.log"), opensnoop: opensnoopDeps{openLog: openLog, run: nil}})
	if called {
		t.Fatal("openLog called though gadget disabled")
	}
	if console.Len() != 0 {
		t.Fatalf("unexpected console output: %q", console.String())
	}
}

// TestStartOpensnoopAbsentToken: gadget wired but no observe path →
// silent skip, like the other start* helpers.
func TestStartOpensnoopAbsentToken(t *testing.T) {
	called := false
	openLog := func(string) (io.WriteCloser, error) { called = true; return newFakeLog(), nil }
	run := func(context.Context, io.Writer, *opensnoop.Aggregator, uint32, func()) error { return nil }
	var console bytes.Buffer
	startOpensnoop(context.Background(), &console, opensnoopTestDeps(openLog, run))
	if called {
		t.Fatal("openLog called with no path configured")
	}
	if console.Len() != 0 {
		t.Fatalf("unexpected console output: %q", console.String())
	}
}

// TestStartOpensnoopReportsTracefsError: a tracefs mount failure is
// reported and the trace file is never opened (the attach would fail
// without tracefs anyway).
func TestStartOpensnoopReportsTracefsError(t *testing.T) {
	opened := false
	openLog := func(string) (io.WriteCloser, error) { opened = true; return newFakeLog(), nil }
	run := func(context.Context, io.Writer, *opensnoop.Aggregator, uint32, func()) error { return nil }
	var console bytes.Buffer
	d := runDeps{cfg: opensnoopCfg("/share/x.log"), opensnoop: opensnoopDeps{
		mountTracefs: func() error { return errors.New("boom-tracefs") },
		openLog:      openLog,
		run:          run,
	}}
	startOpensnoop(context.Background(), &console, d)
	if opened {
		t.Fatal("openLog called despite tracefs mount failure")
	}
	if !strings.Contains(console.String(), OpensnoopReportPrefix+"boom-tracefs") {
		t.Fatalf("missing tracefs error report: %q", console.String())
	}
}

// TestStartOpensnoopReportsOpenError: a failure opening the trace file
// is reported and the drain loop is not entered.
func TestStartOpensnoopReportsOpenError(t *testing.T) {
	openLog := func(string) (io.WriteCloser, error) { return nil, errors.New("boom-open") }
	runCalled := false
	run := func(context.Context, io.Writer, *opensnoop.Aggregator, uint32, func()) error {
		runCalled = true
		return nil
	}
	var console bytes.Buffer
	d := opensnoopTestDeps(openLog, run)
	d.cfg = opensnoopCfg("/share/x.log")
	startOpensnoop(context.Background(), &console, d)
	if runCalled {
		t.Fatal("run called despite open error")
	}
	if !strings.Contains(console.String(), OpensnoopReportPrefix+"boom-open") {
		t.Fatalf("missing open error report: %q", console.String())
	}
}

// TestStartOpensnoopHappyPath: the gadget opens the file, reports the
// path, drains into that exact writer, and closes the file when ctx is
// cancelled (the normal stop, which reports nothing).
func TestStartOpensnoopHappyPath(t *testing.T) {
	log := newFakeLog()
	var opened string
	openLog := func(path string) (io.WriteCloser, error) {
		opened = path
		return log, nil
	}
	started := make(chan io.Writer, 1)
	run := func(ctx context.Context, w io.Writer, _ *opensnoop.Aggregator, _ uint32, _ func()) error {
		started <- w
		<-ctx.Done()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var console bytes.Buffer
	// opensnoopTestDeps leaves selfPIDNS nil, so the filter is off and the
	// report says "all"; the container-scope path is covered separately.
	d := opensnoopTestDeps(openLog, run)
	d.cfg = opensnoopCfg("/share/openat.log")
	startOpensnoop(ctx, &console, d)

	gotW := <-started
	if gotW != io.Writer(log) {
		t.Fatal("run did not receive the opened trace file as its writer")
	}
	if opened != "/share/openat.log" {
		t.Fatalf("openLog path = %q, want /share/openat.log", opened)
	}
	if !strings.Contains(console.String(), OpensnoopReportPrefix+"tracing all openat → /share/openat.log") {
		t.Fatalf("missing tracing report: %q", console.String())
	}
	cancel()
	<-log.closed // run returned on ctx cancel → file closed
}

// TestStartOpensnoopFiltersToContainer: when selfPIDNS resolves, its value
// is threaded into the gadget run and the report names the container scope —
// the path that drops agent/init noise from the build summary.
func TestStartOpensnoopFiltersToContainer(t *testing.T) {
	log := newFakeLog()
	gotNS := make(chan uint32, 1)
	d := runDeps{cfg: opensnoopCfg("/share/openat.log"), opensnoop: opensnoopDeps{
		mountTracefs: func() error { return nil },
		openLog:      func(string) (io.WriteCloser, error) { return log, nil },
		selfPIDNS:    func() (uint32, error) { return 4026531836, nil },
		run: func(ctx context.Context, _ io.Writer, _ *opensnoop.Aggregator, selfPidns uint32, _ func()) error {
			gotNS <- selfPidns
			<-ctx.Done()
			return nil
		},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	var console bytes.Buffer
	startOpensnoop(ctx, &console, d)

	if ns := <-gotNS; ns != 4026531836 {
		t.Errorf("run received selfPidns = %d, want 4026531836", ns)
	}
	if !strings.Contains(console.String(), OpensnoopReportPrefix+"tracing container openat → ") {
		t.Fatalf("missing container-scope report: %q", console.String())
	}
	cancel()
	<-log.closed
}

// TestStartOpensnoopReportsPIDNSError: a namespace read failure is reported
// and the gadget still starts unfiltered (selfPidns 0) rather than not at
// all — a degraded but non-empty trace beats a silent gap.
//
//nolint:dupl // parallel to the exec-snoop pidns-error test by design.
func TestStartOpensnoopReportsPIDNSError(t *testing.T) {
	log := newFakeLog()
	gotNS := make(chan uint32, 1)
	d := runDeps{cfg: opensnoopCfg("/share/openat.log"), opensnoop: opensnoopDeps{
		mountTracefs: func() error { return nil },
		openLog:      func(string) (io.WriteCloser, error) { return log, nil },
		selfPIDNS:    func() (uint32, error) { return 0, errors.New("boom-pidns") },
		run: func(ctx context.Context, _ io.Writer, _ *opensnoop.Aggregator, selfPidns uint32, _ func()) error {
			gotNS <- selfPidns
			<-ctx.Done()
			return nil
		},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	var console bytes.Buffer
	startOpensnoop(ctx, &console, d)

	if ns := <-gotNS; ns != 0 {
		t.Errorf("run received selfPidns = %d, want 0 (unfiltered fallback)", ns)
	}
	out := console.String()
	if !strings.Contains(out, OpensnoopReportPrefix+"pidns: boom-pidns") ||
		!strings.Contains(out, OpensnoopReportPrefix+"tracing all openat → ") {
		t.Fatalf("missing pidns-error fallback report: %q", out)
	}
	cancel()
	<-log.closed
}

// TestStartOpensnoopReportsRunError: a real mid-run failure (ctx still
// live) is surfaced on the console by the drain goroutine. Waiting on
// the file Close (deferred, so it runs after the report write) gives a
// race-free read of the console.
func TestStartOpensnoopReportsRunError(t *testing.T) {
	log := newFakeLog()
	openLog := func(string) (io.WriteCloser, error) { return log, nil }
	run := func(context.Context, io.Writer, *opensnoop.Aggregator, uint32, func()) error {
		return errors.New("boom-run")
	}
	var console bytes.Buffer
	d := opensnoopTestDeps(openLog, run)
	d.cfg = opensnoopCfg("/share/x.log")
	startOpensnoop(context.Background(), &console, d)
	<-log.closed
	if !strings.Contains(console.String(), OpensnoopReportPrefix+"boom-run") {
		t.Fatalf("missing run error report: %q", console.String())
	}
}

// TestDefaultOpensnoopDeps covers the production wiring: run is the real
// gadget entry point, and openLog creates a writable, truncating file.
func TestDefaultOpensnoopDeps(t *testing.T) {
	d := defaultOpensnoopDeps()
	if d.run == nil {
		t.Fatal("defaultOpensnoopDeps run is nil")
	}
	// selfPIDNS is the real reader: on this Linux test host it must return
	// the test process's PID-namespace inode (non-zero), so the gadget gets
	// a usable filter rather than silently degrading to unfiltered.
	if d.selfPIDNS == nil {
		t.Fatal("defaultOpensnoopDeps selfPIDNS is nil")
	}
	if ns, err := d.selfPIDNS(); err != nil || ns == 0 {
		t.Fatalf("selfPIDNS() = (%d, %v), want a non-zero inode and no error", ns, err)
	}
	path := filepath.Join(t.TempDir(), "openat.log")
	wc, err := d.openLog(path)
	if err != nil {
		t.Fatalf("openLog: %v", err)
	}
	if _, err := io.WriteString(wc, "line\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := wc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // G304: path is t.TempDir()-derived test fixture.
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "line\n" {
		t.Fatalf("file = %q, want %q", got, "line\\n")
	}
}
