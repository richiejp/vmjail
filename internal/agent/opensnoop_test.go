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

	"github.com/localai-org/darkaifactory/internal/observability/ebpf/opensnoop"
)

// fakeLog is a fake trace file: its Close signals on a channel so a test
// can wait for the drain goroutine to finish (the deferred Close runs
// after run returns, so observing it means the goroutine is done).
type fakeLog struct{ closed chan struct{} }

func newFakeLog() *fakeLog { return &fakeLog{closed: make(chan struct{})} }

func (f *fakeLog) Write(p []byte) (int, error) { return len(p), nil }
func (f *fakeLog) Close() error                { close(f.closed); return nil }

// opensnoopTestDeps builds a runDeps wiring the gadget seam with a
// no-op tracefs mount (the privileged mount is covered separately).
func opensnoopTestDeps(openLog func(string) (io.WriteCloser, error), run func(context.Context, io.Writer, *opensnoop.Aggregator) error) runDeps {
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
	startOpensnoopFromCmdline(context.Background(), &console,
		runDeps{opensnoop: opensnoopDeps{openLog: openLog, run: nil}},
		"daft.observe.opensnoop=/share/x.log")
	if called {
		t.Fatal("openLog called though gadget disabled")
	}
	if console.Len() != 0 {
		t.Fatalf("unexpected console output: %q", console.String())
	}
}

// TestStartOpensnoopAbsentToken: gadget wired but no cmdline token →
// silent skip, like the other start*FromCmdline helpers.
func TestStartOpensnoopAbsentToken(t *testing.T) {
	called := false
	openLog := func(string) (io.WriteCloser, error) { called = true; return newFakeLog(), nil }
	run := func(context.Context, io.Writer, *opensnoop.Aggregator) error { return nil }
	var console bytes.Buffer
	startOpensnoopFromCmdline(context.Background(), &console,
		opensnoopTestDeps(openLog, run), "console=hvc0 reboot=t")
	if called {
		t.Fatal("openLog called with no token present")
	}
	if console.Len() != 0 {
		t.Fatalf("unexpected console output: %q", console.String())
	}
}

// TestStartOpensnoopReportsCmdlineError: a malformed (relative) path is
// reported and the gadget is not started.
func TestStartOpensnoopReportsCmdlineError(t *testing.T) {
	called := false
	openLog := func(string) (io.WriteCloser, error) { called = true; return newFakeLog(), nil }
	run := func(context.Context, io.Writer, *opensnoop.Aggregator) error { return nil }
	var console bytes.Buffer
	startOpensnoopFromCmdline(context.Background(), &console,
		opensnoopTestDeps(openLog, run), "daft.observe.opensnoop=rel/path")
	if called {
		t.Fatal("openLog called despite cmdline error")
	}
	if !strings.Contains(console.String(), OpensnoopReportPrefix+"cmdline:") {
		t.Fatalf("missing cmdline error report: %q", console.String())
	}
}

// TestStartOpensnoopReportsTracefsError: a tracefs mount failure is
// reported and the trace file is never opened (the attach would fail
// without tracefs anyway).
func TestStartOpensnoopReportsTracefsError(t *testing.T) {
	opened := false
	openLog := func(string) (io.WriteCloser, error) { opened = true; return newFakeLog(), nil }
	run := func(context.Context, io.Writer, *opensnoop.Aggregator) error { return nil }
	var console bytes.Buffer
	d := runDeps{opensnoop: opensnoopDeps{
		mountTracefs: func() error { return errors.New("boom-tracefs") },
		openLog:      openLog,
		run:          run,
	}}
	startOpensnoopFromCmdline(context.Background(), &console, d, "daft.observe.opensnoop=/share/x.log")
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
	run := func(context.Context, io.Writer, *opensnoop.Aggregator) error { runCalled = true; return nil }
	var console bytes.Buffer
	startOpensnoopFromCmdline(context.Background(), &console,
		opensnoopTestDeps(openLog, run), "daft.observe.opensnoop=/share/x.log")
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
	run := func(ctx context.Context, w io.Writer, _ *opensnoop.Aggregator) error {
		started <- w
		<-ctx.Done()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	var console bytes.Buffer
	startOpensnoopFromCmdline(ctx, &console, opensnoopTestDeps(openLog, run),
		"daft.observe.opensnoop=/share/openat.log")

	gotW := <-started
	if gotW != io.Writer(log) {
		t.Fatal("run did not receive the opened trace file as its writer")
	}
	if opened != "/share/openat.log" {
		t.Fatalf("openLog path = %q, want /share/openat.log", opened)
	}
	if !strings.Contains(console.String(), OpensnoopReportPrefix+"tracing openat → /share/openat.log") {
		t.Fatalf("missing tracing report: %q", console.String())
	}
	cancel()
	<-log.closed // run returned on ctx cancel → file closed
}

// TestStartOpensnoopReportsRunError: a real mid-run failure (ctx still
// live) is surfaced on the console by the drain goroutine. Waiting on
// the file Close (deferred, so it runs after the report write) gives a
// race-free read of the console.
func TestStartOpensnoopReportsRunError(t *testing.T) {
	log := newFakeLog()
	openLog := func(string) (io.WriteCloser, error) { return log, nil }
	run := func(context.Context, io.Writer, *opensnoop.Aggregator) error { return errors.New("boom-run") }
	var console bytes.Buffer
	startOpensnoopFromCmdline(context.Background(), &console,
		opensnoopTestDeps(openLog, run), "daft.observe.opensnoop=/share/x.log")
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
