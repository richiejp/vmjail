package trace

import (
	"context"
	"strings"
	"testing"
	"time"

	tracev1 "github.com/richiejp/vmjail/internal/genpb/trace/v1"
)

// ingressFixture is shared scaffolding: a tracer with an injected
// clock, a reaper that targets it with the same clock + a
// deterministic span ID generator. Returns advance() so individual
// tests can move the clock forward without rewriting the lambdas.
type ingressFixture struct {
	now     time.Time
	tracer  *Tracer
	reaper  *IngressReaper
	advance func(time.Duration)
}

func newIngressFixture() *ingressFixture {
	f := &ingressFixture{
		now: time.Unix(1_700_000_000, 0).UTC(),
	}
	f.tracer = New("orchestrator", WithClock(func() time.Time { return f.now }))
	f.reaper = NewIngressReaper(
		f.tracer, "orchestrator",
		WithIngressClock(func() time.Time { return f.now }),
		WithIngressQuietAfter(2*time.Minute),
		WithIngressScanInterval(0),
	)
	f.advance = func(d time.Duration) { f.now = f.now.Add(d) }
	return f
}

// makeProtoSpan builds a wire-format span with the controllable
// fields we care about for reaper tests. Times are seeded from the
// fixture's clock so the maxEnd computation lines up with the
// quiet-after logic.
func makeProtoSpan(traceID, spanID, parentID string, end time.Time) *tracev1.Span {
	return &tracev1.Span{
		TraceId:       traceID,
		SpanId:        spanID,
		ParentSpanId:  parentID,
		Name:          "op",
		Kind:          tracev1.SpanKind_SPAN_KIND_SERVER,
		Status:        tracev1.SpanStatus_SPAN_STATUS_OK,
		StartUnixNano: end.Add(-time.Second).UnixNano(),
		EndUnixNano:   end.UnixNano(),
		Component:     "producer",
	}
}

func TestIngressReaperAbandonsQuietTraceWithoutRoot(t *testing.T) {
	f := newIngressFixture()
	traceID := strings.Repeat("c", 32)
	childSpanID := strings.Repeat("d", 16)

	// One child span lands. Its parent_span_id is non-empty, so
	// no root has arrived yet.
	f.tracer.IngestProto([]*tracev1.Span{
		makeProtoSpan(traceID, childSpanID, strings.Repeat("e", 16), f.now),
	})
	f.advance(3 * time.Minute) // past quietAfter (2m)

	f.reaper.scanOnce()

	got := findQuiescedMarker(t, f.tracer, traceID)
	if got.Status() != StatusAbandoned {
		t.Errorf("marker status = %v, want ABANDONED (no root span ever seen)", got.Status())
	}
	wantMsg := "producer likely crashed"
	if !strings.Contains(got.StatusMessage(), wantMsg) {
		t.Errorf("marker StatusMessage = %q, want contains %q", got.StatusMessage(), wantMsg)
	}
}

func TestIngressReaperOKMarkerWhenRootIsPresent(t *testing.T) {
	f := newIngressFixture()
	traceID := strings.Repeat("c", 32)

	// A root (parent_span_id == "") plus one child both landed.
	f.tracer.IngestProto([]*tracev1.Span{
		makeProtoSpan(traceID, strings.Repeat("d", 16), "", f.now),
		makeProtoSpan(traceID, strings.Repeat("e", 16), strings.Repeat("d", 16), f.now),
	})
	f.advance(3 * time.Minute)

	f.reaper.scanOnce()

	got := findQuiescedMarker(t, f.tracer, traceID)
	if got.Status() != StatusOK {
		t.Errorf("marker status = %v, want OK (root span was present)", got.Status())
	}
	if got.ParentSpanID() != "" {
		t.Errorf("marker parent_span_id = %q, want empty (marker is itself a root)", got.ParentSpanID())
	}
}

func TestIngressReaperDoesNotRemarkInLaterScan(t *testing.T) {
	f := newIngressFixture()
	traceID := strings.Repeat("c", 32)
	f.tracer.IngestProto([]*tracev1.Span{
		makeProtoSpan(traceID, strings.Repeat("d", 16), strings.Repeat("e", 16), f.now),
	})
	f.advance(3 * time.Minute)

	f.reaper.scanOnce()
	f.reaper.scanOnce()
	f.reaper.scanOnce()

	count := 0
	for _, s := range f.tracer.Snapshot() {
		if s.Name() == QuiescedSpanName && s.TraceID() == traceID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("marker count = %d, want 1 (reaper must not re-mark a trace_id it already markered)", count)
	}
}

func TestIngressReaperSkipsTracesStillInQuietWindow(t *testing.T) {
	f := newIngressFixture()
	traceID := strings.Repeat("c", 32)
	f.tracer.IngestProto([]*tracev1.Span{
		makeProtoSpan(traceID, strings.Repeat("d", 16), strings.Repeat("e", 16), f.now),
	})
	// Only 30 seconds passes — well within the 2-minute quiet window.
	f.advance(30 * time.Second)

	f.reaper.scanOnce()

	for _, s := range f.tracer.Snapshot() {
		if s.Name() == QuiescedSpanName {
			t.Errorf("marker emitted too early: %+v", s)
		}
	}
}

func TestIngressReaperRefreshesOnNewSpans(t *testing.T) {
	f := newIngressFixture()
	traceID := strings.Repeat("c", 32)

	// First span lands at t=0.
	f.tracer.IngestProto([]*tracev1.Span{
		makeProtoSpan(traceID, strings.Repeat("d", 16), strings.Repeat("e", 16), f.now),
	})

	// 90s passes (under the 2m window), a fresh span lands at t=90s.
	f.advance(90 * time.Second)
	f.tracer.IngestProto([]*tracev1.Span{
		makeProtoSpan(traceID, strings.Repeat("f", 16), strings.Repeat("e", 16), f.now),
	})

	// Now another 90s. Cumulative is 3m from t=0, but only 90s from
	// the most recent span — still inside the window.
	f.advance(90 * time.Second)
	f.reaper.scanOnce()

	for _, s := range f.tracer.Snapshot() {
		if s.Name() == QuiescedSpanName {
			t.Errorf("marker emitted despite fresh activity: %+v", s)
		}
	}
}

func TestIngressReaperRunStopsOnContextCancel(t *testing.T) {
	f := newIngressFixture()
	// A long scan interval would normally block; set explicitly to
	// confirm ctx.Done() wins over the timer path.
	f.reaper.scanInterval = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.reaper.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after context cancel")
	}
}

func TestIngressReaperRunDisabledIntervalBlocksUntilCancel(t *testing.T) {
	f := newIngressFixture()
	f.reaper.scanInterval = 0 // disabled scan path

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.reaper.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Run returned without cancel; scanInterval=0 should block on ctx")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after cancel with scanInterval=0")
	}
}

// TestNewAccessorsNilSafe pins the nil-receiver contract on the
// accessors added for the ingress reaper. The reaper itself never
// passes nil spans (Snapshot can't return them), but the public
// surface promises nil-safety on every Span method.
func TestNewAccessorsNilSafe(t *testing.T) {
	var s *Span
	if got := s.EndTime(); !got.IsZero() {
		t.Errorf("nil EndTime = %v, want zero", got)
	}
	if got := s.Status(); got != StatusUnset {
		t.Errorf("nil Status = %v, want StatusUnset", got)
	}
	if got := s.StatusMessage(); got != "" {
		t.Errorf("nil StatusMessage = %q, want empty", got)
	}
}

// findQuiescedMarker returns the single quiesced marker span for
// traceID, failing the test if there isn't exactly one.
func findQuiescedMarker(t *testing.T, tr *Tracer, traceID string) *Span {
	t.Helper()
	var found *Span
	for _, s := range tr.Snapshot() {
		if s.Name() == QuiescedSpanName && s.TraceID() == traceID {
			if found != nil {
				t.Fatalf("duplicate quiesced markers for trace %s", traceID)
			}
			found = s
		}
	}
	if found == nil {
		t.Fatalf("no quiesced marker found for trace %s", traceID)
	}
	return found
}
