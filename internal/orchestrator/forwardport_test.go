package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

// fakeForwardStream is an in-process forwardServerStream for
// tests: inbound frames live on `incoming`, outbound on
// `outgoing`. Mirrors fakeServerStream / fakePortStream but typed
// for ForwardFrame.
type fakeForwardStream struct {
	mu       sync.Mutex
	incoming chan *workspacev1.ForwardFrame
	outgoing []*workspacev1.ForwardFrame
	recvErr  error
}

func newFakeForwardStream() *fakeForwardStream {
	return &fakeForwardStream{incoming: make(chan *workspacev1.ForwardFrame, 8)}
}

func (s *fakeForwardStream) Send(m *workspacev1.ForwardFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outgoing = append(s.outgoing, m)
	return nil
}

func (s *fakeForwardStream) Receive() (*workspacev1.ForwardFrame, error) {
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

func (s *fakeForwardStream) push(m *workspacev1.ForwardFrame) { s.incoming <- m }
func (s *fakeForwardStream) closeIncoming()                   { close(s.incoming) }
func (s *fakeForwardStream) sent() []*workspacev1.ForwardFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*workspacev1.ForwardFrame, len(s.outgoing))
	copy(out, s.outgoing)
	return out
}

// fakeBridger satisfies WorkspacePortBridger: each OpenPortBridge
// call returns the next pre-paired net.Conn or the configured err.
type fakeBridger struct {
	mu       sync.Mutex
	calls    int
	wsSeen   string
	portSeen uint32
	conns    []net.Conn
	err      error
}

func (b *fakeBridger) OpenPortBridge(_ context.Context, _ identity.Fingerprint, ws string, port uint32) (net.Conn, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.wsSeen = ws
	b.portSeen = port
	if b.err != nil {
		return nil, b.err
	}
	if len(b.conns) == 0 {
		return nil, errors.New("fakeBridger: out of conns")
	}
	c := b.conns[0]
	b.conns = b.conns[1:]
	return c, nil
}

// newForwardingTestHandler builds a WorkspaceCatalogHandler with
// a workspace UUID + a RUNNING status (current_sandbox set) so
// runForwardPort's catalog lookup + readiness gate both pass.
func newForwardingTestHandler(t *testing.T, bridger WorkspacePortBridger) (*WorkspaceCatalogHandler, string, identity.Fingerprint) {
	t.Helper()
	cat := workspacecatalog.NewStore(nil)
	h, err := NewWorkspaceCatalogHandler(cat, nil)
	if err != nil {
		t.Fatalf("NewWorkspaceCatalogHandler: %v", err)
	}
	h.SetPortBridger(bridger)
	owner := identity.Fingerprint{0xaa, 0xbb}
	uid := "ws-uuid-fwd"
	if _, err := cat.Create(workspacecatalog.CreateInput{
		UUID:             uid,
		Name:             "test-forward",
		OwnerFingerprint: owner,
	}); err != nil {
		t.Fatalf("catalog.Create: %v", err)
	}
	// Mark the workspace RUNNING by stamping CurrentSandbox.
	if _, err := cat.Update(uid, workspacecatalog.MutateStatus,
		func(ws *workspacecatalog.Workspace) error {
			ws.Status.CurrentSandbox = &workspacecatalog.SandboxRef{
				UID:             "sb-1",
				NodeFingerprint: identity.Fingerprint{0xcc},
			}
			return nil
		}); err != nil {
		t.Fatalf("catalog.Update: %v", err)
	}
	return h, uid, owner
}

// helloFrame builds a Hello frame addressed at (uuid, port).
func helloFrame(uuid string, port uint32) *workspacev1.ForwardFrame {
	return &workspacev1.ForwardFrame{
		Body: &workspacev1.ForwardFrame_Hello{
			Hello: &workspacev1.ForwardHello{
				Workspace:     &workspacev1.WorkspaceHandle{Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: uuid}},
				ContainerPort: port,
			},
		},
	}
}

// TestRunForwardPortSplicesEndToEnd pins the happy path: the
// Hello frame steers the lookup, OpenPortBridge returns a
// net.Pipe-backed conn, and bytes flowing each direction land
// where expected.
func TestRunForwardPortSplicesEndToEnd(t *testing.T) {
	dsLocal, dsRemote := net.Pipe()
	t.Cleanup(func() { _ = dsLocal.Close() })
	br := &fakeBridger{conns: []net.Conn{dsRemote}}
	h, uid, _ := newForwardingTestHandler(t, br)

	stream := newFakeForwardStream()
	stream.push(helloFrame(uid, 80))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() {
		runDone <- h.runForwardPort(ctx, identity.Fingerprint{}, stream)
	}()

	// upstream → downstream: a Data frame pushed on the stream is
	// io.Copy'd to dsLocal.
	stream.push(&workspacev1.ForwardFrame{Body: &workspacev1.ForwardFrame_Data{Data: []byte("to-vm")}})
	gotDS := make([]byte, len("to-vm"))
	if _, err := io.ReadFull(dsLocal, gotDS); err != nil {
		t.Fatalf("dsLocal.Read: %v", err)
	}
	if !bytes.Equal(gotDS, []byte("to-vm")) {
		t.Errorf("dsLocal got %q, want to-vm", gotDS)
	}

	// downstream → upstream: bytes written on dsLocal are sent as
	// Data frames on the stream.
	go func() { _, _ = dsLocal.Write([]byte("from-vm")) }()
	deadline := time.Now().Add(2 * time.Second)
	//daft:lint:disable=forloopbound reason="bounded by 2s wall clock; exits when the expected Data frame is observed"
	for time.Now().Before(deadline) {
		sent := stream.sent()
		if len(sent) > 0 && bytes.Equal(sent[0].GetData(), []byte("from-vm")) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(stream.sent()) == 0 {
		t.Fatal("no frames sent within 2s")
	}

	// Close both sides; runForwardPort returns.
	stream.closeIncoming()
	_ = dsLocal.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("runForwardPort: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runForwardPort did not return within 2s")
	}
	if br.wsSeen != uid || br.portSeen != 80 {
		t.Errorf("bridger seen ws=%q port=%d, want %q 80", br.wsSeen, br.portSeen, uid)
	}
}

// TestRunForwardPortRejectsBadHello pins the protocol-violation
// branch: a first frame that's not a Hello (or has missing
// fields) is refused with InvalidArgument.
func TestRunForwardPortRejectsBadHello(t *testing.T) {
	br := &fakeBridger{}
	h, _, _ := newForwardingTestHandler(t, br)
	for _, tc := range []struct {
		name  string
		first *workspacev1.ForwardFrame
	}{
		{"data first", &workspacev1.ForwardFrame{
			Body: &workspacev1.ForwardFrame_Data{Data: []byte("nope")},
		}},
		{"hello empty handle", &workspacev1.ForwardFrame{
			Body: &workspacev1.ForwardFrame_Hello{Hello: &workspacev1.ForwardHello{ContainerPort: 80}},
		}},
		{"hello zero port", helloFrame("ws-uuid-fwd", 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := newFakeForwardStream()
			stream.push(tc.first)
			err := h.runForwardPort(context.Background(), identity.Fingerprint{}, stream)
			if !errors.Is(err, ErrForwardHelloMissing) {
				t.Errorf("err = %v, want ErrForwardHelloMissing", err)
			}
			if br.calls != 0 {
				t.Errorf("bridger called %d times; want 0", br.calls)
			}
		})
	}
}

// TestRunForwardPortNotRunning pins the readiness gate: a
// workspace whose Status.CurrentSandbox is nil yields
// ErrForwardWorkspaceNotRunning before the bridger is touched.
func TestRunForwardPortNotRunning(t *testing.T) {
	br := &fakeBridger{}
	// Build a handler whose workspace is NOT RUNNING.
	cat := workspacecatalog.NewStore(nil)
	h, err := NewWorkspaceCatalogHandler(cat, nil)
	if err != nil {
		t.Fatalf("NewWorkspaceCatalogHandler: %v", err)
	}
	h.SetPortBridger(br)
	owner := identity.Fingerprint{0xaa}
	uid := "ws-not-running"
	if _, err := cat.Create(workspacecatalog.CreateInput{
		UUID: uid, Name: "n", OwnerFingerprint: owner,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	stream := newFakeForwardStream()
	stream.push(helloFrame(uid, 80))
	got := h.runForwardPort(context.Background(), identity.Fingerprint{}, stream)
	if !errors.Is(got, ErrForwardWorkspaceNotRunning) {
		t.Errorf("err = %v, want ErrForwardWorkspaceNotRunning", got)
	}
	if br.calls != 0 {
		t.Errorf("bridger called %d times; want 0", br.calls)
	}
}

// TestRunForwardPortNoBridger pins the Unimplemented branch when
// no PortBridger is configured — the catalog-only test-rig
// scenario.
func TestRunForwardPortNoBridger(t *testing.T) {
	cat := workspacecatalog.NewStore(nil)
	h, err := NewWorkspaceCatalogHandler(cat, nil)
	if err != nil {
		t.Fatalf("NewWorkspaceCatalogHandler: %v", err)
	}
	stream := newFakeForwardStream()
	stream.push(helloFrame("anything", 80))
	got := h.runForwardPort(context.Background(), identity.Fingerprint{}, stream)
	if got == nil {
		t.Fatal("expected error when no PortBridger configured")
	}
}

// TestRunForwardPortBridgerError pins the bridger-failure branch:
// an OpenPortBridge error propagates wrapped via mapCatalogErr.
func TestRunForwardPortBridgerError(t *testing.T) {
	br := &fakeBridger{err: errors.New("synthetic bridge fail")}
	h, uid, _ := newForwardingTestHandler(t, br)
	stream := newFakeForwardStream()
	stream.push(helloFrame(uid, 80))
	err := h.runForwardPort(context.Background(), identity.Fingerprint{}, stream)
	if err == nil {
		t.Error("expected error when bridger fails")
	}
}

// TestRunForwardPortFirstReceiveError pins the transport-error
// path: a Receive failure before the Hello arrives surfaces
// wrapped rather than silently consumed.
func TestRunForwardPortFirstReceiveError(t *testing.T) {
	br := &fakeBridger{}
	h, _, _ := newForwardingTestHandler(t, br)
	stream := newFakeForwardStream()
	stream.recvErr = errors.New("synthetic recv fail")
	err := h.runForwardPort(context.Background(), identity.Fingerprint{}, stream)
	if err == nil || !errors.Is(err, stream.recvErr) {
		t.Errorf("err = %v, want wrap of recvErr", err)
	}
}

// TestForwardFrameConnIdempotentClose and TestForwardFrameConnAddrShape
// pin the small conn adapter surface that splice depends on.
func TestForwardFrameConnIdempotentClose(t *testing.T) {
	c := newForwardFrameConn(newFakeForwardStream())
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestForwardFrameConnAddrShape(t *testing.T) {
	c := newForwardFrameConn(newFakeForwardStream())
	if c.LocalAddr().Network() != "daft-forward-port" {
		t.Errorf("Network = %q", c.LocalAddr().Network())
	}
	if c.LocalAddr().String() != "daft-forward-port:local" {
		t.Errorf("LocalAddr = %q", c.LocalAddr())
	}
	if c.RemoteAddr().String() != "daft-forward-port:remote" {
		t.Errorf("RemoteAddr = %q", c.RemoteAddr())
	}
	now := time.Now()
	if err := c.SetDeadline(now); err != nil {
		t.Errorf("SetDeadline: %v", err)
	}
	if err := c.SetReadDeadline(now); err != nil {
		t.Errorf("SetReadDeadline: %v", err)
	}
	if err := c.SetWriteDeadline(now); err != nil {
		t.Errorf("SetWriteDeadline: %v", err)
	}
}

func TestForwardFrameConnWriteAfterCloseErrsClosed(t *testing.T) {
	c := newForwardFrameConn(newFakeForwardStream())
	_ = c.Close()
	_, err := c.Write([]byte("x"))
	if !errors.Is(err, net.ErrClosed) {
		t.Errorf("err = %v, want net.ErrClosed", err)
	}
}

// TestForwardPortConnectE2E pins the Connect-level entry point
// (the line of code that extracts the peer fingerprint and calls
// runForwardPort): a real Connect bidi stream from a TOFU-approved
// client gets a Hello frame through, the fake bridger observes the
// expected workspace+port, and bytes round-trip via the splice.
func TestForwardPortConnectE2E(t *testing.T) {
	cat := workspacecatalog.NewStore(nil)
	dsLocal, dsRemote := net.Pipe()
	t.Cleanup(func() { _ = dsLocal.Close() })
	go func() {
		// Echo loop: bytes read on dsLocal are written back.
		buf := make([]byte, 64)
		//daft:lint:disable=forloopbound reason="exits on dsLocal close (Read returns err)"
		for {
			n, err := dsLocal.Read(buf)
			if err != nil {
				return
			}
			if _, werr := dsLocal.Write(buf[:n]); werr != nil {
				return
			}
		}
	}()
	br := &fakeBridger{conns: []net.Conn{dsRemote}}

	env := newOrchTestEnv(t, identity.RoleClient, Config{
		WorkspaceCatalog:     cat,
		WorkspacePortBridger: br,
	})
	// Seed a RUNNING workspace owned by the caller fingerprint so
	// the by-name resolution scope check passes too — UUID lookups
	// don't strictly require it, but the test exercises both.
	owner := env.callerID.Fingerprint()
	uid := "ws-connect-fwd"
	if _, err := cat.Create(workspacecatalog.CreateInput{
		UUID: uid, Name: "fwd", OwnerFingerprint: owner,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := cat.Update(uid, workspacecatalog.MutateStatus,
		func(ws *workspacecatalog.Workspace) error {
			ws.Status.CurrentSandbox = &workspacecatalog.SandboxRef{
				UID: "sb-1", NodeFingerprint: identity.Fingerprint{0xcc},
			}
			return nil
		}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	client := workspacev1connect.NewWorkspaceCatalogServiceClient(env.httpc, env.baseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := client.ForwardPort(ctx)

	if err := stream.Send(helloFrame(uid, 80)); err != nil {
		t.Fatalf("Send Hello: %v", err)
	}

	// Round-trip a byte: write Data frame, read it back through the
	// echo loop.
	want := []byte("ping-fwd")
	if err := stream.Send(&workspacev1.ForwardFrame{
		Body: &workspacev1.ForwardFrame_Data{Data: want},
	}); err != nil {
		t.Fatalf("Send Data: %v", err)
	}
	// The first frame back may be a Data frame.
	deadline := time.Now().Add(3 * time.Second)
	//daft:lint:disable=forloopbound reason="bounded by 3s wall clock; exits when matching Data frame is observed"
	for time.Now().Before(deadline) {
		msg, err := stream.Receive()
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		if bytes.Equal(msg.GetData(), want) {
			break
		}
	}
	_ = stream.CloseRequest()
	_ = stream.CloseResponse()

	if br.calls != 1 || br.wsSeen != uid || br.portSeen != 80 {
		t.Errorf("bridger calls=%d ws=%q port=%d, want 1 %q 80", br.calls, br.wsSeen, br.portSeen, uid)
	}
}
