package sandbox

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"

	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/trace"
)

// startVsockTraceReceiver opens a UDS listener at
// <HostSocket>_<TracePort> (the Cloud Hypervisor hybrid-vsock
// convention: every guest AF_VSOCK port maps to one host UDS) and
// hands incoming framed batches to the Tracer reachable from ctx
// (via trace.TracerFromContext). Returns a stop func that closes
// the listener and joins the accept goroutine.
//
// No-op (returns a do-nothing stop) when cfg has no Vsock or no
// TracePort.
//
// If TracePort is set but no Tracer is in ctx, the listener still
// runs (so the in-VM agent's Flush doesn't error on dial), but the
// spans are silently dropped. The expected caller wires a Tracer
// via trace.ContextWithTracer before invoking Hello.
func startVsockTraceReceiver(ctx context.Context, cfg Config) (stop func(), err error) {
	if cfg.Vsock == nil || cfg.Vsock.TracePort == 0 {
		return func() {}, nil
	}
	sockPath := fmt.Sprintf("%s_%d", cfg.Vsock.HostSocket, cfg.Vsock.TracePort)
	// Pre-clean a stale socket from a crashed previous run; Listen
	// would otherwise EADDRINUSE.
	oops.Skip("remove stale vsock trace socket", os.Remove(sockPath), fs.ErrNotExist)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return func() {}, fmt.Errorf("sandbox: listen %s: %w", sockPath, err)
	}

	tracer := trace.TracerFromContext(ctx)
	if tracer == nil {
		tracer = trace.NewWithCapacity("dev-null", 1)
	}
	recv := trace.NewStreamReceiver(listener, tracer)

	// The listener's lifetime is bounded by Hello's defer stop(),
	// not the caller's request ctx — detach cancellation from the
	// parent (a request-cancelled or SIGTERM ctx would close the
	// listener before the agent has flushed its spans) while
	// keeping any values the parent carried.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = recv.Run(runCtx)
	}()

	stop = func() {
		cancel()
		<-done
		oops.Skip("remove vsock trace socket on shutdown",
			os.Remove(sockPath), fs.ErrNotExist)
	}
	return stop, nil
}
