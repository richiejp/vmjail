package trace

import (
	"time"

	tracev1 "github.com/richiejp/vmjail/internal/genpb/trace/v1"
)

// toProto serialises a Span for the wire. Caller holds no locks —
// toProto is called from Flush after the span has been removed from
// the active set, so its fields are stable.
func (s *Span) toProto() *tracev1.Span {
	out := &tracev1.Span{
		TraceId:       s.traceID,
		SpanId:        s.spanID,
		ParentSpanId:  s.parentID,
		Name:          s.name,
		Kind:          spanKindToProto(s.kind),
		Status:        spanStatusToProto(s.status),
		StatusMessage: s.statusMsg,
		StartUnixNano: s.start.UnixNano(),
		EndUnixNano:   s.end.UnixNano(),
		Component:     s.component,
	}
	if len(s.attrs) > 0 {
		out.Attributes = kvSliceToProto(s.attrs)
	}
	if len(s.events) > 0 {
		out.Events = make([]*tracev1.SpanEvent, len(s.events))
		for i, e := range s.events {
			out.Events[i] = &tracev1.SpanEvent{
				Name:         e.Name,
				TimeUnixNano: e.Time.UnixNano(),
				Attributes:   kvSliceToProto(e.Attrs),
			}
		}
	}
	return out
}

// spanFromProto reconstructs a span from its wire form. Used by
// IngestProto on the orchestrator side.
func spanFromProto(t *Tracer, p *tracev1.Span) *Span {
	s := &Span{
		tracer:    t,
		traceID:   p.GetTraceId(),
		spanID:    p.GetSpanId(),
		parentID:  p.GetParentSpanId(),
		name:      p.GetName(),
		kind:      spanKindFromProto(p.GetKind()),
		component: p.GetComponent(),
		status:    spanStatusFromProto(p.GetStatus()),
		statusMsg: p.GetStatusMessage(),
		start:     unixNanoToTime(p.GetStartUnixNano()),
		end:       unixNanoToTime(p.GetEndUnixNano()),
		finished:  true,
	}
	for _, kv := range p.GetAttributes() {
		s.attrs = append(s.attrs, KV{Key: kv.GetKey(), Value: kv.GetValue()})
	}
	for _, ev := range p.GetEvents() {
		e := Event{
			Name: ev.GetName(),
			Time: unixNanoToTime(ev.GetTimeUnixNano()),
		}
		for _, kv := range ev.GetAttributes() {
			e.Attrs = append(e.Attrs, KV{Key: kv.GetKey(), Value: kv.GetValue()})
		}
		s.events = append(s.events, e)
	}
	return s
}

func kvSliceToProto(in []KV) []*tracev1.KeyValue {
	out := make([]*tracev1.KeyValue, len(in))
	for i, kv := range in {
		out[i] = &tracev1.KeyValue{Key: kv.Key, Value: kv.Value}
	}
	return out
}

func spanKindToProto(k SpanKind) tracev1.SpanKind {
	switch k {
	case SpanKindClient:
		return tracev1.SpanKind_SPAN_KIND_CLIENT
	case SpanKindServer:
		return tracev1.SpanKind_SPAN_KIND_SERVER
	case SpanKindInternal:
		return tracev1.SpanKind_SPAN_KIND_INTERNAL
	default:
		return tracev1.SpanKind_SPAN_KIND_UNSPECIFIED
	}
}

func spanKindFromProto(k tracev1.SpanKind) SpanKind {
	switch k {
	case tracev1.SpanKind_SPAN_KIND_CLIENT:
		return SpanKindClient
	case tracev1.SpanKind_SPAN_KIND_SERVER:
		return SpanKindServer
	default:
		return SpanKindInternal
	}
}

func spanStatusToProto(s SpanStatus) tracev1.SpanStatus {
	switch s {
	case StatusOK:
		return tracev1.SpanStatus_SPAN_STATUS_OK
	case StatusError:
		return tracev1.SpanStatus_SPAN_STATUS_ERROR
	case StatusAbandoned:
		return tracev1.SpanStatus_SPAN_STATUS_ABANDONED
	default:
		return tracev1.SpanStatus_SPAN_STATUS_UNSET
	}
}

func spanStatusFromProto(s tracev1.SpanStatus) SpanStatus {
	switch s {
	case tracev1.SpanStatus_SPAN_STATUS_OK:
		return StatusOK
	case tracev1.SpanStatus_SPAN_STATUS_ERROR:
		return StatusError
	case tracev1.SpanStatus_SPAN_STATUS_ABANDONED:
		return StatusAbandoned
	default:
		return StatusUnset
	}
}

// unixNanoToTime is the inverse of time.Time.UnixNano. n == 0 maps
// to the zero time so a never-ended span (end_unix_nano left at 0)
// round-trips faithfully.
func unixNanoToTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}
