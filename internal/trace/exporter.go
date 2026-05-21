package trace

import (
	"context"

	"connectrpc.com/connect"

	tracev1 "github.com/localai-org/darkaifactory/internal/genpb/trace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/trace/v1/tracev1connect"
)

// Exporter ships finished spans to a remote consumer (typically the
// orchestrator's TraceService). Implementations must be safe for
// concurrent calls.
type Exporter interface {
	Export(ctx context.Context, spans []*tracev1.Span) error
}

// ConnectExporter ships spans via a Connect-RPC client. It does not
// own the underlying HTTP client — the caller wires the same mTLS
// transport here as it uses for the rest of its RPCs.
type ConnectExporter struct {
	Client tracev1connect.TraceServiceClient
}

// NewConnectExporter is a small constructor for symmetry with the
// rest of the package.
func NewConnectExporter(client tracev1connect.TraceServiceClient) *ConnectExporter {
	return &ConnectExporter{Client: client}
}

// Export issues a single ReportSpans call. Empty span lists short-
// circuit so callers don't pay for an RPC on a no-op flush.
func (e *ConnectExporter) Export(ctx context.Context, spans []*tracev1.Span) error {
	if e == nil || e.Client == nil || len(spans) == 0 {
		return nil
	}
	_, err := e.Client.ReportSpans(ctx, connect.NewRequest(&tracev1.ReportSpansRequest{
		Spans: spans,
	}))
	return err
}
