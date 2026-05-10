// Package trace is daft's cross-component tracing system. Each
// component (daft client, daft-orchestrator, daft-sandboxd, the
// in-VM agent) runs one Tracer; spans accumulate in a fixed-capacity
// in-memory ring buffer and are shipped to the orchestrator via
// TraceService when an Exporter is configured.
//
// The model is OpenTelemetry-shaped (trace_id + span_id + parent
// span_id, KeyValue attributes, span events) but with one important
// extension: spans whose End never arrives within a deadline are
// emitted with StatusAbandoned rather than silently dropped. That
// makes a crashed or buggy producer visible to the consumer, which
// is what we want for components that can die without warning.
//
// All Span methods are nil-safe so callers can write
// `trace.SpanFromContext(ctx).RecordError(err)` without checking
// for a missing span first. Spans are not safe for concurrent
// mutation from multiple goroutines without external synchronisation
// — the typical pattern is one goroutine per span.
package trace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	tracev1 "github.com/richiejp/darkaifactory/internal/genpb/trace/v1"
)

// DefaultCapacity sizes the ring buffer when New is called without
// an explicit capacity. ~1k spans is plenty for an interactive
// session on a producer; orchestrator-side buffers should override
// via NewWithCapacity.
const DefaultCapacity = 1024

// DefaultSpanDeadline is how long a span may stay open before the
// reaper marks it abandoned. Long enough that a slow image pull
// doesn't get spurious abandonments, short enough that a forgotten
// span surfaces within an interactive session.
const DefaultSpanDeadline = 10 * time.Minute

// DefaultReapInterval is how often Run scans the active set for
// past-deadline spans. We don't care about millisecond precision
// on abandonment.
const DefaultReapInterval = 30 * time.Second

// SpanKind mirrors the proto enum; aliased into the package so
// callers don't need to import the generated types.
type SpanKind int8

const (
	SpanKindInternal SpanKind = iota
	SpanKindClient
	SpanKindServer
)

// String returns the lowercase OTel-compatible name of the kind
// ("client" / "server" / "internal"). Used by the orchestrator's
// JSON debug endpoint and any other human-readable surface.
func (k SpanKind) String() string {
	switch k {
	case SpanKindClient:
		return "client"
	case SpanKindServer:
		return "server"
	default:
		return "internal"
	}
}

// SpanStatus mirrors the proto enum. StatusAbandoned is set by the
// reaper or by Shutdown when a span never received End.
type SpanStatus int8

const (
	StatusUnset SpanStatus = iota
	StatusOK
	StatusError
	StatusAbandoned
)

// String returns the upper-case form used in human dumps
// ("OK" / "ERR" / "ABANDONED" / "UNSET").
func (s SpanStatus) String() string {
	switch s {
	case StatusOK:
		return "OK"
	case StatusError:
		return "ERR"
	case StatusAbandoned:
		return "ABANDONED"
	default:
		return "UNSET"
	}
}

// KV is one attribute. Wire format is string-only; producers
// stringify numeric/bool values at the call site.
type KV struct {
	Key   string
	Value string
}

// Event is a timestamped marker inside a span (logs, errors,
// lifecycle).
type Event struct {
	Name  string
	Time  time.Time
	Attrs []KV
}

// Span is one unit of work. Producers call End once and Set/Add/
// RecordError as desired; consumers read via Tracer.Snapshot.
//
// Mutable fields are guarded by Tracer.mu — Span methods take that
// mutex briefly. Immutable fields (the IDs, name, kind, component,
// start) are set at Start time and never change.
type Span struct {
	tracer    *Tracer
	traceID   string
	spanID    string
	parentID  string
	name      string
	kind      SpanKind
	component string

	status    SpanStatus
	statusMsg string
	start     time.Time
	end       time.Time
	deadline  time.Time
	attrs     []KV
	events    []Event
	finished  bool
}

// TraceID, SpanID, ParentSpanID, Name, Kind, Component are accessors
// for callers that need to inspect a span without mutating it
// (interceptors, tests, the proto exporter). Safe on a nil Span.
func (s *Span) TraceID() string      { return safeStr(s, func(s *Span) string { return s.traceID }) }
func (s *Span) SpanID() string       { return safeStr(s, func(s *Span) string { return s.spanID }) }
func (s *Span) ParentSpanID() string { return safeStr(s, func(s *Span) string { return s.parentID }) }
func (s *Span) Name() string         { return safeStr(s, func(s *Span) string { return s.name }) }
func (s *Span) Kind() SpanKind {
	if s == nil {
		return SpanKindInternal
	}
	return s.kind
}
func (s *Span) Component() string { return safeStr(s, func(s *Span) string { return s.component }) }

func safeStr(s *Span, get func(*Span) string) string {
	if s == nil {
		return ""
	}
	return get(s)
}

// SetAttribute records a single attribute on the span. No-op on a
// nil span or after End.
func (s *Span) SetAttribute(key, value string) {
	if s == nil {
		return
	}
	s.tracer.mu.Lock()
	defer s.tracer.mu.Unlock()
	if s.finished {
		return
	}
	s.attrs = append(s.attrs, KV{Key: key, Value: value})
}

// SetStatus sets the span's outcome. StatusUnset is a sentinel —
// passing it explicitly is a no-op so callers can use a single
// SetStatus call site without conditionals. End auto-promotes
// StatusUnset to StatusOK at finish time.
func (s *Span) SetStatus(code SpanStatus, msg string) {
	if s == nil || code == StatusUnset {
		return
	}
	s.tracer.mu.Lock()
	defer s.tracer.mu.Unlock()
	if s.finished {
		return
	}
	s.status = code
	s.statusMsg = msg
}

// AddEvent appends a span event with the current time.
func (s *Span) AddEvent(name string, attrs ...KV) {
	if s == nil {
		return
	}
	s.tracer.mu.Lock()
	defer s.tracer.mu.Unlock()
	if s.finished {
		return
	}
	s.events = append(s.events, Event{
		Name:  name,
		Time:  s.tracer.nowFn(),
		Attrs: append([]KV(nil), attrs...),
	})
}

// RecordError records err as an "exception" event and (if status
// is still unset) marks the span as Error. Returns err unchanged so
// callers can write `return span.RecordError(err)` inline. Safe on
// a nil span and a nil err.
func (s *Span) RecordError(err error) error {
	if s == nil || err == nil {
		return err
	}
	s.tracer.mu.Lock()
	defer s.tracer.mu.Unlock()
	if s.finished {
		return err
	}
	if s.status == StatusUnset {
		s.status = StatusError
		s.statusMsg = err.Error()
	}
	s.events = append(s.events, Event{
		Name:  "exception",
		Time:  s.tracer.nowFn(),
		Attrs: []KV{{Key: "exception.message", Value: err.Error()}},
	})
	return err
}

// End finalises the span and moves it from the active set into the
// ring buffer. Calling End twice is a no-op.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.tracer.endSpan(s, s.tracer.nowFn(), false)
}

// Tracer is the per-component span lifecycle manager and ring
// buffer. One per component; shared across goroutines.
type Tracer struct {
	component    string
	capacity     int
	nowFn        func() time.Time
	reapInterval time.Duration
	exporter     Exporter

	mu     sync.Mutex
	buf    []*Span
	head   int
	full   bool
	active map[string]*Span
}

// Option configures a Tracer at construction time.
type Option func(*Tracer)

// WithExporter attaches an Exporter that Flush ships finished spans
// to. Without one, Flush is a no-op and the buffer is observable
// only via Snapshot.
func WithExporter(e Exporter) Option { return func(t *Tracer) { t.exporter = e } }

// WithReapInterval overrides DefaultReapInterval (used by Run).
// Setting <= 0 disables the reaper entirely.
func WithReapInterval(d time.Duration) Option { return func(t *Tracer) { t.reapInterval = d } }

// WithClock replaces time.Now. Tests only.
func WithClock(fn func() time.Time) Option { return func(t *Tracer) { t.nowFn = fn } }

// New returns a Tracer with default capacity. component identifies
// the producer in emitted spans (e.g. "daft", "daft-orchestrator").
func New(component string, opts ...Option) *Tracer {
	return NewWithCapacity(component, DefaultCapacity, opts...)
}

// NewWithCapacity returns a Tracer with an explicit ring-buffer
// size. capacity <= 0 falls back to DefaultCapacity.
func NewWithCapacity(component string, capacity int, opts ...Option) *Tracer {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	t := &Tracer{
		component:    component,
		capacity:     capacity,
		nowFn:        time.Now,
		reapInterval: DefaultReapInterval,
		buf:          make([]*Span, capacity),
		active:       map[string]*Span{},
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

// Component is the identifier passed to New.
func (t *Tracer) Component() string { return t.component }

// SetExporter replaces the tracer's exporter. The daft CLI uses this
// to wire the exporter after dialing the orchestrator (the
// chicken-and-egg of "tracer needs an exporter, exporter needs a
// client, client needs the same TLS config the tracer's interceptor
// is going to wrap"). Safe to call concurrently.
func (t *Tracer) SetExporter(e Exporter) {
	t.mu.Lock()
	t.exporter = e
	t.mu.Unlock()
}

// SpanOption configures a span at start time.
type SpanOption func(*spanOpts)

type spanOpts struct {
	kind     SpanKind
	deadline time.Duration
	parentID string
	traceID  string
	attrs    []KV
}

// WithKind tags the span as client, server, or internal (default).
func WithKind(k SpanKind) SpanOption { return func(o *spanOpts) { o.kind = k } }

// WithSpanDeadline overrides DefaultSpanDeadline for one span.
func WithSpanDeadline(d time.Duration) SpanOption {
	return func(o *spanOpts) { o.deadline = d }
}

// WithParent forces a specific (trace, parent-span) pair. Used by
// the server interceptor to inherit a wire-propagated trace context;
// in normal code, parentage flows implicitly via context.
func WithParent(traceID, parentSpanID string) SpanOption {
	return func(o *spanOpts) {
		o.traceID = traceID
		o.parentID = parentSpanID
	}
}

// WithAttributes seeds the span's attribute list at Start.
func WithAttributes(kv ...KV) SpanOption {
	return func(o *spanOpts) { o.attrs = append(o.attrs, kv...) }
}

// Start is the free-function form of Tracer.Start. It locates a
// Tracer via TracerFromContext, falling back to the active Span's
// Tracer when present. With no Tracer reachable from ctx, returns
// ctx unchanged and a nil Span — the nil-safe Span methods make
// instrumentation calls safe to leave in place even when the caller
// never wired a tracer (typical for library code).
func Start(ctx context.Context, name string, opts ...SpanOption) (context.Context, *Span) {
	tr := TracerFromContext(ctx)
	if tr == nil {
		if parent := SpanFromContext(ctx); parent != nil {
			tr = parent.tracer
		}
	}
	if tr == nil {
		return ctx, nil
	}
	return tr.Start(ctx, name, opts...)
}

// Start opens a new span, attaches it to ctx, and returns the
// updated context plus the span. parentage is taken from ctx (the
// span there becomes the parent); WithParent overrides for
// wire-propagated traces. The returned span MUST be ended (or
// abandoned via Shutdown) — callers typically `defer span.End()`.
func (t *Tracer) Start(ctx context.Context, name string, opts ...SpanOption) (context.Context, *Span) {
	o := spanOpts{deadline: DefaultSpanDeadline}
	for _, opt := range opts {
		opt(&o)
	}
	if o.traceID == "" {
		if parent := SpanFromContext(ctx); parent != nil {
			o.traceID = parent.traceID
			if o.parentID == "" {
				o.parentID = parent.spanID
			}
		} else {
			o.traceID = newTraceID()
		}
	}
	now := t.nowFn()
	s := &Span{
		tracer:    t,
		traceID:   o.traceID,
		spanID:    newSpanID(),
		parentID:  o.parentID,
		name:      name,
		kind:      o.kind,
		component: t.component,
		start:     now,
		deadline:  now.Add(o.deadline),
		attrs:     append([]KV(nil), o.attrs...),
	}
	t.mu.Lock()
	t.active[s.spanID] = s
	t.mu.Unlock()
	return ContextWithSpan(ctx, s), s
}

// Snapshot returns a copy of finished spans in the ring buffer in
// chronological order. Active spans are not included.
func (t *Tracer) Snapshot() []*Span {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked()
}

func (t *Tracer) snapshotLocked() []*Span {
	if !t.full && t.head == 0 {
		return nil
	}
	if !t.full {
		out := make([]*Span, t.head)
		copy(out, t.buf[:t.head])
		return out
	}
	out := make([]*Span, 0, t.capacity)
	out = append(out, t.buf[t.head:]...)
	out = append(out, t.buf[:t.head]...)
	return out
}

// IngestProto appends spans received from another component to the
// buffer. Used by the orchestrator's TraceService handler. Producer
// Tracers don't call this.
func (t *Tracer) IngestProto(spans []*tracev1.Span) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, ps := range spans {
		t.appendLocked(spanFromProto(t, ps))
	}
}

func (t *Tracer) appendLocked(s *Span) {
	t.buf[t.head] = s
	t.head++
	if t.head == t.capacity {
		t.head = 0
		t.full = true
	}
}

func (t *Tracer) endSpan(s *Span, now time.Time, abandoned bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.finishLocked(s, now, abandoned, "")
}

// finishLocked is the shared "mark finished, move to buffer" path
// used by clean End, the deadline reaper, and Shutdown's abandon-
// all sweep. Caller holds t.mu. abandonedMsg is used only when
// abandoned is true and the span doesn't already have a status
// message.
func (t *Tracer) finishLocked(s *Span, now time.Time, abandoned bool, abandonedMsg string) {
	if s.finished {
		return
	}
	s.finished = true
	s.end = now
	switch {
	case abandoned:
		s.status = StatusAbandoned
		if s.statusMsg == "" {
			s.statusMsg = abandonedMsg
			if s.statusMsg == "" {
				s.statusMsg = "deadline exceeded without End"
			}
		}
	case s.status == StatusUnset:
		s.status = StatusOK
	}
	delete(t.active, s.spanID)
	t.appendLocked(s)
}

// Run drives the abandoned-span reaper until ctx is cancelled.
// Long-running components call this once at startup; one-shot
// CLIs skip it (their spans are reaped by Shutdown instead).
func (t *Tracer) Run(ctx context.Context) {
	if t.reapInterval <= 0 {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(t.reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.reapOnce(t.nowFn())
		}
	}
}

func (t *Tracer) reapOnce(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	dead := make([]*Span, 0)
	for _, s := range t.active {
		if !s.deadline.IsZero() && now.After(s.deadline) {
			dead = append(dead, s)
		}
	}
	for _, s := range dead {
		t.finishLocked(s, now, true, "deadline exceeded without End")
	}
}

// Flush exports buffered finished spans (if an Exporter is wired)
// and clears the buffer afterwards. Safe to call concurrently —
// each call drains a snapshot of the buffer at the moment of call.
func (t *Tracer) Flush(ctx context.Context) error {
	t.mu.Lock()
	exp := t.exporter
	if exp == nil {
		t.mu.Unlock()
		return nil
	}
	spans := t.snapshotLocked()
	for i := range t.buf {
		t.buf[i] = nil
	}
	t.head = 0
	t.full = false
	t.mu.Unlock()
	if len(spans) == 0 {
		return nil
	}
	out := make([]*tracev1.Span, len(spans))
	for i, s := range spans {
		out[i] = s.toProto()
	}
	return exp.Export(ctx, out)
}

// Shutdown abandons every still-active span (because the component
// is going down without End'ing them) and Flushes once. Subsequent
// calls are no-ops.
func (t *Tracer) Shutdown(ctx context.Context) error {
	t.abandonAll(t.nowFn())
	return t.Flush(ctx)
}

func (t *Tracer) abandonAll(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	dead := make([]*Span, 0, len(t.active))
	for _, s := range t.active {
		dead = append(dead, s)
	}
	for _, s := range dead {
		t.finishLocked(s, now, true, "tracer shutdown")
	}
}

// Context plumbing.

type (
	spanCtxKey   struct{}
	tracerCtxKey struct{}
)

// ContextWithSpan returns ctx augmented with span as the active
// span. SpanFromContext on the returned ctx will return span.
func ContextWithSpan(ctx context.Context, s *Span) context.Context {
	return context.WithValue(ctx, spanCtxKey{}, s)
}

// SpanFromContext returns the active span for ctx, or nil if none
// is set. All *Span methods are nil-safe so callers don't need to
// check.
func SpanFromContext(ctx context.Context) *Span {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(spanCtxKey{}).(*Span)
	return s
}

// ContextWithTracer attaches a Tracer to ctx so deeper code can
// pick it up via TracerFromContext. Tests use this to plug in a
// pre-built Tracer (typically one without an exporter) so they can
// inspect spans without shipping them.
func ContextWithTracer(ctx context.Context, t *Tracer) context.Context {
	return context.WithValue(ctx, tracerCtxKey{}, t)
}

// TracerFromContext returns the Tracer attached to ctx, or nil.
func TracerFromContext(ctx context.Context) *Tracer {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(tracerCtxKey{}).(*Tracer)
	return t
}

// RecordError attaches err to the active span in ctx and returns
// err unchanged. No-op when there's no span. Use as
// `return trace.RecordError(ctx, fmt.Errorf("foo: %w", err))`.
func RecordError(ctx context.Context, err error) error {
	return SpanFromContext(ctx).RecordError(err)
}

// Errorf creates an error with fmt.Errorf and attaches it to the
// active span. Returns the new error.
func Errorf(ctx context.Context, format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	_ = SpanFromContext(ctx).RecordError(err)
	return err
}

// IDs.

// newTraceID returns a 16-byte random ID hex-encoded (32 chars).
func newTraceID() string { return randHex(16) }

// newSpanID returns an 8-byte random ID hex-encoded (16 chars).
func newSpanID() string { return randHex(8) }

func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read on Linux is documented infallible.
		// If it ever does fail we'd rather see a loud panic than
		// emit duplicate IDs and silently break the trace tree.
		panic(fmt.Sprintf("trace: rand.Read: %v", err))
	}
	return hex.EncodeToString(buf)
}

// IsValidTraceID reports whether s is a 32-char lowercase hex
// trace ID. Used by the server interceptor to filter out malformed
// wire-propagated values.
func IsValidTraceID(s string) bool { return isValidHexID(s, 32) }

// IsValidSpanID reports whether s is a 16-char lowercase hex
// span ID.
func IsValidSpanID(s string) bool { return isValidHexID(s, 16) }

func isValidHexID(s string, want int) bool {
	if len(s) != want {
		return false
	}
	for i := range len(s) {
		c := s[i]
		isDigit := c >= '0' && c <= '9'
		isHex := c >= 'a' && c <= 'f'
		if !isDigit && !isHex {
			return false
		}
	}
	return true
}
