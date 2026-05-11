package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"

	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/trace"
)

// Indirection points so coverage tests can simulate stale-socket
// cleanup failures without root or filesystem hackery.
var (
	osRemove   = os.Remove
	isNotExist = func(err error) bool { return errors.Is(err, fs.ErrNotExist) }
)

// startVsockTraceReceiver opens a UDS listener at
// <HostSocket>_<TracePort> (the Cloud Hypervisor hybrid-vsock
// convention: every guest AF_VSOCK port maps to one host UDS) and
// hands incoming framed batches to the Tracer reachable from ctx
// (via trace.TracerFromContext). Returns a stop func that closes
// the listener and joins the accept goroutine.
//
// No-op (returns a do-nothing stop) when cfg has no Vsock or no
// TracePort — sandbox-host calls this unconditionally so the
// callers in Hello/run.go don't need a conditional.
//
// If TracePort is set but no Tracer is in ctx, the listener still
// runs (so the in-VM agent's Flush doesn't error on dial), but the
// spans are silently dropped — they have nowhere to go. The
// expected caller wires a Tracer via trace.ContextWithTracer
// before invoking Hello.
func startVsockTraceReceiver(ctx context.Context, cfg Config) (stop func(), err error) {
	if cfg.Vsock == nil || cfg.Vsock.TracePort == 0 {
		return func() {}, nil
	}
	sockPath := fmt.Sprintf("%s_%d", cfg.Vsock.HostSocket, cfg.Vsock.TracePort)
	// Pre-clean a stale socket from a crashed previous run. Listen
	// would otherwise EADDRINUSE; the path is in our own per-VM
	// temp dir (caller's responsibility) so removing is safe.
	// Best-effort: Log + continue on any failure (including
	// permission errors) — Listen will surface a real bind problem
	// loudly enough that masking the cleanup error doesn't hide
	// anything.
	oops.Log("remove stale vsock trace socket", removeIfExists(sockPath))

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return func() {}, fmt.Errorf("sandbox: listen %s: %w", sockPath, err)
	}

	tracer := trace.TracerFromContext(ctx)
	if tracer == nil {
		// Drop spans on the floor but keep accepting connections so
		// the agent's Flush doesn't see a dial error. Construct a
		// throwaway tracer with capacity 1 — its buffer is never
		// observed.
		tracer = trace.NewWithCapacity("dev-null", 1)
	}
	recv := trace.NewStreamReceiver(listener, tracer)

	// The listener's lifetime is bounded by Hello's defer stop(),
	// not the caller's request ctx — so we detach cancellation
	// from the parent (a request-cancelled or SIGTERM ctx would
	// close the listener before the agent has flushed its spans)
	// while keeping any values the parent carried. stop()
	// cancels runCtx explicitly.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		// runCtx-cancel closes the listener; we ignore the returned
		// error (it's either ctx.Canceled or nil on a clean Close).
		_ = recv.Run(runCtx)
	}()

	stop = func() {
		cancel()
		<-done
		// Best-effort path cleanup. The os.Remove inside Listen.Close
		// only fires on the abstract-name UDS path; the filesystem
		// node we created persists until we explicitly delete it.
		oops.Log("remove vsock trace socket on shutdown", removeIfExists(sockPath))
	}
	return stop, nil
}

// removeIfExists removes path, treating "not present" as success.
// Pulled out because Skip allowlists are clearer with one
// dedicated error type per call site.
func removeIfExists(path string) error {
	err := osRemove(path)
	if err == nil || isNotExist(err) {
		return nil
	}
	return err
}
