package trace

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	healthv1 "github.com/localai-org/darkaifactory/internal/genpb/health/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/health/v1/healthv1connect"
	tracev1 "github.com/localai-org/darkaifactory/internal/genpb/trace/v1"
)

// pingHandler is a minimal HealthService implementation that
// records the trace headers it sees so the interceptor tests can
// assert on them. We don't import orchestrator's HealthHandler to
// avoid the cycle.
type pingHandler struct {
	healthv1connect.UnimplementedHealthServiceHandler
	gotTrace  string
	gotParent string
	failWith  error
}

func (p *pingHandler) Ping(_ context.Context, req *connect.Request[healthv1.PingRequest]) (*connect.Response[healthv1.PingResponse], error) {
	p.gotTrace = req.Header().Get(HeaderTraceID)
	p.gotParent = req.Header().Get(HeaderParentSpanID)
	if p.failWith != nil {
		return nil, p.failWith
	}
	return connect.NewResponse(&healthv1.PingResponse{Message: req.Msg.GetMessage()}), nil
}

// startInprocessServer spins up a Health service in-process over
// HTTP/1.1 (no mTLS overhead) for interceptor unit tests. Returns
// the URL and the handler so tests can inspect headers it saw.
func startInprocessServer(t *testing.T, srv *Tracer) (string, *pingHandler) {
	t.Helper()
	h := &pingHandler{}
	mux := http.NewServeMux()
	path, handler := healthv1connect.NewHealthServiceHandler(h,
		connect.WithInterceptors(srv.ServerInterceptor()))
	mux.Handle(path, handler)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, h
}

func TestClientInterceptorInjectsHeaders(t *testing.T) {
	srvTracer := New("server")
	url, h := startInprocessServer(t, srvTracer)

	cliTracer := New("client")
	client := healthv1connect.NewHealthServiceClient(http.DefaultClient, url,
		connect.WithInterceptors(cliTracer.ClientInterceptor()))
	if _, err := client.Ping(context.Background(),
		connect.NewRequest(&healthv1.PingRequest{Message: "x"})); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if !IsValidTraceID(h.gotTrace) {
		t.Errorf("server saw bad trace_id %q", h.gotTrace)
	}
	if !IsValidSpanID(h.gotParent) {
		t.Errorf("server saw bad parent_span_id %q", h.gotParent)
	}

	cliSnap := cliTracer.Snapshot()
	srvSnap := srvTracer.Snapshot()
	if len(cliSnap) != 1 || cliSnap[0].kind != SpanKindClient {
		t.Errorf("client snap = %+v", cliSnap)
	}
	if len(srvSnap) != 1 || srvSnap[0].kind != SpanKindServer {
		t.Errorf("server snap = %+v", srvSnap)
	}
	if cliSnap[0].traceID != srvSnap[0].traceID {
		t.Errorf("trace_ids differ: client=%q server=%q",
			cliSnap[0].traceID, srvSnap[0].traceID)
	}
	if srvSnap[0].parentID != cliSnap[0].spanID {
		t.Errorf("server parent %q != client span_id %q",
			srvSnap[0].parentID, cliSnap[0].spanID)
	}
}

func TestServerInterceptorIgnoresMalformedHeaders(t *testing.T) {
	srvTracer := New("server")
	url, h := startInprocessServer(t, srvTracer)

	// Direct call without the client interceptor: send a malformed
	// trace header. The server should start a fresh trace rather
	// than parent off the bogus value.
	client := healthv1connect.NewHealthServiceClient(http.DefaultClient, url)
	req := connect.NewRequest(&healthv1.PingRequest{Message: "x"})
	req.Header().Set(HeaderTraceID, "not-hex")
	req.Header().Set(HeaderParentSpanID, "not-hex")
	if _, err := client.Ping(context.Background(), req); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	srvSnap := srvTracer.Snapshot()
	if len(srvSnap) != 1 {
		t.Fatalf("snap len = %d", len(srvSnap))
	}
	if srvSnap[0].traceID == "not-hex" {
		t.Errorf("server adopted malformed trace_id")
	}
	if srvSnap[0].parentID != "" {
		t.Errorf("server kept malformed parent: %q", srvSnap[0].parentID)
	}
	_ = h
}

func TestClientInterceptorRecordsErrors(t *testing.T) {
	srvTracer := New("server")
	url, h := startInprocessServer(t, srvTracer)
	h.failWith = connect.NewError(connect.CodeUnavailable, errors.New("nope"))

	cliTracer := New("client")
	client := healthv1connect.NewHealthServiceClient(http.DefaultClient, url,
		connect.WithInterceptors(cliTracer.ClientInterceptor()))
	if _, err := client.Ping(context.Background(),
		connect.NewRequest(&healthv1.PingRequest{Message: "x"})); err == nil {
		t.Fatal("expected Ping error")
	}
	cliSnap := cliTracer.Snapshot()
	if len(cliSnap) != 1 {
		t.Fatalf("snap len = %d", len(cliSnap))
	}
	if cliSnap[0].status != StatusError {
		t.Errorf("client span status = %v, want Error", cliSnap[0].status)
	}
	srvSnap := srvTracer.Snapshot()
	if srvSnap[0].status != StatusError {
		t.Errorf("server span status = %v, want Error", srvSnap[0].status)
	}
}

// TestNewConnectExporter verifies the constructor handles the
// trivial case (used by `daft ping`).
func TestNewConnectExporter(t *testing.T) {
	if e := NewConnectExporter(nil); e == nil || e.Client != nil {
		t.Errorf("constructor returned %+v", e)
	}
}

// TestConnectExporterEmptySpansShortCircuits verifies we don't
// pay for a no-op RPC.
func TestConnectExporterEmptySpansShortCircuits(t *testing.T) {
	exp := &ConnectExporter{}
	if err := exp.Export(context.Background(), nil); err != nil {
		t.Errorf("Export: %v", err)
	}
	if err := exp.Export(context.Background(), []*tracev1.Span{}); err != nil {
		t.Errorf("Export: %v", err)
	}
	var nilExp *ConnectExporter
	if err := nilExp.Export(context.Background(), nil); err != nil {
		t.Errorf("nil receiver Export: %v", err)
	}
}
