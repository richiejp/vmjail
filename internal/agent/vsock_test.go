package agent

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tracev1 "github.com/richiejp/darkaifactory/internal/genpb/trace/v1"
	"github.com/richiejp/darkaifactory/internal/trace"
)

// TestRunWithVsockShipsSpansToFakeHost wires runWith with a fake
// dialVsock that returns one side of a net.Pipe; the test reads
// the other side and decodes the framed ReportSpansRequest. Same
// flow as the real e2e path but no VM, no kernel cmdline parser
// path — just the agent's tracer→exporter→dial chain.
func TestRunWithVsockShipsSpansToFakeHost(t *testing.T) {
	hostSide, agentSide := net.Pipe()
	t.Cleanup(func() { _ = hostSide.Close() })

	receivedCh := make(chan []*tracev1.Span, 1)
	go func() {
		spans, err := trace.ReadFramedBatch(hostSide)
		if err != nil {
			t.Logf("read framed: %v", err)
			receivedCh <- nil
			return
		}
		receivedCh <- spans
	}()

	deps := runDeps{
		readCmdline: func() (string, error) {
			return "console=hvc0 daft.vsock.trace_port=8000", nil
		},
		share: noopShareDeps(),
		dialVsock: func(_ context.Context, port uint32) (net.Conn, error) {
			if port != 8000 {
				return nil, errors.New("wrong port")
			}
			return agentSide, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled so runWith exits after the banner
	var buf bytes.Buffer
	_ = runWith(ctx, &buf, deps)

	select {
	case spans := <-receivedCh:
		if len(spans) == 0 {
			t.Fatalf("no spans received; agent output:\n%s", buf.String())
		}
		// Find the agent.boot span (others may exist from share
		// failures wrapped inside).
		found := false
		for _, s := range spans {
			if s.GetName() == "agent.boot" {
				found = true
				if s.GetComponent() != "daft-agent" {
					t.Errorf("component = %q, want daft-agent", s.GetComponent())
				}
			}
		}
		if !found {
			t.Errorf("agent.boot span missing; got names: %v", spanNames(spans))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("host side did not receive spans; agent output:\n%s", buf.String())
	}
}

// TestRunWithVsockMalformedCmdlinePortReports surfaces the parse
// error on the console without preventing the banner. The test
// also confirms no dial is attempted (since the cmdline never
// successfully resolved a port).
func TestRunWithVsockMalformedCmdlinePortReports(t *testing.T) {
	dialed := false
	deps := runDeps{
		readCmdline: func() (string, error) {
			return "daft.vsock.trace_port=notanumber", nil
		},
		share: noopShareDeps(),
		dialVsock: func(context.Context, uint32) (net.Conn, error) {
			dialed = true
			return nil, errors.New("should not be called")
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	_ = runWith(ctx, &buf, deps)

	if dialed {
		t.Error("dial attempted despite cmdline parse failure")
	}
	if !strings.Contains(buf.String(), TraceReportPrefix+"cmdline:") {
		t.Errorf("expected %s line for cmdline parse failure; got:\n%s", TraceReportPrefix, buf.String())
	}
	if !strings.Contains(buf.String(), Banner) {
		t.Errorf("banner missing despite trace failure; got:\n%s", buf.String())
	}
}

// TestRunWithVsockDialFailureReports confirms a Flush error (dial
// returns an error) lands on the console as TraceReportPrefix
// without preventing the banner or downstream behaviour.
func TestRunWithVsockDialFailureReports(t *testing.T) {
	deps := runDeps{
		readCmdline: func() (string, error) {
			return "daft.vsock.trace_port=8000", nil
		},
		share: noopShareDeps(),
		dialVsock: func(context.Context, uint32) (net.Conn, error) {
			return nil, errors.New("vsock dial nope")
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	_ = runWith(ctx, &buf, deps)

	if !strings.Contains(buf.String(), TraceReportPrefix) {
		t.Errorf("expected %s line for dial failure; got:\n%s", TraceReportPrefix, buf.String())
	}
	if !strings.Contains(buf.String(), "vsock dial nope") {
		t.Errorf("expected dial error text in report; got:\n%s", buf.String())
	}
}

// TestShutdownTracerNilIsNoOp covers the nil-tracer early return.
func TestShutdownTracerNilIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	shutdownTracer(context.Background(), nil, &buf)
	if buf.Len() != 0 {
		t.Errorf("shutdownTracer(nil) wrote %q, want nothing", buf.String())
	}
}

// TestRunWithNoVsockCmdlineSkipsShipping confirms a missing trace
// port leaves the agent shipping no spans (and no TraceReportPrefix
// line). Span data still accumulates in the local Tracer buffer —
// the integration test side never sees it.
func TestRunWithNoVsockCmdlineSkipsShipping(t *testing.T) {
	dialed := false
	deps := runDeps{
		readCmdline: func() (string, error) {
			return testCmdlineShareOnly, nil
		},
		share: noopShareDeps(),
		dialVsock: func(context.Context, uint32) (net.Conn, error) {
			dialed = true
			return nil, errors.New("dial should not have been called")
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	_ = runWith(ctx, &buf, deps)

	if dialed {
		t.Error("dial attempted despite absent trace_port cmdline")
	}
	if strings.Contains(buf.String(), TraceReportPrefix) {
		t.Errorf("unexpected %s line; got:\n%s", TraceReportPrefix, buf.String())
	}
}

func spanNames(spans []*tracev1.Span) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s.GetName())
	}
	return out
}

// noopShareDeps returns ShareDeps whose Mount/MkdirAll/ReadFile are
// safe no-ops so runWith doesn't side-effect the filesystem during
// these tests. Same pattern as newFakeShareDeps in share_test.go
// but minimal — these tests don't care about share content.
func noopShareDeps() ShareDeps {
	return ShareDeps{
		Mount:    func(_, _, _ string) error { return nil },
		MkdirAll: func(_ string, _ os.FileMode) error { return nil },
		ReadFile: func(name string) ([]byte, error) {
			if filepath.Base(name) == ShareMarkerFile {
				return []byte("noop"), nil
			}
			return nil, errors.New("not found")
		},
	}
}
