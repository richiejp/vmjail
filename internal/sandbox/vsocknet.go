package sandbox

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"sync"

	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/sandboxnet"
)

// startVsockNetReceiver opens a UDS listener at
// <HostSocket>_<NetPort> and hands every accepted conn to a
// per-VM sandboxnet.Network. No-op (returns a do-nothing stop)
// when cfg has no Vsock or no NetPort. The returned stop is
// always safe to call.
func startVsockNetReceiver(ctx context.Context, cfg Config) (stop func(), err error) {
	if cfg.Vsock == nil || cfg.Vsock.NetPort == 0 {
		return func() {}, nil
	}
	network, err := sandboxnet.New(sandboxnet.Config{})
	if err != nil {
		return func() {}, fmt.Errorf("sandbox: sandboxnet.New: %w", err)
	}

	sockPath := fmt.Sprintf("%s_%d", cfg.Vsock.HostSocket, cfg.Vsock.NetPort)
	oops.Skip("remove stale vsock net socket", os.Remove(sockPath), fs.ErrNotExist)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return func() {}, fmt.Errorf("sandbox: listen %s: %w", sockPath, err)
	}

	// Detached from ctx so stop() owns teardown — a request-scoped
	// cancellation must not close the listener mid-VM.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	conns := &connSet{}
	var wg sync.WaitGroup
	wg.Go(func() {
		acceptVsockNetConns(runCtx, listener, network, conns)
	})

	stop = func() {
		cancel()
		oops.Skip("close vsock net listener", listener.Close(), net.ErrClosed)
		// Close any conns still owned by per-conn goroutines.
		// gvproxy's rxStream parks in io.ReadFull and only checks
		// ctx between reads — without an explicit Close the
		// goroutine sits forever and wg.Wait below deadlocks.
		conns.closeAll()
		wg.Wait()
		oops.Skip("remove vsock net socket on shutdown",
			os.Remove(sockPath), fs.ErrNotExist)
	}
	return stop, nil
}

// connSet tracks accepted conns so stop() can close them all and
// unblock the per-conn goroutines parked in the netstack's rx
// read.
type connSet struct {
	mu sync.Mutex
	cs []net.Conn
}

func (s *connSet) add(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cs = append(s.cs, c)
}

func (s *connSet) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.cs {
		oops.Skip("sandboxnet: close tracked conn",
			c.Close(), net.ErrClosed)
	}
	s.cs = nil
}

// acceptVsockNetConns loops on listener.Accept and spawns a
// goroutine per accepted conn to drive (*Network).Accept.
// Returns when listener.Accept errors.
func acceptVsockNetConns(ctx context.Context, listener net.Listener, network *sandboxnet.Network, conns *connSet) {
	var wg sync.WaitGroup
	defer wg.Wait()
	//daft:lint:disable=forloopbound reason="bounded by listener.Close() in stop() — Accept returns ErrClosed"
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		conns.add(conn)
		wg.Go(func() {
			acceptErr := network.Accept(ctx, conn)
			if acceptErr != nil && !oops.IsExpectedPumpExit(acceptErr) {
				oops.Log("sandboxnet: Accept loop exit", acceptErr)
			}
			oops.Skip("sandboxnet: close conn",
				conn.Close(), net.ErrClosed)
		})
	}
}
