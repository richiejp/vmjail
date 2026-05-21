package trace

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tracev1 "github.com/localai-org/darkaifactory/internal/genpb/trace/v1"
)

func TestStartAndEndAppendsToBuffer(t *testing.T) {
	tr := New("test")
	ctx, span := tr.Start(context.Background(), "op")
	if span == nil {
		t.Fatal("Start returned nil span")
	}
	if span.TraceID() == "" || span.SpanID() == "" {
		t.Errorf("missing IDs: trace=%q span=%q", span.TraceID(), span.SpanID())
	}
	if got := SpanFromContext(ctx); got != span {
		t.Errorf("SpanFromContext returned different span")
	}
	span.End()
	got := tr.Snapshot()
	if len(got) != 1 {
		t.Fatalf("Snapshot len = %d, want 1", len(got))
	}
	if got[0].status != StatusOK {
		t.Errorf("status = %v, want OK after clean End", got[0].status)
	}
}

func TestNestedSpansShareTraceID(t *testing.T) {
	tr := New("test")
	ctx, root := tr.Start(context.Background(), "root")
	defer root.End()
	_, child := tr.Start(ctx, "child")
	defer child.End()
	if child.TraceID() != root.TraceID() {
		t.Errorf("child trace_id %q != root %q", child.TraceID(), root.TraceID())
	}
	if child.ParentSpanID() != root.SpanID() {
		t.Errorf("child parent %q != root span_id %q", child.ParentSpanID(), root.SpanID())
	}
}

func TestStartWithParent(t *testing.T) {
	tr := New("test")
	wireTrace := strings.Repeat("a", 32)
	wireParent := strings.Repeat("b", 16)
	_, span := tr.Start(
		context.Background(), "server",
		WithKind(SpanKindServer),
		WithParent(wireTrace, wireParent),
	)
	defer span.End()
	if span.TraceID() != wireTrace {
		t.Errorf("trace_id = %q, want %q", span.TraceID(), wireTrace)
	}
	if span.ParentSpanID() != wireParent {
		t.Errorf("parent_span_id = %q, want %q", span.ParentSpanID(), wireParent)
	}
	if span.Kind() != SpanKindServer {
		t.Errorf("kind = %v, want server", span.Kind())
	}
}

func TestRecordErrorSetsStatusAndEvent(t *testing.T) {
	tr := New("test")
	_, span := tr.Start(context.Background(), "op")
	myErr := errors.New("boom")
	got := span.RecordError(myErr)
	if !errors.Is(got, myErr) {
		t.Errorf("RecordError should return its argument")
	}
	span.End()
	snap := tr.Snapshot()
	if snap[0].status != StatusError {
		t.Errorf("status = %v, want Error", snap[0].status)
	}
	if snap[0].statusMsg != "boom" {
		t.Errorf("statusMsg = %q, want boom", snap[0].statusMsg)
	}
	if len(snap[0].events) != 1 || snap[0].events[0].Name != "exception" {
		t.Errorf("expected one exception event, got %+v", snap[0].events)
	}
}

func TestSetAttributeAndAddEvent(t *testing.T) {
	tr := New("test")
	_, span := tr.Start(context.Background(), "op", WithAttributes(KV{Key: "k0", Value: "v0"}))
	span.SetAttribute("k1", "v1")
	span.AddEvent("checkpoint", KV{Key: "where", Value: "mid"})
	span.End()
	snap := tr.Snapshot()
	if len(snap[0].attrs) != 2 || snap[0].attrs[1].Key != "k1" {
		t.Errorf("attrs = %+v, want k0+k1", snap[0].attrs)
	}
	if len(snap[0].events) != 1 || snap[0].events[0].Name != "checkpoint" {
		t.Errorf("events = %+v, want checkpoint", snap[0].events)
	}
}

func TestSetStatusUnsetIsNoOp(t *testing.T) {
	tr := New("test")
	_, span := tr.Start(context.Background(), "op")
	span.SetStatus(StatusError, "explicit error")
	span.SetStatus(StatusUnset, "should be ignored")
	span.End()
	snap := tr.Snapshot()
	if snap[0].status != StatusError {
		t.Errorf("status = %v, want Error", snap[0].status)
	}
	if snap[0].statusMsg != "explicit error" {
		t.Errorf("statusMsg = %q", snap[0].statusMsg)
	}
}

func TestNilSpanMethodsAreNoops(t *testing.T) {
	var s *Span
	s.SetAttribute("k", "v")
	s.AddEvent("e")
	s.SetStatus(StatusError, "x")
	if got := s.RecordError(errors.New("x")); got == nil || got.Error() != "x" {
		t.Errorf("RecordError on nil should return its argument")
	}
	if got := s.RecordError(nil); got != nil { //nolint:staticcheck // nil check is the point
		t.Errorf("RecordError(nil, nil) = %v, want nil", got)
	}
	s.End()
	if s.TraceID() != "" || s.SpanID() != "" || s.Kind() != SpanKindInternal || s.Component() != "" {
		t.Errorf("nil-span accessors should return zero values")
	}
}

func TestSpanFromContextOnEmptyContext(t *testing.T) {
	if got := SpanFromContext(context.Background()); got != nil {
		t.Errorf("SpanFromContext on empty ctx = %v, want nil", got)
	}
	var nilCtx context.Context
	if got := SpanFromContext(nilCtx); got != nil {
		t.Errorf("SpanFromContext(nil) = %v, want nil", got)
	}
}

func TestRecordErrorPackageFunc(t *testing.T) {
	tr := New("test")
	ctx, span := tr.Start(context.Background(), "op")
	defer span.End()
	myErr := errors.New("propagated")
	if got := RecordError(ctx, myErr); !errors.Is(got, myErr) {
		t.Errorf("RecordError pkg func should return its argument")
	}
	// no-span ctx is a silent no-op
	if got := RecordError(context.Background(), myErr); !errors.Is(got, myErr) {
		t.Errorf("RecordError on contextless ctx should still return err")
	}
}

func TestErrorf(t *testing.T) {
	tr := New("test")
	ctx, span := tr.Start(context.Background(), "op")
	err := Errorf(ctx, "wrapped: %d", 42)
	span.End()
	if err.Error() != "wrapped: 42" {
		t.Errorf("Errorf = %q", err.Error())
	}
	snap := tr.Snapshot()
	if snap[0].status != StatusError {
		t.Errorf("status = %v, want Error", snap[0].status)
	}
}

func TestRingBufferWrap(t *testing.T) {
	tr := NewWithCapacity("test", 3)
	for i := range 5 {
		_, s := tr.Start(context.Background(), "op")
		s.SetAttribute("i", string(rune('0'+i)))
		s.End()
	}
	snap := tr.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot len = %d, want 3", len(snap))
	}
	// Oldest two evicted; newest three retained in chronological order.
	got := []string{snap[0].attrs[0].Value, snap[1].attrs[0].Value, snap[2].attrs[0].Value}
	want := []string{"2", "3", "4"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("snap[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSnapshotEmpty(t *testing.T) {
	tr := New("test")
	if got := tr.Snapshot(); got != nil {
		t.Errorf("empty Snapshot = %+v, want nil", got)
	}
}

func TestSnapshotNotFullDoesNotWrap(t *testing.T) {
	tr := NewWithCapacity("test", 5)
	for range 3 {
		_, s := tr.Start(context.Background(), "op")
		s.End()
	}
	if got := tr.Snapshot(); len(got) != 3 {
		t.Errorf("Snapshot len = %d, want 3", len(got))
	}
}

func TestEndIsIdempotent(t *testing.T) {
	tr := New("test")
	_, span := tr.Start(context.Background(), "op")
	span.End()
	span.End()
	if got := tr.Snapshot(); len(got) != 1 {
		t.Errorf("Snapshot len = %d, want 1 after double End", len(got))
	}
}

func TestReaperAbandonsPastDeadline(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := &mockClock{now: now}
	tr := New("test", WithClock(clock.Now))
	_, span := tr.Start(context.Background(), "long",
		WithSpanDeadline(time.Second))
	clock.advance(2 * time.Second)
	tr.reapOnce(clock.Now())
	snap := tr.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot len = %d, want 1 after reap", len(snap))
	}
	if snap[0].status != StatusAbandoned {
		t.Errorf("status = %v, want Abandoned", snap[0].status)
	}
	if snap[0].statusMsg == "" {
		t.Error("statusMsg should be set on abandoned span")
	}
	// Late End is a no-op.
	span.End()
	if got := tr.Snapshot(); len(got) != 1 {
		t.Errorf("Snapshot len = %d after late End, want 1", len(got))
	}
}

func TestShutdownAbandonsActive(t *testing.T) {
	tr := New("test")
	_, _ = tr.Start(context.Background(), "leaked-1")
	_, _ = tr.Start(context.Background(), "leaked-2")
	if err := tr.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	snap := tr.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot len = %d, want 2", len(snap))
	}
	for _, s := range snap {
		if s.status != StatusAbandoned {
			t.Errorf("span %q status = %v, want Abandoned", s.name, s.status)
		}
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	tr := New("test", WithReapInterval(10*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		tr.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunWithDisabledReaperWaitsForCancel(t *testing.T) {
	tr := New("test", WithReapInterval(0))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		tr.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestProtoRoundTrip(t *testing.T) {
	tr := New("test", WithClock(time.Now))
	ctx, span := tr.Start(
		context.Background(), "rpc",
		WithKind(SpanKindClient),
		WithAttributes(KV{Key: "rpc.method", Value: "Ping"}),
	)
	span.AddEvent("dispatched", KV{Key: "step", Value: "send"})
	span.SetStatus(StatusOK, "")
	_ = ctx
	span.End()
	snap := tr.Snapshot()
	pb := snap[0].toProto()
	if pb.GetTraceId() != span.TraceID() {
		t.Errorf("proto trace_id mismatch")
	}
	tr2 := New("ingest")
	tr2.IngestProto([]*tracev1.Span{pb})
	got := tr2.Snapshot()
	if len(got) != 1 {
		t.Fatalf("ingested len = %d", len(got))
	}
	if got[0].name != "rpc" || got[0].kind != SpanKindClient {
		t.Errorf("ingested span shape = %+v", got[0])
	}
	if len(got[0].attrs) != 1 || got[0].attrs[0].Key != "rpc.method" {
		t.Errorf("ingested attrs = %+v", got[0].attrs)
	}
	if len(got[0].events) != 1 || got[0].events[0].Name != "dispatched" {
		t.Errorf("ingested events = %+v", got[0].events)
	}
}

func TestProtoZeroEnd(t *testing.T) {
	// Round-trip a span whose end_unix_nano is zero (never ended).
	pb := &tracev1.Span{
		TraceId:       strings.Repeat("a", 32),
		SpanId:        strings.Repeat("b", 16),
		Name:          "open",
		StartUnixNano: 100,
		EndUnixNano:   0,
		Status:        tracev1.SpanStatus_SPAN_STATUS_UNSET,
	}
	tr := New("ingest")
	tr.IngestProto([]*tracev1.Span{pb})
	got := tr.Snapshot()
	if !got[0].end.IsZero() {
		t.Errorf("end = %v, want zero", got[0].end)
	}
}

func TestFlushExportsAndClears(t *testing.T) {
	exp := &captureExporter{}
	tr := New("test", WithExporter(exp))
	for range 3 {
		_, s := tr.Start(context.Background(), "op")
		s.End()
	}
	if err := tr.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := len(exp.spans); got != 3 {
		t.Errorf("exporter received %d spans, want 3", got)
	}
	if got := tr.Snapshot(); got != nil {
		t.Errorf("buffer not cleared after Flush: %+v", got)
	}
}

func TestFlushNoExporter(t *testing.T) {
	tr := New("test")
	_, s := tr.Start(context.Background(), "op")
	s.End()
	if err := tr.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := tr.Snapshot(); len(got) != 1 {
		t.Errorf("buffer cleared without exporter; len = %d", len(got))
	}
}

func TestFlushEmptyBuffer(t *testing.T) {
	exp := &captureExporter{}
	tr := New("test", WithExporter(exp))
	if err := tr.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if exp.calls != 0 {
		t.Errorf("Export called on empty buffer: %d times", exp.calls)
	}
}

func TestIDValidation(t *testing.T) {
	cases := []struct {
		name string
		fn   func(string) bool
		in   string
		want bool
	}{
		{"valid trace", IsValidTraceID, strings.Repeat("a", 32), true},
		{"trace too short", IsValidTraceID, strings.Repeat("a", 31), false},
		{"trace uppercase", IsValidTraceID, strings.Repeat("A", 32), false},
		{"trace non-hex", IsValidTraceID, strings.Repeat("g", 32), false},
		{"valid span", IsValidSpanID, strings.Repeat("a", 16), true},
		{"span empty", IsValidSpanID, "", false},
		{"span has space", IsValidSpanID, "0123456789abcde ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fn(tc.in); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewWithCapacityZeroFallsBack(t *testing.T) {
	tr := NewWithCapacity("test", 0)
	if tr.capacity != DefaultCapacity {
		t.Errorf("capacity = %d, want %d", tr.capacity, DefaultCapacity)
	}
	tr2 := NewWithCapacity("test", -5)
	if tr2.capacity != DefaultCapacity {
		t.Errorf("capacity = %d, want %d", tr2.capacity, DefaultCapacity)
	}
}

func TestComponentField(t *testing.T) {
	tr := New("daft-orchestrator")
	if tr.Component() != "daft-orchestrator" {
		t.Errorf("Component = %q", tr.Component())
	}
	_, s := tr.Start(context.Background(), "op")
	if s.Component() != "daft-orchestrator" {
		t.Errorf("span Component = %q", s.Component())
	}
}

func TestConcurrentSpansSafe(t *testing.T) {
	tr := NewWithCapacity("test", 1024)
	var wg sync.WaitGroup
	wg.Add(32)
	for range 32 {
		go func() {
			defer wg.Done()
			ctx, root := tr.Start(context.Background(), "root")
			_, child := tr.Start(ctx, "child")
			child.SetAttribute("a", "b")
			child.End()
			root.End()
		}()
	}
	wg.Wait()
	if got := len(tr.Snapshot()); got != 64 {
		t.Errorf("Snapshot len = %d, want 64", got)
	}
}

// captureExporter records every Export call for assertion. nil-safe
// — its zero value is usable.
type captureExporter struct {
	mu    sync.Mutex
	spans []*tracev1.Span
	calls int
	err   error
}

func (e *captureExporter) Export(_ context.Context, spans []*tracev1.Span) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	e.spans = append(e.spans, spans...)
	return e.err
}

type mockClock struct {
	mu  sync.Mutex
	now time.Time
}

func (m *mockClock) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now
}

func (m *mockClock) advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = m.now.Add(d)
}

func TestFormatTextEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := FormatText(&buf, nil); err != nil {
		t.Fatalf("FormatText: %v", err)
	}
	if !strings.Contains(buf.String(), "no spans") {
		t.Errorf("output = %q", buf.String())
	}
}

func TestFormatTextTreeShape(t *testing.T) {
	tr := New("daft")
	ctx, root := tr.Start(context.Background(), "root.op")
	_, child := tr.Start(ctx, "child.op", WithKind(SpanKindClient))
	child.SetAttribute("rpc.method", "Ping")
	child.AddEvent("dispatched")
	child.End()
	root.End()
	var buf bytes.Buffer
	if err := FormatText(&buf, tr.Snapshot()); err != nil {
		t.Fatalf("FormatText: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"root.op", "child.op", "rpc.method=\"Ping\"", "dispatched", "trace "} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestFormatTextOrphanRoot(t *testing.T) {
	// A span with parent_span_id pointing at a span not in the
	// snapshot should render as a root, not silently drop.
	tr := New("daft")
	_, span := tr.Start(
		context.Background(), "orphan",
		WithParent(strings.Repeat("a", 32), strings.Repeat("b", 16)),
	)
	span.End()
	var buf bytes.Buffer
	if err := FormatText(&buf, tr.Snapshot()); err != nil {
		t.Fatalf("FormatText: %v", err)
	}
	if !strings.Contains(buf.String(), "orphan") {
		t.Errorf("orphan span missing from output:\n%s", buf.String())
	}
}

func TestFormatTextErrorStatus(t *testing.T) {
	tr := New("daft")
	_, s := tr.Start(context.Background(), "fail")
	_ = s.RecordError(errors.New("kaboom"))
	s.End()
	var buf bytes.Buffer
	if err := FormatText(&buf, tr.Snapshot()); err != nil {
		t.Fatalf("FormatText: %v", err)
	}
	if !strings.Contains(buf.String(), "ERR") || !strings.Contains(buf.String(), "kaboom") {
		t.Errorf("error status missing:\n%s", buf.String())
	}
}

type fakeTB struct {
	failed bool
	logs   []string
	cleans []func()
}

func (f *fakeTB) Cleanup(fn func())               { f.cleans = append(f.cleans, fn) }
func (f *fakeTB) Failed() bool                    { return f.failed }
func (f *fakeTB) Logf(format string, args ...any) { f.logs = append(f.logs, format) }

func TestLogOnFailureRunsOnFailure(t *testing.T) {
	tr := New("test")
	_, s := tr.Start(context.Background(), "op")
	s.End()
	tb := &fakeTB{failed: true}
	LogOnFailure(tb, tr)
	for _, fn := range tb.cleans {
		fn()
	}
	if len(tb.logs) == 0 {
		t.Errorf("expected dump on failure, got no logs")
	}
}

func TestLogOnFailureSkipsOnPass(t *testing.T) {
	tr := New("test")
	_, s := tr.Start(context.Background(), "op")
	s.End()
	tb := &fakeTB{failed: false}
	LogOnFailure(tb, tr)
	for _, fn := range tb.cleans {
		fn()
	}
	if len(tb.logs) != 0 {
		t.Errorf("unexpected dump on pass: %v", tb.logs)
	}
}

func TestLogOnFailureNilArgsAreNoop(t *testing.T) {
	LogOnFailure(nil, nil)
	tb := &fakeTB{failed: true}
	LogOnFailure(tb, nil)
	if len(tb.cleans) != 0 {
		t.Errorf("unexpected cleanup registration on nil tracer")
	}
}

func TestLogOnFailureAbandonsOpenSpans(t *testing.T) {
	tr := New("test")
	_, _ = tr.Start(context.Background(), "leaked")
	tb := &fakeTB{failed: true}
	LogOnFailure(tb, tr)
	for _, fn := range tb.cleans {
		fn()
	}
	if len(tb.logs) == 0 {
		t.Fatal("expected dump")
	}
	// after Cleanup the leaked span should appear in the snapshot
	// as abandoned
	snap := tr.Snapshot()
	if len(snap) != 1 || snap[0].status != StatusAbandoned {
		t.Errorf("leaked span not abandoned; snapshot = %+v", snap)
	}
}
