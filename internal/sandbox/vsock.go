package sandbox

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"time"

	"github.com/localai-org/darkaifactory/internal/agent"
	agentv1 "github.com/localai-org/darkaifactory/internal/genpb/agent/v1"
	"github.com/localai-org/darkaifactory/internal/oops"
	"github.com/localai-org/darkaifactory/internal/trace"
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

// configServeTimeout bounds one config exchange (read handshake +
// write response) on an accepted connection so a client that connects
// but never sends its handshake can't pin a server goroutine.
const configServeTimeout = 5 * time.Second

// startVsockConfigServer opens a UDS listener at
// <HostSocket>_<DefaultConfigVsockPort> (the same Cloud Hypervisor
// hybrid-vsock convention the trace receiver uses) and replies to each
// accepted connection with this VM's GuestConfig, built from cfg. The
// in-VM agent dials it right after printing its banner to learn what
// shares to mount and what bundle to run, replacing the daft.* kernel
// cmdline tokens.
//
// No-op (returns a do-nothing stop) when cfg has no Vsock / HostSocket:
// such a VM has no vsock device at all, so the agent can't dial and
// falls back to the cmdline (the slice-1 transition).
func startVsockConfigServer(ctx context.Context, cfg Config) (stop func(), err error) {
	if cfg.Vsock == nil || cfg.Vsock.HostSocket == "" {
		return func() {}, nil
	}
	sockPath := fmt.Sprintf("%s_%d", cfg.Vsock.HostSocket, agent.DefaultConfigVsockPort)
	// Pre-clean a stale socket from a crashed previous run; Listen
	// would otherwise EADDRINUSE.
	oops.Skip("remove stale vsock config socket", os.Remove(sockPath), fs.ErrNotExist)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return func() {}, fmt.Errorf("sandbox: listen %s: %w", sockPath, err)
	}

	guestCfg := buildGuestConfig(cfg)
	// Detach from the request ctx (same rationale as the trace
	// receiver): the listener's lifetime is bounded by the caller's
	// defer stop(), not a request-cancelled or SIGTERM ctx.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveGuestConfig(runCtx, listener, guestCfg)
	}()

	stop = func() {
		cancel()
		_ = listener.Close()
		<-done
		oops.Skip("remove vsock config socket on shutdown",
			os.Remove(sockPath), fs.ErrNotExist)
	}
	return stop, nil
}

// buildGuestConfig projects the boot Config into the GuestConfig wire
// message the agent applies: the container bundle plus each virtio-fs
// share. Mirrors the bundle/share half of buildKernelCmdline.
func buildGuestConfig(cfg Config) *agentv1.GuestConfig {
	gc := &agentv1.GuestConfig{Bundle: cfg.Bundle}
	for _, s := range cfg.Shares {
		gc.Shares = append(gc.Shares, &agentv1.GuestConfig_Share{
			Tag:        s.Tag,
			Mountpoint: s.Mountpoint,
		})
	}
	return gc
}

// serveGuestConfig accepts connections until the listener is closed,
// replying to each with cfg. One request→response per connection; a
// failed exchange just drops that conn.
func serveGuestConfig(ctx context.Context, listener net.Listener, cfg *agentv1.GuestConfig) {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	//daft:lint:disable=forloopbound reason="exits when listener.Close drives Accept to return net.ErrClosed"
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go handleGuestConfigConn(conn, cfg)
	}
}

// handleGuestConfigConn reads the agent's handshake and writes the
// GuestConfig reply, then closes. Bounded by configServeTimeout.
func handleGuestConfigConn(conn net.Conn, cfg *agentv1.GuestConfig) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(configServeTimeout))
	if _, err := agent.ReadGuestConfigRequest(conn); err != nil {
		return
	}
	oops.Skip("sandbox: write guest config", agent.WriteGuestConfig(conn, cfg),
		net.ErrClosed, os.ErrDeadlineExceeded)
}
