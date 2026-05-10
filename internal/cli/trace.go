package cli

import (
	"context"
	"net/http"
	"time"

	"github.com/richiejp/darkaifactory/internal/component"
	"github.com/richiejp/darkaifactory/internal/genpb/trace/v1/tracev1connect"
	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/trace"
)

// flushTimeout caps how long Tracer.Shutdown waits for the trace
// export RPC. Long enough to clear a small batch over a slow link;
// short enough not to delay the user past their RPC timeout.
const flushTimeout = 5 * time.Second

// tracerForCommand returns the Tracer to use for a one-shot CLI
// command. Tests plant a Tracer on ctx via trace.ContextWithTracer
// so they can inspect spans without shipping them; production picks
// up a fresh local one (owned=true), which the caller then ships
// via shipTraces on success.
func tracerForCommand(ctx context.Context) (tr *trace.Tracer, owned bool) {
	if t := trace.TracerFromContext(ctx); t != nil {
		return t, false
	}
	return trace.New(component.Client), true
}

// shipTraces wires a TraceService exporter pointed at addr and
// Shutdowns the tracer (which abandons any still-active spans then
// flushes). The export client deliberately bypasses the trace
// interceptor — instrumenting the export path would recurse and
// leave a tail of unshipped spans on every call. label is used in
// the diagnostic message if the flush itself fails.
func shipTraces(ctx context.Context, tracer *trace.Tracer, httpc *http.Client, addr, label string) {
	traceClient := tracev1connect.NewTraceServiceClient(httpc, "https://"+addr)
	tracer.SetExporter(trace.NewConnectExporter(traceClient))
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
	defer cancel()
	oops.Log(label+": trace flush", tracer.Shutdown(flushCtx))
}
