package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
)

// TestParsePortMappingArg covers the CLI's first-line parser.
func TestParsePortMappingArg(t *testing.T) {
	for _, tc := range []struct {
		in        string
		wantLocal uint32
		wantCont  uint32
		wantErr   string
	}{
		{"8080:80", 8080, 80, ""},
		{"22:22", 22, 22, ""},
		{"65535:1", 65535, 1, ""},
		{"", 0, 0, "expected <local>:<container>"},
		{"8080", 0, 0, "expected <local>:<container>"},
		{"8080:", 0, 0, "empty container port"},
		{":80", 0, 0, "empty local port"},
		{"0:80", 0, 0, "must be non-zero"},
		{"80:0", 0, 0, "must be non-zero"},
		{"8080:80:90", 0, 0, "too many ':'"},
		{"99999:80", 0, 0, "out of range"},
		{"foo:80", 0, 0, "bad local port"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			local, cont, err := parsePortMappingArg(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if local != tc.wantLocal || cont != tc.wantCont {
				t.Errorf("got (%d, %d), want (%d, %d)", local, cont, tc.wantLocal, tc.wantCont)
			}
		})
	}
}

// fakeForwardClientStream is an in-process forwardClientStream
// pair for tests. push() feeds inbound frames the caller will
// Receive; sent() returns what the caller Sent. closeOnce gates
// CloseResponse so the pump's cross-cancel and the test's
// explicit closeIncoming don't double-close the channel.
type fakeForwardClientStream struct {
	mu        sync.Mutex
	incoming  chan *workspacev1.ForwardFrame
	outgoing  []*workspacev1.ForwardFrame
	sendErr   error
	recvErr   error
	closeOnce sync.Once
}

func newFakeForwardClientStream() *fakeForwardClientStream {
	return &fakeForwardClientStream{incoming: make(chan *workspacev1.ForwardFrame, 16)}
}

func (s *fakeForwardClientStream) Send(m *workspacev1.ForwardFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.outgoing = append(s.outgoing, m)
	return nil
}

func (s *fakeForwardClientStream) Receive() (*workspacev1.ForwardFrame, error) {
	s.mu.Lock()
	err := s.recvErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	m, ok := <-s.incoming
	if !ok {
		return nil, io.EOF
	}
	return m, nil
}

func (s *fakeForwardClientStream) push(m *workspacev1.ForwardFrame) { s.incoming <- m }

func (s *fakeForwardClientStream) CloseResponse() error {
	s.closeOnce.Do(func() { close(s.incoming) })
	return nil
}

func (s *fakeForwardClientStream) closeIncoming() { _ = s.CloseResponse() }

// unreachableForwardClient is a placeholder WorkspaceCatalogServiceClient
// for tests that need a non-nil client to satisfy the nilnil lint but
// must not actually dial — every method panics so a misroute surfaces
// instead of silently passing.
type unreachableForwardClient struct {
	workspacev1connect.WorkspaceCatalogServiceClient
}

func (s *fakeForwardClientStream) sent() []*workspacev1.ForwardFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*workspacev1.ForwardFrame, len(s.outgoing))
	copy(out, s.outgoing)
	return out
}

// TestPumpForwardConnRoundTrip pins the per-tunnel pump: bytes
// written on conn appear as Data frames on the stream, and Data
// frames pushed to the stream become bytes readable on conn.
func TestPumpForwardConnRoundTrip(t *testing.T) {
	connLocal, connRemote := net.Pipe()
	t.Cleanup(func() { _ = connRemote.Close() })
	stream := newFakeForwardClientStream()

	pumpDone := make(chan error, 1)
	go func() { pumpDone <- pumpForwardConn(connRemote, stream) }()

	// local → stream
	want := []byte("client-says-hi")
	go func() { _, _ = connLocal.Write(want) }()
	deadline := time.Now().Add(2 * time.Second)
	//daft:lint:disable=forloopbound reason="bounded by 2s wall clock; exits once the Data frame is observed"
	for time.Now().Before(deadline) {
		sent := stream.sent()
		if len(sent) > 0 && bytes.Equal(sent[0].GetData(), want) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(stream.sent()) == 0 {
		t.Fatal("no frame Sent within 2s")
	}

	// stream → local
	reply := []byte("server-reply")
	stream.push(&workspacev1.ForwardFrame{
		Body: &workspacev1.ForwardFrame_Data{Data: reply},
	})
	got := make([]byte, len(reply))
	if _, err := io.ReadFull(connLocal, got); err != nil {
		t.Fatalf("connLocal.Read: %v", err)
	}
	if !bytes.Equal(got, reply) {
		t.Errorf("got %q, want %q", got, reply)
	}

	// Drain: close both ends, pump returns.
	_ = connLocal.Close()
	stream.closeIncoming()
	select {
	case err := <-pumpDone:
		if err != nil {
			t.Errorf("pumpForwardConn: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pumpForwardConn did not return within 2s")
	}
}

// TestPumpForwardConnReceiveError pins that a non-EOF error from
// stream.Receive surfaces as the pump's return value rather than
// silently dropping the tunnel.
func TestPumpForwardConnReceiveError(t *testing.T) {
	connLocal, connRemote := net.Pipe()
	t.Cleanup(func() { _ = connLocal.Close() })
	t.Cleanup(func() { _ = connRemote.Close() })
	want := errors.New("synthetic recv fail")
	stream := newFakeForwardClientStream()
	stream.recvErr = want
	err := pumpForwardConn(connRemote, stream)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestRunWorkspaceForwardRejectsBadInputs pins the validation
// TestRunWorkspaceForwardDefaultsWorkspaceFromConfig pins that an
// omitted workspace is filled from ./.daft/config.json: resolution
// succeeds and the command proceeds far enough to open the listener
// (here a stubbed listen error), rather than rejecting a missing arg.
func TestRunWorkspaceForwardDefaultsWorkspaceFromConfig(t *testing.T) {
	clearOrchestratorEnv(t)
	chdirClean(t)
	dlPath, err := dirLocalConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := saveFileConfig(dlPath, fileConfig{Name: "cfgws"}); err != nil {
		t.Fatal(err)
	}
	wantListen := errors.New("listen sentinel")
	deps := forwardDeps{
		dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
			return unreachableForwardClient{}, nil
		},
		listenLocal: func(string) (net.Listener, error) { return nil, wantListen },
	}
	err = runWorkspaceForwardWith(context.Background(), io.Discard,
		workspaceForwardOpts{configDir: t.TempDir(), localPort: 1, containerPort: 1, bind: e2eForwardLoopback}, deps)
	if !errors.Is(err, wantListen) {
		t.Errorf("err = %v, want listen sentinel (proves workspace resolved from config)", err)
	}
}

// TestRunWorkspaceForwardListenError pins the listen-failure
// branch: a deps.listenLocal that errors must surface wrapped
// without spawning any goroutines.
func TestRunWorkspaceForwardListenError(t *testing.T) {
	want := errors.New("synthetic listen fail")
	dialClient := unreachableForwardClient{}
	deps := forwardDeps{
		dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
			// Returning a non-nil client with a nil error keeps the
			// nilnil lint quiet; listenLocal errors out before
			// anything is dialed against it.
			return dialClient, nil
		},
		listenLocal: func(string) (net.Listener, error) { return nil, want },
	}
	err := runWorkspaceForwardWith(context.Background(), io.Discard,
		workspaceForwardOpts{
			orchestrator: "u", workspace: "ws", bind: e2eForwardLoopback,
			localPort: 1, containerPort: 1,
		}, deps)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestRunWorkspaceForwardDialError pins the dial-failure branch:
// a deps.dialOrchestrator that errors must surface verbatim
// before any listener is opened.
func TestRunWorkspaceForwardDialError(t *testing.T) {
	want := errors.New("synthetic dial fail")
	deps := forwardDeps{
		dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
			return nil, want
		},
		listenLocal: func(string) (net.Listener, error) {
			t.Fatal("listenLocal should not be called after dial fail")
			return nil, errors.New("unreachable")
		},
	}
	err := runWorkspaceForwardWith(context.Background(), io.Discard,
		workspaceForwardOpts{
			orchestrator: "u", workspace: "ws", bind: e2eForwardLoopback,
			localPort: 1, containerPort: 1,
		}, deps)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestAcceptForwardLoopExitsOnCtx pins the teardown surface: the
// accept loop must return nil after the listener is closed by
// the ctx-Done watchdog inside runWorkspaceForwardWith.
func TestAcceptForwardLoopExitsOnCtx(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- acceptForwardLoop(ctx, io.Discard, ln, nil,
			workspaceForwardOpts{workspace: "x", containerPort: 1}, forwardDeps{})
	}()
	_ = ln.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("acceptForwardLoop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acceptForwardLoop did not return within 2s")
	}
}

// echoStreamFactory returns a forwardDeps.openStream func that
// builds a fakeForwardClientStream whose sent Data frames are
// echoed back as inbound Data frames. Lets the test verify the
// full tunnel splice round-trips bytes.
func echoStreamFactory(t *testing.T) func(context.Context, workspacev1connect.WorkspaceCatalogServiceClient) forwardClientStream {
	t.Helper()
	return func(_ context.Context, _ workspacev1connect.WorkspaceCatalogServiceClient) forwardClientStream {
		s := newFakeForwardClientStream()
		go func() {
			deadline := time.Now().Add(3 * time.Second)
			lastIdx := 0
			//daft:lint:disable=forloopbound reason="bounded by 3s wall clock; CloseResponse-driven recv close also wakes any pending push"
			for time.Now().Before(deadline) {
				sent := s.sent()
				for i := lastIdx; i < len(sent); i++ {
					if d := sent[i].GetData(); len(d) > 0 {
						// Push may block if the receiver hasn't
						// drained; the 3s deadline above is the
						// safety net.
						select {
						case s.incoming <- &workspacev1.ForwardFrame{
							Body: &workspacev1.ForwardFrame_Data{Data: d},
						}:
						case <-time.After(time.Second):
							return
						}
					}
				}
				lastIdx = len(sent)
				time.Sleep(5 * time.Millisecond)
			}
		}()
		return s
	}
}

// TestRunWorkspaceForwardEndToEnd drives runWorkspaceForwardWith
// with an echo-loop stream factory, then connects a local TCP
// client and round-trips bytes. Covers acceptForwardLoop,
// runForwardTunnel, and the pump in one shot. The test seeds a
// free port by binding+closing once, then reuses it for the
// real forwarder run.
func TestRunWorkspaceForwardEndToEnd(t *testing.T) {
	// Pick a free port by binding once and closing — same trick
	// the e2e tests in nodectrlclient use. There's a small TOCTOU
	// window before the forwarder re-binds, accepted at test
	// scale.
	const loopback = "127.0.0.1"
	seed, err := net.Listen("tcp", loopback+":0")
	if err != nil {
		t.Fatalf("seed listen: %v", err)
	}
	tcpAddr, ok := seed.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("seed listener Addr not *net.TCPAddr: %T", seed.Addr())
	}
	chosenPort := tcpAddr.Port
	_ = seed.Close()
	if chosenPort < 1 || chosenPort > 65535 {
		t.Fatalf("OS-assigned port %d out of range", chosenPort)
	}
	// #nosec G115 — chosenPort is range-checked immediately above.
	chosen := uint32(chosenPort)

	deps := forwardDeps{
		dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
			return unreachableForwardClient{}, nil
		},
		listenLocal: func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) },
		openStream:  echoStreamFactory(t),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- runWorkspaceForwardWith(ctx, io.Discard,
			workspaceForwardOpts{
				orchestrator: "u", workspace: "ws", bind: loopback,
				localPort: chosen, containerPort: 80,
			}, deps)
	}()

	// Wait until the forwarder's listener is up, then dial it.
	var conn net.Conn
	dialDeadline := time.Now().Add(2 * time.Second)
	//daft:lint:disable=forloopbound reason="bounded by 2s wall clock; exits on first successful dial"
	for time.Now().Before(dialDeadline) {
		c, derr := net.Dial("tcp", net.JoinHostPort(loopback, fmt.Sprintf("%d", chosen)))
		if derr == nil {
			conn = c
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if conn == nil {
		t.Fatal("could not dial local forwarder within 2s")
	}
	defer func() { _ = conn.Close() }()

	want := []byte("ping-fwd")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("conn.Write: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("conn.Read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}

	_ = conn.Close()
	cancel()
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("runWorkspaceForwardWith did not return after cancel")
	}
}

// TestDefaultForwardDepsNonNil pins the production wiring path
// — every dep slot is populated so production code never
// dereferences nil. listenLocal is exercised against a free port
// to confirm the closure is reachable, not just non-nil.
func TestDefaultForwardDepsNonNil(t *testing.T) {
	deps := defaultForwardDeps()
	if deps.dialOrchestrator == nil {
		t.Error("dialOrchestrator is nil")
	}
	if deps.openStream == nil {
		t.Error("openStream is nil")
	}
	ln, err := deps.listenLocal("127.0.0.1:0")
	if err != nil {
		t.Fatalf("listenLocal: %v", err)
	}
	_ = ln.Close()
}

// TestDialWorkspaceCatalogForForwardConfigError pins the
// dialWorkspaceCatalogForForward seam by handing it a config
// dir override that can't load an identity. The error path is
// the only one unit-testable without standing up a real client
// identity; the happy path is covered transitively by the e2e
// rigs in nodectrlclient_test.
func TestDialWorkspaceCatalogForForwardConfigError(t *testing.T) {
	// A file path where a directory is expected → identity load
	// surfaces a clear error.
	blocker := t.TempDir() + "/notadir"
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	if _, err := dialWorkspaceCatalogForForward(blocker, "https://orch.invalid:8443"); err == nil {
		t.Error("expected error from identity load against a file path")
	}
}

// TestRunWorkspaceForwardPassthrough pins that the public
// runWorkspaceForward entrypoint forwards to runWorkspaceForwardWith
// with the production deps — covers the trivial wrapper line.
func TestRunWorkspaceForwardPassthrough(t *testing.T) {
	clearOrchestratorEnv(t)
	chdirClean(t)
	// A config dir that is actually a file makes orchestrator
	// resolution (the global-config read) fail fast, so the
	// production wrapper returns before opening any listener.
	blocker := t.TempDir() + "/notadir"
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	err := runWorkspaceForward(context.Background(), io.Discard,
		workspaceForwardOpts{configDir: blocker, localPort: 1, containerPort: 1})
	if err == nil {
		t.Error("expected error from production runWorkspaceForward")
	}
}

// TestNewWorkspaceForwardCmdShape pins the cobra command's
// surface so a future flag rename doesn't silently change the
// user-facing CLI.
func TestNewWorkspaceForwardCmdShape(t *testing.T) {
	cmd := newWorkspaceForwardCmd()
	if cmd.Use != forwardSubName+" <local>:<container> [workspace]" {
		t.Errorf("Use = %q", cmd.Use)
	}
	for _, flag := range []string{"orchestrator", "bind", configDirFlag} {
		if cmd.Flag(flag) == nil {
			t.Errorf("flag --%s not registered", flag)
		}
	}
	// Bad-positional-args test: the RunE must surface the
	// parsePortMappingArg error rather than crashing.
	cmd.SetArgs([]string{"bad-port-mapping", "ws"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil {
		t.Error("expected error for malformed port mapping arg")
	}
}

// TestPumpStreamToLocalSkipsEmptyData pins the empty-Data branch:
// a frame carrying no bytes is skipped without writing to conn.
// Same loop, next iteration.
func TestPumpStreamToLocalSkipsEmptyData(t *testing.T) {
	connLocal, connRemote := net.Pipe()
	t.Cleanup(func() { _ = connLocal.Close() })
	t.Cleanup(func() { _ = connRemote.Close() })
	stream := newFakeForwardClientStream()
	// Empty-Data first, then a real Data frame: the pump must
	// see the second frame's bytes on the local side.
	stream.push(&workspacev1.ForwardFrame{Body: &workspacev1.ForwardFrame_Data{Data: nil}})
	stream.push(&workspacev1.ForwardFrame{Body: &workspacev1.ForwardFrame_Data{Data: []byte("real")}})
	done := make(chan error, 1)
	go func() { done <- pumpStreamToLocal(connRemote, stream) }()
	got := make([]byte, 4)
	if _, err := io.ReadFull(connLocal, got); err != nil {
		t.Fatalf("connLocal.Read: %v", err)
	}
	if string(got) != "real" {
		t.Errorf("got %q, want real", got)
	}
	stream.closeIncoming()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("pumpStreamToLocal: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pumpStreamToLocal did not return within 2s")
	}
}

// TestPumpLocalToStreamSendError pins the send-error branch of the
// local→stream pump: a Send that errors must terminate the pump
// with the wrapped error.
func TestPumpLocalToStreamSendError(t *testing.T) {
	connLocal, connRemote := net.Pipe()
	t.Cleanup(func() { _ = connLocal.Close() })
	t.Cleanup(func() { _ = connRemote.Close() })
	want := errors.New("synthetic send fail")
	stream := newFakeForwardClientStream()
	stream.sendErr = want
	done := make(chan error, 1)
	go func() { done <- pumpLocalToStream(connRemote, stream) }()
	// Trigger the Send by writing bytes on the local side.
	_, _ = connLocal.Write([]byte("trigger"))
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Errorf("err = %v, want wrap of %v", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pumpLocalToStream did not return within 2s")
	}
}

// TestRunForwardTunnelNilStream pins the safety guard against a
// stream factory that returns nil. Returning an error rather than
// crashing keeps a misconfigured forwardDeps from segfaulting the
// daemon.
func TestRunForwardTunnelNilStream(t *testing.T) {
	connLocal, connRemote := net.Pipe()
	t.Cleanup(func() { _ = connLocal.Close() })
	deps := forwardDeps{
		openStream: func(context.Context, workspacev1connect.WorkspaceCatalogServiceClient) forwardClientStream {
			return nil
		},
	}
	err := runForwardTunnel(context.Background(), connRemote, unreachableForwardClient{},
		workspaceForwardOpts{workspace: "ws", containerPort: 80}, deps)
	if err == nil {
		t.Error("expected error from nil stream")
	}
}

// TestRunForwardTunnelHelloSendError pins the Hello-send failure
// branch: a stream whose first Send errors must surface that
// error without entering the byte pump.
func TestRunForwardTunnelHelloSendError(t *testing.T) {
	connLocal, connRemote := net.Pipe()
	t.Cleanup(func() { _ = connLocal.Close() })
	t.Cleanup(func() { _ = connRemote.Close() })
	want := errors.New("synthetic send fail")
	deps := forwardDeps{
		openStream: func(context.Context, workspacev1connect.WorkspaceCatalogServiceClient) forwardClientStream {
			s := newFakeForwardClientStream()
			s.sendErr = want
			return s
		},
	}
	err := runForwardTunnel(context.Background(), connRemote, unreachableForwardClient{},
		workspaceForwardOpts{workspace: "ws", containerPort: 80}, deps)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestPumpLocalToStreamSilentExitOnClosedPipe pins the
// io.ErrClosedPipe branch: a Read against an already-closed
// net.Pipe end returns ErrClosedPipe (not EOF), which the pump
// must treat as a clean exit rather than a real error.
func TestPumpLocalToStreamSilentExitOnClosedPipe(t *testing.T) {
	connLocal, connRemote := net.Pipe()
	// Close BOTH ends so Read returns immediately.
	_ = connLocal.Close()
	_ = connRemote.Close()
	stream := newFakeForwardClientStream()
	if err := pumpLocalToStream(connRemote, stream); err != nil {
		t.Errorf("pumpLocalToStream: %v, want nil for closed-pipe exit", err)
	}
}

// TestPumpStreamToLocalConnWriteClosed pins the write-against-
// closed-conn branch: when conn is closed mid-pump, the Write
// returns ErrClosedPipe (or net.ErrClosed) and the pump exits
// cleanly with nil.
func TestPumpStreamToLocalConnWriteClosed(t *testing.T) {
	_, connRemote := net.Pipe()
	_ = connRemote.Close()
	stream := newFakeForwardClientStream()
	stream.push(&workspacev1.ForwardFrame{Body: &workspacev1.ForwardFrame_Data{Data: []byte("doomed")}})
	if err := pumpStreamToLocal(connRemote, stream); err != nil {
		t.Errorf("pumpStreamToLocal: %v, want nil after closed-conn write", err)
	}
}

// TestPumpStreamToLocalConnWriteError pins the unexpected-write-
// error branch (i.e. an error that is NOT net.ErrClosed /
// io.ErrClosedPipe): the pump propagates the wrapped error.
func TestPumpStreamToLocalConnWriteError(t *testing.T) {
	wantErr := errors.New("synthetic write fail")
	stream := newFakeForwardClientStream()
	stream.push(&workspacev1.ForwardFrame{Body: &workspacev1.ForwardFrame_Data{Data: []byte("x")}})
	w := writeErrorConn{err: wantErr}
	if err := pumpStreamToLocal(w, stream); !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrap of %v", err, wantErr)
	}
}

// writeErrorConn is a net.Conn whose Write always returns a
// caller-supplied error. Used to drive the "unexpected write
// error" branch in pumpStreamToLocal.
type writeErrorConn struct {
	net.Conn
	err error
}

func (w writeErrorConn) Write([]byte) (int, error) { return 0, w.err }
func (w writeErrorConn) Read([]byte) (int, error)  { return 0, io.EOF }
func (w writeErrorConn) Close() error              { return nil }

// TestAcceptForwardLoopAcceptError pins the non-ErrClosed accept
// branch: a listener that returns an arbitrary accept error
// surfaces it wrapped.
func TestAcceptForwardLoopAcceptError(t *testing.T) {
	wantErr := errors.New("synthetic accept fail")
	ln := &errorListener{err: wantErr}
	err := acceptForwardLoop(context.Background(), io.Discard, ln, nil,
		workspaceForwardOpts{workspace: "x", containerPort: 1}, forwardDeps{})
	if !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrap of %v", err, wantErr)
	}
}

// errorListener is a net.Listener whose Accept always returns a
// caller-supplied error. Drives the unexpected-accept-error
// branch.
type errorListener struct{ err error }

func (l *errorListener) Accept() (net.Conn, error) { return nil, l.err }
func (l *errorListener) Close() error              { return nil }
func (l *errorListener) Addr() net.Addr            { return &net.TCPAddr{} }

// TestDefaultForwardDepsOpenStreamPanicsWithoutClient pins that
// the default openStream wrapper dispatches the client call —
// calling it with a nil client panics, which is what we want
// (misconfigured wiring surfaces immediately). Recovers the
// panic so the test passes when the dispatch is reached.
func TestDefaultForwardDepsOpenStreamPanicsWithoutClient(t *testing.T) {
	deps := defaultForwardDeps()
	defer func() { _ = recover() }()
	deps.openStream(context.Background(), nil)
	t.Error("expected panic from nil client dispatch")
}
