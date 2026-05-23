package sandboxnet

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

// BenchmarkForwarderTCPThroughput measures the gvisor TCP forwarder
// path end-to-end without a real VM. Two netstacks are wired
// link-to-link:
//
//   - host: sandboxnet.Network with HostLoopbackIP=.254 → NAT to
//     127.0.0.1. The TCP forwarder catches SYNs to addresses with no
//     local endpoint and `net.Dial`s them on the host kernel.
//   - guest: barebones virtualnetwork.New at gateway IP .2, no NAT,
//     no virtual IPs. Used purely as a TCP source.
//
// Wire: net.Pipe between the two AcceptStdio loops. Same
// StdioProtocol framing the real sandboxd path uses.
//
// Traffic path under bench: guest gonet.DialTCP → guest IP layer →
// guest's link → net.Pipe → host's Accept → host switch → host's
// TCP forwarder → net.Dial("tcp", 127.0.0.1:N) → real net.Listen
// drainer. Bytes flow client → drainer at b.SetBytes(chunk) rate;
// b.Loop's natural backpressure (Write blocks once gonet's send
// buffer is full) makes the steady-state rate equal the actual
// network throughput, not the queue-fill rate.
//
// Bench file lives in package sandboxnet (not sandboxnet_test) so
// it can drive virtualnetwork.New directly for the guest side
// — the alternative would have been a sandboxnet.Config knob
// to disable the HostLoopbackIP NAT, which is API surface added
// for a one-time experiment.
func BenchmarkForwarderTCPThroughput(b *testing.B) {
	for _, chunkSize := range []int{1024, 4096, 32 * 1024, 128 * 1024} {
		b.Run("chunk="+strconv.Itoa(chunkSize), func(b *testing.B) {
			benchmarkForwarderTCP(b, chunkSize)
		})
	}
}

const (
	benchGuestGatewayIP  = "192.168.127.2"
	benchGuestGatewayMAC = "5a:94:ef:e4:0c:de"
	benchDialTimeout     = 5 * time.Second
)

// forwarderBenchRig holds the per-iteration plumbing: a host
// sandboxnet, a guest virtualnetwork, a client/server TCP pair
// already handshaked, and a drainer goroutine consuming from the
// server side. Construction is the bulk of the bench setup — the
// hot loop only writes into clientConn.
type forwarderBenchRig struct {
	clientConn net.Conn
	cleanup    func()
}

func benchmarkForwarderTCP(b *testing.B, chunkSize int) {
	b.Helper()

	rig := newForwarderBenchRig(b)
	defer rig.cleanup()

	chunk := make([]byte, chunkSize)
	b.SetBytes(int64(chunkSize))
	for b.Loop() {
		if _, werr := rig.clientConn.Write(chunk); werr != nil {
			b.Fatalf("clientConn.Write: %v", werr)
		}
	}
}

func newForwarderBenchRig(b *testing.B) *forwarderBenchRig {
	b.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatalf("net.Listen: %v", err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		b.Fatalf("listener address is not *net.TCPAddr: %T", ln.Addr())
	}

	hostNet, guestNet := newBenchStacks(b)
	hostConn, guestConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())

	acceptsDone := startAcceptLoops(ctx, hostNet, guestNet, hostConn, guestConn)
	clientConn, serverConn := dialAndAccept(b, ctx, guestNet, ln, tcpAddr.Port)

	drainerDone := make(chan struct{})
	go func() {
		defer close(drainerDone)
		_, _ = io.Copy(io.Discard, serverConn)
	}()

	cleanup := func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
		select {
		case <-drainerDone:
		case <-time.After(benchDialTimeout):
		}
		cancel()
		_ = hostConn.Close()
		_ = guestConn.Close()
		_ = ln.Close()
		for range 2 {
			select {
			case <-acceptsDone:
			case <-time.After(time.Second):
			}
		}
	}
	return &forwarderBenchRig{clientConn: clientConn, cleanup: cleanup}
}

func newBenchStacks(b *testing.B) (*Network, *virtualnetwork.VirtualNetwork) {
	b.Helper()
	hostNet, err := New(Config{})
	if err != nil {
		b.Fatalf("sandboxnet.New(host): %v", err)
	}
	guestNet, err := virtualnetwork.New(&types.Configuration{
		Subnet:            DefaultSubnet,
		GatewayIP:         benchGuestGatewayIP,
		GatewayMacAddress: benchGuestGatewayMAC,
		MTU:               DefaultMTU,
		Protocol:          types.StdioProtocol,
	})
	if err != nil {
		b.Fatalf("virtualnetwork.New(guest): %v", err)
	}
	return hostNet, guestNet
}

func startAcceptLoops(
	ctx context.Context,
	hostNet *Network,
	guestNet *virtualnetwork.VirtualNetwork,
	hostConn, guestConn net.Conn,
) chan struct{} {
	done := make(chan struct{}, 2)
	go func() {
		_ = hostNet.Accept(ctx, hostConn)
		done <- struct{}{}
	}()
	go func() {
		_ = guestNet.AcceptStdio(ctx, guestConn)
		done <- struct{}{}
	}()
	return done
}

func dialAndAccept(
	b *testing.B,
	ctx context.Context,
	guestNet *virtualnetwork.VirtualNetwork,
	ln net.Listener,
	port int,
) (clientConn, serverConn net.Conn) {
	b.Helper()
	type acceptResult struct {
		conn net.Conn
		err  error
	}
	results := make(chan acceptResult, 1)
	go func() {
		c, aerr := ln.Accept()
		results <- acceptResult{conn: c, err: aerr}
	}()

	target := net.JoinHostPort(DefaultHostLoopbackIP, strconv.Itoa(port))
	dialCtx, dialCancel := context.WithTimeout(ctx, benchDialTimeout)
	defer dialCancel()
	cc, err := guestNet.DialContextTCP(dialCtx, target)
	if err != nil {
		b.Fatalf("guest Dial %s: %v", target, err)
	}

	select {
	case r := <-results:
		if r.err != nil {
			_ = cc.Close()
			b.Fatalf("listener accept: %v", r.err)
		}
		return cc, r.conn
	case <-time.After(benchDialTimeout):
		_ = cc.Close()
		b.Fatalf("listener never accepted after %s", benchDialTimeout)
		return nil, nil
	}
}
