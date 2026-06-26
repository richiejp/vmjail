package sandbox

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"sync"

	"github.com/richiejp/darkaifactory/internal/agent"
	"github.com/richiejp/darkaifactory/internal/observability"
	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/sandboxnet"
)

// startVsockNetReceiver opens a UDS listener at
// <HostSocket>_<agent.DefaultNetVsockPort> and hands every accepted conn
// to a per-VM sandboxnet.Network. No-op (returns a do-nothing stop and
// nil collector) when cfg has no Vsock or the net feature is off
// (cfg.Vsock.Net == false). The returned stop is always safe to call.
//
// collectNet, when non-nil, is a snapshot function over the per-VM
// network's byte counters in the shape the observer expects. The
// observer pulls it once per tick to record rx/tx bytes through the
// userspace netstack — a layer /proc can't see because the traffic
// never crosses a host tap. Returned nil when no network is wired
// (cfg.Vsock nil or net off).
func startVsockNetReceiver(ctx context.Context, cfg Config) (stop func(), collectNet func() *observability.NetSample, err error) {
	if cfg.Vsock == nil || !cfg.Vsock.Net {
		return func() {}, nil, nil
	}
	network, err := sandboxnet.New(sandboxnet.Config{
		DenyEgress:   cfg.DenyEgress,
		ProxyHandler: cfg.ProxyHandler,
		ProxyPort:    cfg.ProxyPort,
	})
	if err != nil {
		return func() {}, nil, fmt.Errorf("sandbox: sandboxnet.New: %w", err)
	}

	sockPath := fmt.Sprintf("%s_%d", cfg.Vsock.HostSocket, agent.DefaultNetVsockPort)
	oops.Skip("remove stale vsock net socket", os.Remove(sockPath), fs.ErrNotExist)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return func() {}, nil, fmt.Errorf("sandbox: listen %s: %w", sockPath, err)
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
	collectNet = func() *observability.NetSample {
		// Until the netstack has accepted at least one conn, the
		// observer can't tell zero-counters-because-idle from
		// zero-counters-because-never-connected. Return nil for
		// the pre-conn window so the Net block is omitted and
		// the distinction lives in the JSON shape, not in a
		// number that means two different things.
		if !network.Active() {
			return nil
		}
		c := network.Counters()
		return &observability.NetSample{RxBytes: c.RxBytes, TxBytes: c.TxBytes}
	}
	return stop, collectNet, nil
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
