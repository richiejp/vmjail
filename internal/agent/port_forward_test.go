package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/localai-org/darkaifactory/internal/genpb/agent/v1"
)

// fakeVsockListener is an in-process net.Listener that accepts
// connections enqueued by the test (via push). Stands in for the
// real AF_VSOCK listener so port_forward_test stays platform-
// independent.
type fakeVsockListener struct {
	addr  fakeAddr
	conns chan net.Conn
	done  chan struct{}

	closeOnce sync.Once
}

func newFakeVsockListener(port uint32) *fakeVsockListener {
	return &fakeVsockListener{
		addr:  fakeAddr{net: "vsock", str: "vsock:" + strconv.FormatUint(uint64(port), 10)},
		conns: make(chan net.Conn, 4),
		done:  make(chan struct{}),
	}
}

func (f *fakeVsockListener) Accept() (net.Conn, error) {
	select {
	case c, ok := <-f.conns:
		if !ok {
			return nil, net.ErrClosed
		}
		return c, nil
	case <-f.done:
		return nil, net.ErrClosed
	}
}

func (f *fakeVsockListener) Close() error {
	f.closeOnce.Do(func() { close(f.done) })
	return nil
}

func (f *fakeVsockListener) Addr() net.Addr { return f.addr }

// push hands a pre-paired net.Conn to the listener; the next
// Accept returns it.
func (f *fakeVsockListener) push(c net.Conn) {
	f.conns <- c
}

type fakeAddr struct{ net, str string }

func (a fakeAddr) Network() string { return a.net }
func (a fakeAddr) String() string  { return a.str }

// TestPortForwarderPumpsBytesEndToEnd drives runPortForwarders
// against an in-process pair: a test net.Pipe stands in for the
// vsock-side connection, another net.Pipe stands in for the
// container connection. Bytes written upstream must reach the
// container side and vice-versa.
func TestPortForwarderPumpsBytesEndToEnd(t *testing.T) {
	ln := newFakeVsockListener(8000)
	containerL, containerR := net.Pipe()
	t.Cleanup(func() { _ = containerR.Close() })

	d := portForwardDeps{
		listenVsock: func(port uint32) (net.Listener, error) {
			if port != 8000 {
				t.Fatalf("unexpected port %d", port)
			}
			return ln, nil
		},
		dialContainer: func(context.Context, uint32) (net.Conn, error) {
			return containerR, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var buf bytes.Buffer
		runPortForwarders(ctx, &buf, []PortForwardSpec{{VsockPort: 8000, ContainerPort: 3000}}, d)
	}()

	// Drive an upstream connection through the listener.
	upstream, vsockSide := net.Pipe()
	t.Cleanup(func() { _ = upstream.Close() })
	ln.push(vsockSide)

	// Upstream → container.
	want := []byte("hello container")
	go func() { _, _ = upstream.Write(want) }()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(containerL, got); err != nil {
		t.Fatalf("read container side: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("container received %q, want %q", got, want)
	}

	// Container → upstream.
	reply := []byte("hello agent")
	go func() { _, _ = containerL.Write(reply) }()
	gotReply := make([]byte, len(reply))
	if _, err := io.ReadFull(upstream, gotReply); err != nil {
		t.Fatalf("read upstream: %v", err)
	}
	if !bytes.Equal(gotReply, reply) {
		t.Errorf("upstream received %q, want %q", gotReply, reply)
	}

	// Drain. Closing the listener + cancelling ctx unblocks the
	// accept loop and the pump.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runPortForwarders did not return within 2s after ctx cancel")
	}
}

// TestPortForwarderReportsListenError pins the per-port "binding
// failed" branch: a listenVsock returning an error must be
// reported on the console and skip that spec without crashing.
func TestPortForwarderReportsListenError(t *testing.T) {
	want := errors.New("synthetic bind failure")
	d := portForwardDeps{
		listenVsock: func(uint32) (net.Listener, error) { return nil, want },
		dialContainer: func(context.Context, uint32) (net.Conn, error) {
			t.Fatal("dial should not be called when listen fails")
			return nil, errors.New("unreachable")
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPortForwarders(ctx, &buf, []PortForwardSpec{{VsockPort: 9000, ContainerPort: 22}}, d)
	}()
	// Listen failure is per-port — runPortForwarders still waits
	// on ctx because the for-loop completed without registering
	// any pump goroutine. Cancel to let it return.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runPortForwarders did not return within 2s after ctx cancel")
	}
	if got := buf.String(); !bytes.Contains([]byte(got), []byte("synthetic bind failure")) {
		t.Errorf("console missing error report:\n%s", got)
	}
}

// TestPortForwarderEmptySpecsIsNoOp pins the no-ports path so the
// host doesn't have to special-case workspaces with empty
// PortMapping slices.
func TestPortForwarderEmptySpecsIsNoOp(t *testing.T) {
	d := portForwardDeps{
		listenVsock: func(uint32) (net.Listener, error) {
			t.Fatal("listenVsock should not be called for empty specs")
			return nil, errors.New("unreachable")
		},
	}
	var buf bytes.Buffer
	// Should return immediately; if it doesn't this test will
	// time out at the test runner level.
	runPortForwarders(context.Background(), &buf, nil, d)
}

// TestStartPortForwardersReportsBadCmdline pins the cmdline-parse
// error branch on the dispatcher: a malformed daft.port= must be
// surfaced on the console, not silently dropped.
func TestStartPortForwardersReportsBadCmdline(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startPortForwarders(ctx, &buf, runDeps{
		portForward: portForwardDeps{
			listenVsock: func(uint32) (net.Listener, error) {
				t.Fatal("listenVsock should not be called for a malformed cmdline")
				return nil, errors.New("unreachable")
			},
		},
	}, "daft.port=abc:def")
	if got := buf.String(); !bytes.Contains([]byte(got), []byte("daft.port")) {
		t.Errorf("console missing cmdline error:\n%s", got)
	}
}

// TestStartPortForwardersEmptyIsNoOp pins the no-tokens path: an
// empty cmdline must not spawn a goroutine nor touch listenVsock.
func TestStartPortForwardersEmptyIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	called := false
	startPortForwarders(context.Background(), &buf, runDeps{
		portForward: portForwardDeps{
			listenVsock: func(uint32) (net.Listener, error) {
				called = true
				return nil, errors.New("unreachable")
			},
		},
	}, "console=ttyS0")
	if called {
		t.Error("listenVsock was called for empty cmdline")
	}
}

// TestDialLoopbackTCPConnects covers the production container dialer
// against a real loopback listener: it must reach 127.0.0.1:<port>.
func TestDialLoopbackTCPConnects(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr %T is not *net.TCPAddr", ln.Addr())
	}
	port := uint32(addr.Port) //nolint:gosec // G115: an ephemeral TCP port fits a uint32.

	conn, err := dialLoopbackTCP(context.Background(), port)
	if err != nil {
		t.Fatalf("dialLoopbackTCP: %v", err)
	}
	_ = conn.Close()
}

// TestStartPortForwardersPrefersConfig pins the config-preferred path:
// when the GuestConfig carries a port-forward table, the dispatcher uses
// it (and ignores the cmdline daft.port token entirely).
func TestStartPortForwardersPrefersConfig(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gotPort := make(chan uint32, 1)
	startPortForwarders(ctx, &buf, runDeps{
		cfg: &agentv1.GuestConfig{PortForwards: []*agentv1.GuestConfig_PortForward{
			{VsockPort: 10001, ContainerPort: 8080},
		}},
		portForward: portForwardDeps{
			listenVsock: func(port uint32) (net.Listener, error) {
				gotPort <- port
				return newFakeVsockListener(port), nil
			},
		},
	}, "daft.port=10009:9999")
	select {
	case p := <-gotPort:
		if p != 10001 {
			t.Errorf("listened on vsock %d, want 10001 (config, not cmdline)", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listenVsock never called for config port-forward")
	}
}

// TestPumpConnectionDialErrorReportsAndExits pins the dial-failed
// branch of pumpConnection: a failed container dial logs the
// error and doesn't deadlock.
func TestPumpConnectionDialErrorReportsAndExits(t *testing.T) {
	ln := newFakeVsockListener(8000)
	wantErr := errors.New("synthetic container-dial fail")
	d := portForwardDeps{
		listenVsock: func(uint32) (net.Listener, error) { return ln, nil },
		dialContainer: func(context.Context, uint32) (net.Conn, error) {
			return nil, wantErr
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		runPortForwarders(ctx, &buf, []PortForwardSpec{{VsockPort: 8000, ContainerPort: 3000}}, d)
	}()

	upstream, vsockSide := net.Pipe()
	t.Cleanup(func() { _ = upstream.Close() })
	ln.push(vsockSide)

	// Give the goroutine a moment to attempt the dial. The upstream
	// pipe gets closed when pumpConnection bails.
	deadline := time.Now().Add(time.Second)
	//daft:lint:disable=forloopbound reason="wall-clock bounded to 1s; exits on observed close or on the deadline"
	for time.Now().Before(deadline) {
		// A 1-byte write either blocks (pump still alive) or
		// errors (pump tore down). Use a short deadline so the
		// loop terminates either way.
		_ = upstream.SetWriteDeadline(time.Now().Add(50 * time.Millisecond))
		_, err := upstream.Write([]byte{0})
		if err != nil {
			break
		}
	}
	cancel()
	<-done
	if got := buf.String(); !bytes.Contains([]byte(got), []byte("synthetic container-dial fail")) {
		t.Errorf("console missing dial error:\n%s", got)
	}
}
