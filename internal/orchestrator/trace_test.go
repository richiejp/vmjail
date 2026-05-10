package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"github.com/richiejp/darkaifactory/internal/component"
	healthv1 "github.com/richiejp/darkaifactory/internal/genpb/health/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/health/v1/healthv1connect"
	tracev1 "github.com/richiejp/darkaifactory/internal/genpb/trace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/trace/v1/tracev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/trace"
)

func TestNewTraceHandlerNilTracer(t *testing.T) {
	if _, err := NewTraceHandler(nil); err == nil {
		t.Error("expected error for nil tracer")
	}
}

func TestTraceHandlerIngestsSpans(t *testing.T) {
	tr := trace.New("test")
	h, err := NewTraceHandler(tr)
	if err != nil {
		t.Fatalf("NewTraceHandler: %v", err)
	}
	traceID := strings.Repeat("a", 32)
	spanID := strings.Repeat("b", 16)
	req := connect.NewRequest(&tracev1.ReportSpansRequest{
		Spans: []*tracev1.Span{
			{
				TraceId:       traceID,
				SpanId:        spanID,
				Name:          "remote.op",
				Component:     "daft-test",
				StartUnixNano: time.Now().UnixNano(),
				EndUnixNano:   time.Now().Add(time.Millisecond).UnixNano(),
			},
		},
	})
	if _, err := h.ReportSpans(context.Background(), req); err != nil {
		t.Fatalf("ReportSpans: %v", err)
	}
	snap := tr.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot len = %d, want 1", len(snap))
	}
	if snap[0].TraceID() != traceID || snap[0].SpanID() != spanID {
		t.Errorf("ingested IDs mismatch: trace=%q span=%q", snap[0].TraceID(), snap[0].SpanID())
	}
}

// TestServerWiresTraceServiceAndDebugEndpoint exercises the end-to-
// end shape: a client-side Tracer ships spans via Connect-RPC; the
// orchestrator ingests them and surfaces them on /debug/traces.
func TestServerWiresTraceServiceAndDebugEndpoint(t *testing.T) {
	p := newTestPair(t, true, true)

	// Client-side: build a Tracer wired to ship via the same mTLS
	// transport we set up for the test pair.
	clientTracer := trace.New(component.Client)
	cliTLS, err := identity.ClientTLSConfig(p.cliID, p.cliStore, "")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	httpc := &http.Client{
		Transport: &http2.Transport{TLSClientConfig: cliTLS},
		Timeout:   5 * time.Second,
	}
	traceClient := tracev1connect.NewTraceServiceClient(httpc, p.url)
	clientTracer.SetExporter(trace.NewConnectExporter(traceClient))

	// Make a Ping with the client tracer's interceptor wired so we
	// generate a real client span; then ship.
	healthClient := healthv1connect.NewHealthServiceClient(httpc, p.url,
		connect.WithInterceptors(clientTracer.ClientInterceptor()))
	if _, err := healthClient.Ping(context.Background(),
		connect.NewRequest(&healthv1.PingRequest{Message: "trace"})); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := clientTracer.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Hit /debug/traces and verify both client and server spans
	// appear, sharing one trace_id.
	body := getDebugTraces(t, httpc, p.url+"/debug/traces", "application/json")
	var dump []map[string]any
	if err := json.Unmarshal(body, &dump); err != nil {
		t.Fatalf("json: %v\nbody=%s", err, string(body))
	}
	// We expect at least one trace whose spans cover both the daft
	// client and daft-orchestrator components: that's the Ping call,
	// linked across the wire via the propagated trace context.
	// (Reporting the trace itself produces an unrelated server span
	// in its own trace, since the export client deliberately bypasses
	// the interceptor — see ping.go.)
	byTrace := map[string]map[string]struct{}{}
	for _, s := range dump {
		id, _ := s[jsonKeyTraceID].(string)
		comp, _ := s[jsonKeyComponent].(string)
		if byTrace[id] == nil {
			byTrace[id] = map[string]struct{}{}
		}
		byTrace[id][comp] = struct{}{}
	}
	found := false
	for id, comps := range byTrace {
		_, hasClient := comps[component.Client]
		_, hasServer := comps[component.Orchestrator]
		if hasClient && hasServer {
			found = true
			t.Logf("matched trace %s with components %v", id, comps)
			break
		}
	}
	if !found {
		t.Errorf("no trace links daft+daft-orchestrator; byTrace=%v\nbody=%s", byTrace, string(body))
	}
}

func TestDebugTracesHandlerText(t *testing.T) {
	p := newTestPair(t, true, true)
	// Drive the server-side tracer to record at least one span by
	// issuing a Ping. The server's ServerInterceptor records it.
	if _, err := p.client.Ping(context.Background(),
		connect.NewRequest(&healthv1.PingRequest{Message: "x"})); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	cliTLS, err := identity.ClientTLSConfig(p.cliID, p.cliStore, "")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	httpc := &http.Client{
		Transport: &http2.Transport{TLSClientConfig: cliTLS},
		Timeout:   5 * time.Second,
	}
	body := getDebugTraces(t, httpc, p.url+"/debug/traces", "")
	if !bytes.Contains(body, []byte("daft.health.v1.HealthService/Ping")) {
		t.Errorf("text dump missing Ping span:\n%s", string(body))
	}
}

// getDebugTraces hits /debug/traces with the supplied Accept header
// and returns the body or fails the test.
func getDebugTraces(t *testing.T, httpc *http.Client, url, accept string) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return body
}

// Plain ServerHandler error reporting: server returns CodeInternal
// when a malformed request is sent. We don't have a public "make
// the handler return an error" path other than the proto codec, so
// just exercise that the handler returns a Response on a normal
// payload.
func TestTraceHandlerEmptyBatch(t *testing.T) {
	tr := trace.New("test")
	h, err := NewTraceHandler(tr)
	if err != nil {
		t.Fatalf("NewTraceHandler: %v", err)
	}
	resp, err := h.ReportSpans(context.Background(),
		connect.NewRequest(&tracev1.ReportSpansRequest{}))
	if err != nil {
		t.Fatalf("ReportSpans: %v", err)
	}
	if resp == nil || resp.Msg == nil {
		t.Error("expected non-nil response")
	}
}
