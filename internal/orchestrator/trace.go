package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"connectrpc.com/connect"

	tracev1 "github.com/localai-org/darkaifactory/internal/genpb/trace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/trace/v1/tracev1connect"
	"github.com/localai-org/darkaifactory/internal/trace"
)

// JSON field names emitted by /debug/traces and consumed by tests.
// Defined as constants so the producer side (writeTracesJSON) and
// the consumer side (trace_test.go's assertions) can't drift.
const (
	jsonKeyTraceID      = "trace_id"
	jsonKeySpanID       = "span_id"
	jsonKeyParentSpanID = "parent_span_id"
	jsonKeyName         = "name"
	jsonKeyComponent    = "component"
	jsonKeyKind         = "kind"
)

// TraceHandler implements TraceServiceHandler. It funnels every
// reported span into the orchestrator's central Tracer so the
// /debug/traces endpoint can surface them. The handler holds no
// state of its own beyond the Tracer reference.
type TraceHandler struct {
	tracev1connect.UnimplementedTraceServiceHandler
	tracer *trace.Tracer
}

// NewTraceHandler returns a handler that ingests into tr.
func NewTraceHandler(tr *trace.Tracer) (*TraceHandler, error) {
	if tr == nil {
		return nil, errors.New("orchestrator: nil tracer")
	}
	return &TraceHandler{tracer: tr}, nil
}

// ReportSpans appends incoming spans to the tracer's buffer. We
// always return success — a misbehaving producer can flood our
// buffer (which the ring-buffer cap blunts), but we never tell it
// to retry, since retries would just amplify the flood.
func (h *TraceHandler) ReportSpans(_ context.Context, req *connect.Request[tracev1.ReportSpansRequest]) (*connect.Response[tracev1.ReportSpansResponse], error) {
	h.tracer.IngestProto(req.Msg.GetSpans())
	return connect.NewResponse(&tracev1.ReportSpansResponse{}), nil
}

// debugTracesHandler renders the tracer's snapshot. Format is
// chosen by the Accept header (application/json → JSON; everything
// else → text). Behind the same mTLS gate as every other handler,
// so peers must already be approved to read it.
func debugTracesHandler(tr *trace.Tracer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		spans := tr.Snapshot()
		if r.Header.Get("Accept") == "application/json" {
			writeTracesJSON(w, spans)
			return
		}
		var buf bytes.Buffer
		if err := trace.FormatText(&buf, spans); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(buf.Bytes())
	}
}

func writeTracesJSON(w http.ResponseWriter, spans []*trace.Span) {
	out := make([]map[string]any, len(spans))
	for i, s := range spans {
		out[i] = map[string]any{
			jsonKeyTraceID:      s.TraceID(),
			jsonKeySpanID:       s.SpanID(),
			jsonKeyParentSpanID: s.ParentSpanID(),
			jsonKeyName:         s.Name(),
			jsonKeyComponent:    s.Component(),
			jsonKeyKind:         s.Kind().String(),
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
