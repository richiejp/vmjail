package trace

import (
	"context"

	"connectrpc.com/connect"
)

// HeaderTraceID and HeaderParentSpanID carry the wire-propagated
// trace context. We use our own header names rather than W3C
// `traceparent` so a future OTel-compatible proxy-exporter has a
// clear translation step instead of a silent collision with
// other instrumentation in the same request.
const (
	HeaderTraceID      = "Daft-Trace-Id"
	HeaderParentSpanID = "Daft-Parent-Span-Id"
)

// ClientInterceptor wraps every outbound unary call in a CLIENT
// span and injects the trace context into the request headers.
// Errors from the inner call are recorded on the span. Streaming
// is not yet covered — when the project starts using streaming
// RPCs, extend WrapStreamingClient on the same struct.
func (t *Tracer) ClientInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			ctx, span := t.Start(ctx, req.Spec().Procedure, WithKind(SpanKindClient))
			defer span.End()
			req.Header().Set(HeaderTraceID, span.traceID)
			req.Header().Set(HeaderParentSpanID, span.spanID)
			resp, err := next(ctx, req)
			if err != nil {
				_ = span.RecordError(err)
			}
			return resp, err
		}
	}
}

// ServerInterceptor opens a SERVER span for every inbound unary
// call, parented by the wire-propagated trace context if valid.
// Malformed headers (wrong length, non-hex) are ignored and the
// span starts a fresh trace — defence against a misbehaving peer
// linking unrelated work into the orchestrator's view.
func (t *Tracer) ServerInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			opts := []SpanOption{WithKind(SpanKindServer)}
			traceID := req.Header().Get(HeaderTraceID)
			parentID := req.Header().Get(HeaderParentSpanID)
			if IsValidTraceID(traceID) && IsValidSpanID(parentID) {
				opts = append(opts, WithParent(traceID, parentID))
			}
			ctx, span := t.Start(ctx, req.Spec().Procedure, opts...)
			defer span.End()
			resp, err := next(ctx, req)
			if err != nil {
				_ = span.RecordError(err)
			}
			return resp, err
		}
	}
}
