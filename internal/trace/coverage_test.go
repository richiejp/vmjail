package trace

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	tracev1 "github.com/richiejp/vmjail/internal/genpb/trace/v1"
	"github.com/richiejp/vmjail/internal/genpb/trace/v1/tracev1connect"
)

type failAtWriter struct {
	writes int
	failAt int
}

func (w *failAtWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

// These tests fill gaps the main suite leaves uncovered: rare enum
// branches, accessors that aren't on the hot path of the e2e tests,
// and the context-tracer plumbing whose users live in other packages.

func TestNameAccessor(t *testing.T) {
	tr := New("test")
	_, s := tr.Start(context.Background(), "named.op")
	if s.Name() != "named.op" {
		t.Errorf("Name = %q, want named.op", s.Name())
	}
	var nilSpan *Span
	if got := nilSpan.Name(); got != "" {
		t.Errorf("nil span Name = %q, want empty", got)
	}
}

func TestStartFreeFunctionTracerFromContext(t *testing.T) {
	tr := New("test")
	ctx := ContextWithTracer(context.Background(), tr)
	ctx, span := Start(ctx, "op")
	if span == nil {
		t.Fatal("expected span when tracer is on context")
	}
	span.End()
	if got := tr.Snapshot(); len(got) != 1 || got[0].Name() != "op" {
		t.Errorf("snap = %+v", got)
	}
	_ = ctx
}

func TestStartFreeFunctionFallsBackToActiveSpanTracer(t *testing.T) {
	tr := New("test")
	parentCtx, parent := tr.Start(context.Background(), "parent")
	defer parent.End()
	// parentCtx has the span attached but NOT the tracer — Start
	// must locate the tracer via the span.
	_, child := Start(parentCtx, "child")
	if child == nil {
		t.Fatal("expected span via span-fallback")
	}
	if child.TraceID() != parent.TraceID() {
		t.Errorf("child trace_id %q != parent %q", child.TraceID(), parent.TraceID())
	}
	child.End()
}

func TestStartFreeFunctionWithoutAnyTracerIsNoop(t *testing.T) {
	ctx, span := Start(context.Background(), "op")
	if span != nil {
		t.Errorf("expected nil span when no tracer reachable, got %v", span)
	}
	span.End()
	span.SetAttribute("k", "v")
	_ = ctx
}

func TestSetExporterReplaces(t *testing.T) {
	a := &captureExporter{}
	b := &captureExporter{}
	tr := New("test", WithExporter(a))
	_, s := tr.Start(context.Background(), "op")
	s.End()
	tr.SetExporter(b)
	if err := tr.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if a.calls != 0 {
		t.Errorf("old exporter received %d calls", a.calls)
	}
	if b.calls != 1 {
		t.Errorf("new exporter received %d calls, want 1", b.calls)
	}
}

func TestContextWithTracerRoundTrip(t *testing.T) {
	tr := New("test")
	ctx := ContextWithTracer(context.Background(), tr)
	if got := TracerFromContext(ctx); got != tr {
		t.Errorf("round-trip mismatch")
	}
	if got := TracerFromContext(context.Background()); got != nil {
		t.Errorf("missing tracer = %v, want nil", got)
	}
	var nilCtx context.Context
	if got := TracerFromContext(nilCtx); got != nil {
		t.Errorf("nil ctx tracer = %v, want nil", got)
	}
}

func TestSpanKindRoundTrip(t *testing.T) {
	for _, k := range []SpanKind{SpanKindInternal, SpanKindClient, SpanKindServer} {
		got := spanKindFromProto(spanKindToProto(k))
		if got != k {
			t.Errorf("round-trip %v -> %v -> %v", k, spanKindToProto(k), got)
		}
	}
	// Default branch on unknown wire value.
	if got := spanKindFromProto(tracev1.SpanKind_SPAN_KIND_UNSPECIFIED); got != SpanKindInternal {
		t.Errorf("unspecified -> %v, want internal", got)
	}
	// SpanKindToProto on an out-of-range value falls through to
	// UNSPECIFIED.
	if got := spanKindToProto(SpanKind(99)); got != tracev1.SpanKind_SPAN_KIND_UNSPECIFIED {
		t.Errorf("unknown kind -> %v", got)
	}
}

func TestSpanStatusRoundTrip(t *testing.T) {
	for _, s := range []SpanStatus{StatusUnset, StatusOK, StatusError, StatusAbandoned} {
		got := spanStatusFromProto(spanStatusToProto(s))
		if got != s {
			t.Errorf("round-trip %v -> %v -> %v", s, spanStatusToProto(s), got)
		}
	}
	// Out-of-range source.
	if got := spanStatusToProto(SpanStatus(99)); got != tracev1.SpanStatus_SPAN_STATUS_UNSET {
		t.Errorf("unknown status -> %v", got)
	}
	// Unknown wire value defaults to Unset.
	if got := spanStatusFromProto(tracev1.SpanStatus(99)); got != StatusUnset {
		t.Errorf("unknown wire status -> %v", got)
	}
}

func TestKindAndStatusStrings(t *testing.T) {
	if got := kindShort(SpanKind(99)); got != "I" {
		t.Errorf("kindShort default = %q", got)
	}
	if got := SpanStatus(99).String(); got != "UNSET" {
		t.Errorf("SpanStatus default = %q", got)
	}
	if got := kindShort(SpanKindClient); got != "C" {
		t.Errorf("kindShort client = %q", got)
	}
	if got := kindShort(SpanKindServer); got != "S" {
		t.Errorf("kindShort server = %q", got)
	}
	if got := StatusOK.String(); got != "OK" {
		t.Errorf("StatusOK = %q", got)
	}
	if got := StatusAbandoned.String(); got != "ABANDONED" {
		t.Errorf("StatusAbandoned = %q", got)
	}
	if got := StatusError.String(); got != "ERR" {
		t.Errorf("StatusError = %q", got)
	}
	if got := SpanKindClient.String(); got != "client" {
		t.Errorf("SpanKindClient.String = %q", got)
	}
	if got := SpanKindServer.String(); got != "server" {
		t.Errorf("SpanKindServer.String = %q", got)
	}
	if got := SpanKindInternal.String(); got != "internal" {
		t.Errorf("SpanKindInternal.String = %q", got)
	}
}

func TestFormatTextPropagatesWriterFailures(t *testing.T) {
	if err := FormatText(&failAtWriter{failAt: 1}, nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("empty trace error = %v, want closed pipe", err)
	}

	tr := New("test")
	_, first := tr.Start(context.Background(), "first")
	first.SetAttribute("key", "value")
	first.End()
	if err := FormatText(&failAtWriter{failAt: 1}, tr.Snapshot()); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("header error = %v, want closed pipe", err)
	}
	if err := FormatText(&failAtWriter{failAt: 3}, tr.Snapshot()); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("attribute error = %v, want closed pipe", err)
	}

	_, second := tr.Start(context.Background(), "second")
	second.End()
	if err := FormatText(&failAtWriter{failAt: 4}, tr.Snapshot()); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("trace separator error = %v, want closed pipe", err)
	}
}

func TestConnectExporterRoundTrip(t *testing.T) {
	var captured *tracev1.ReportSpansRequest
	mux := http.NewServeMux()
	path, handler := tracev1connect.NewTraceServiceHandler(&captureTraceServer{onReport: func(req *tracev1.ReportSpansRequest) {
		captured = req
	}})
	mux.Handle(path, handler)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	client := tracev1connect.NewTraceServiceClient(http.DefaultClient, ts.URL)
	exp := NewConnectExporter(client)
	spans := []*tracev1.Span{{TraceId: strings.Repeat("a", 32), SpanId: strings.Repeat("b", 16), Name: "x"}}
	if err := exp.Export(context.Background(), spans); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if captured == nil || len(captured.GetSpans()) != 1 {
		t.Errorf("server captured = %+v", captured)
	}
}

func TestConnectExporterPropagatesError(t *testing.T) {
	mux := http.NewServeMux()
	path, handler := tracev1connect.NewTraceServiceHandler(&captureTraceServer{
		err: connect.NewError(connect.CodeUnavailable, errors.New("nope")),
	})
	mux.Handle(path, handler)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	client := tracev1connect.NewTraceServiceClient(http.DefaultClient, ts.URL)
	exp := NewConnectExporter(client)
	spans := []*tracev1.Span{{TraceId: strings.Repeat("a", 32), SpanId: strings.Repeat("b", 16)}}
	if err := exp.Export(context.Background(), spans); err == nil {
		t.Fatal("expected error")
	}
}

type captureTraceServer struct {
	tracev1connect.UnimplementedTraceServiceHandler
	onReport func(*tracev1.ReportSpansRequest)
	err      error
}

func (s *captureTraceServer) ReportSpans(_ context.Context, req *connect.Request[tracev1.ReportSpansRequest]) (*connect.Response[tracev1.ReportSpansResponse], error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.onReport != nil {
		s.onReport(req.Msg)
	}
	return connect.NewResponse(&tracev1.ReportSpansResponse{}), nil
}

// TestEndSpanIsIdempotentInternally exercises the second-call early-
// return inside endSpan that the public End test misses (because End
// is a thin wrapper).
func TestEndSpanIdempotentLowLevel(t *testing.T) {
	tr := New("test")
	_, s := tr.Start(context.Background(), "op")
	tr.endSpan(s, tr.nowFn(), false)
	tr.endSpan(s, tr.nowFn(), false) // should be a no-op now that finished
	if got := tr.Snapshot(); len(got) != 1 {
		t.Errorf("Snapshot len = %d, want 1", len(got))
	}
}

// TestSetStatusOnFinishedSpan ensures the post-finish guard in
// SetStatus doesn't accidentally let mutations through.
func TestSetStatusOnFinishedSpan(t *testing.T) {
	tr := New("test")
	_, s := tr.Start(context.Background(), "op")
	s.SetStatus(StatusOK, "")
	s.End()
	s.SetStatus(StatusError, "too late")
	snap := tr.Snapshot()
	if snap[0].status != StatusOK {
		t.Errorf("status mutated after End: %v", snap[0].status)
	}
}

// TestRecordErrorOnFinishedSpan covers the same guard for RecordError.
func TestRecordErrorOnFinishedSpan(t *testing.T) {
	tr := New("test")
	_, s := tr.Start(context.Background(), "op")
	s.End()
	myErr := errors.New("late")
	if got := s.RecordError(myErr); !errors.Is(got, myErr) {
		t.Errorf("RecordError on finished span should still return err")
	}
	snap := tr.Snapshot()
	if snap[0].status == StatusError {
		t.Errorf("late RecordError leaked: %v", snap[0].status)
	}
}

// TestSetAttributeAndAddEventOnFinishedSpan: the no-op-after-finish
// path on those mutators.
func TestMutatorsOnFinishedSpan(t *testing.T) {
	tr := New("test")
	_, s := tr.Start(context.Background(), "op")
	s.End()
	s.SetAttribute("late", "x")
	s.AddEvent("late")
	snap := tr.Snapshot()
	if len(snap[0].attrs) != 0 || len(snap[0].events) != 0 {
		t.Errorf("late mutations leaked: %+v", snap[0])
	}
}
