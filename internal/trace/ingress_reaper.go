package trace

import (
	"context"
	"strconv"
	"sync"
	"time"

	tracev1 "github.com/richiejp/darkaifactory/internal/genpb/trace/v1"
)

// DefaultIngressQuietAfter is how long a trace_id may go without
// receiving new spans before the ingress reaper marks it quiesced.
// Long enough for a slow cross-component trace (image pull + boot +
// container run) to finish naturally; short enough that an operator
// browsing /debug/traces during an interactive session sees the
// marker for a crashed producer within the same session.
const DefaultIngressQuietAfter = 10 * time.Minute

// DefaultIngressScanInterval is how often IngressReaper.Run sweeps
// the buffer looking for quiet trace_ids.
const DefaultIngressScanInterval = 1 * time.Minute

// QuiescedSpanName is the Name attached to synthetic marker spans
// emitted by IngressReaper. Exported so consumers (tests, the
// /debug/traces renderer, downstream tooling) can filter or
// highlight them without string-matching a literal.
const QuiescedSpanName = "trace.quiesced"

// IngressReaper watches a Tracer that is acting as a span sink
// (typically the orchestrator's central Tracer fed by ReportSpans)
// and emits a synthetic marker span for each trace_id that has gone
// quiet without finishing. The marker's status is ABANDONED when no
// root span (parent_span_id == "") was ever seen for the trace, OK
// otherwise — the assumption being that a crashed producer never
// flushes its root span, so a trace whose children landed but whose
// root never did is almost certainly truncated.
//
// The reaper is read-only against the buffer except for its own
// marker injection via Tracer.IngestProto. It doesn't touch the
// producer-side active-span machinery (Tracer.Run handles that).
type IngressReaper struct {
	tracer       *Tracer
	quietAfter   time.Duration
	scanInterval time.Duration
	nowFn        func() time.Time
	component    string
	randSpanID   func() string

	mu     sync.Mutex
	marked map[string]struct{}
}

// IngressReaperOption configures an IngressReaper.
type IngressReaperOption func(*IngressReaper)

// WithIngressQuietAfter overrides DefaultIngressQuietAfter.
func WithIngressQuietAfter(d time.Duration) IngressReaperOption {
	return func(r *IngressReaper) { r.quietAfter = d }
}

// WithIngressScanInterval overrides DefaultIngressScanInterval.
// Setting <= 0 disables the periodic scan entirely (Run will block
// on ctx).
func WithIngressScanInterval(d time.Duration) IngressReaperOption {
	return func(r *IngressReaper) { r.scanInterval = d }
}

// WithIngressClock replaces time.Now. Tests only.
func WithIngressClock(fn func() time.Time) IngressReaperOption {
	return func(r *IngressReaper) { r.nowFn = fn }
}

// WithIngressSpanIDFn replaces the random span ID generator. Tests
// only — production callers want crypto/rand for marker IDs the
// same as any other span.
func WithIngressSpanIDFn(fn func() string) IngressReaperOption {
	return func(r *IngressReaper) { r.randSpanID = fn }
}

// NewIngressReaper builds a reaper for tr. component labels the
// synthetic marker spans so they're attributable to whoever ran the
// reaper (typically the orchestrator).
func NewIngressReaper(tr *Tracer, component string, opts ...IngressReaperOption) *IngressReaper {
	r := &IngressReaper{
		tracer:       tr,
		quietAfter:   DefaultIngressQuietAfter,
		scanInterval: DefaultIngressScanInterval,
		nowFn:        time.Now,
		component:    component,
		randSpanID:   newSpanID,
		marked:       map[string]struct{}{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run drives the reaper until ctx is cancelled. Returns when ctx
// is done. Safe to call once per reaper.
func (r *IngressReaper) Run(ctx context.Context) {
	if r.scanInterval <= 0 {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(r.scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.scanOnce()
		}
	}
}

// scanOnce is one sweep over the tracer's snapshot. Exported via
// the Run loop; pulled out so tests can drive deterministically
// without a ticker.
func (r *IngressReaper) scanOnce() {
	now := r.nowFn()
	spans := r.tracer.Snapshot()

	type state struct {
		maxEnd  time.Time
		hasRoot bool
	}
	states := map[string]*state{}
	for _, s := range spans {
		if s.Name() == QuiescedSpanName {
			// Don't re-trigger on our own markers.
			continue
		}
		tid := s.TraceID()
		st := states[tid]
		if st == nil {
			st = &state{}
			states[tid] = st
		}
		if end := s.EndTime(); end.After(st.maxEnd) {
			st.maxEnd = end
		}
		if s.ParentSpanID() == "" {
			st.hasRoot = true
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Emit markers for newly-quiet trace_ids.
	for tid, st := range states {
		if _, already := r.marked[tid]; already {
			continue
		}
		if st.maxEnd.IsZero() {
			continue
		}
		if now.Sub(st.maxEnd) < r.quietAfter {
			continue
		}
		r.tracer.IngestProto([]*tracev1.Span{r.markerFor(tid, st.maxEnd, now, st.hasRoot)})
		r.marked[tid] = struct{}{}
	}

	// Garbage-collect entries whose trace_ids have aged out of the
	// ring buffer — without this the marked set grows unboundedly
	// over the lifetime of the process.
	for tid := range r.marked {
		if _, present := states[tid]; !present {
			delete(r.marked, tid)
		}
	}
}

func (r *IngressReaper) markerFor(traceID string, lastSeen, now time.Time, hasRoot bool) *tracev1.Span {
	status := tracev1.SpanStatus_SPAN_STATUS_ABANDONED
	msg := "no spans received within quiet_after; producer likely crashed mid-trace"
	if hasRoot {
		status = tracev1.SpanStatus_SPAN_STATUS_OK
		msg = "trace went quiet after root span finished"
	}
	return &tracev1.Span{
		TraceId:       traceID,
		SpanId:        r.randSpanID(),
		ParentSpanId:  "",
		Name:          QuiescedSpanName,
		Kind:          tracev1.SpanKind_SPAN_KIND_INTERNAL,
		Status:        status,
		StatusMessage: msg,
		StartUnixNano: lastSeen.UnixNano(),
		EndUnixNano:   now.UnixNano(),
		Component:     r.component,
		Attributes: []*tracev1.KeyValue{
			{Key: "last_seen_unix_nano", Value: strconv.FormatInt(lastSeen.UnixNano(), 10)},
			{Key: "had_root", Value: strconv.FormatBool(hasRoot)},
		},
	}
}
