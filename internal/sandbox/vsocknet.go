package sandbox

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"sync"

	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/sandboxnet"
)

// startVsockNetReceiver opens a UDS listener at
// <HostSocket>_<NetPort> and hands every accepted conn to a
// per-VM sandboxnet.Network. Mirrors startVsockTraceReceiver
// (same hybrid-vsock host-side convention: guest dials AF_VSOCK
// CID 2 port NetPort, CH proxies onto <HostSocket>_<NetPort>).
//
// No-op (returns a do-nothing stop) when cfg has no Vsock or no
// NetPort. The returned stop is always safe to call.
//
// Each accepted conn runs (*sandboxnet.Network).Accept on a
// dedicated goroutine; the call returns when the conn is closed
// or runCtx is cancelled. The switch inside Network learns the
// guest's MAC from the first frame and dispatches replies back
// to whichever conn currently owns that MAC.
func startVsockNetReceiver(ctx context.Context, cfg Config) (stop func(), err error) {
	if cfg.Vsock == nil || cfg.Vsock.NetPort == 0 {
		return func() {}, nil
	}
	network, err := sandboxnet.New(sandboxnet.Config{})
	if err != nil {
		return func() {}, fmt.Errorf("sandbox: sandboxnet.New: %w", err)
	}

	sockPath := fmt.Sprintf("%s_%d", cfg.Vsock.HostSocket, cfg.Vsock.NetPort)
	// Pre-clean a stale socket from a crashed previous run; Listen
	// would otherwise EADDRINUSE.
	oops.Skip("remove stale vsock net socket", os.Remove(sockPath), fs.ErrNotExist)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return func() {}, fmt.Errorf("sandbox: listen %s: %w", sockPath, err)
	}

	// Detach from the request ctx the same way startVsockTraceReceiver
	// does — sandbox.Serve passes its full-lifetime ctx, but we
	// want explicit teardown via stop() rather than parent-cancel
	// closing the listener mid-flight.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Go(func() {
		acceptVsockNetConns(runCtx, listener, network)
	})

	stop = func() {
		cancel()
		// Closing the listener unblocks the Accept in
		// acceptVsockNetConns; the loop sees the error and exits.
		oops.Skip("close vsock net listener", listener.Close(), net.ErrClosed)
		wg.Wait()
		oops.Skip("remove vsock net socket on shutdown",
			os.Remove(sockPath), fs.ErrNotExist)
	}
	return stop, nil
}

// acceptVsockNetConns loops on listener.Accept and spawns a
// goroutine per accepted conn to drive (*Network).Accept.
// Returns when listener.Accept errors (which happens when the
// listener is closed by stop()).
func acceptVsockNetConns(ctx context.Context, listener net.Listener, network *sandboxnet.Network) {
	var wg sync.WaitGroup
	defer wg.Wait()
	//daft:lint:disable=forloopbound reason="bounded by listener.Close() in stop() — Accept returns ErrClosed"
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		wg.Go(func() {
			// EOF / ErrClosed / ErrClosedPipe are the normal
			// "guest hung up" signals out of the netstack rx
			// loop — guest reboots, sandbox teardown, conn
			// drop. Anything else is by construction a real
			// I/O failure that oops.Skip → Must escalates.
			oops.Skip("sandboxnet: Accept loop exit",
				network.Accept(ctx, conn),
				io.EOF, net.ErrClosed, context.Canceled,
				io.ErrClosedPipe)
			oops.Skip("sandboxnet: close conn",
				conn.Close(), net.ErrClosed)
		})
	}
}
