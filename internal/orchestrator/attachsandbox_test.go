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
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

// fakeAttachClientStream is an in-process attachServerStream for
// the AttachSandbox handler tests. Same incoming/outgoing shape
// as fakeForwardStream but typed for AttachFrame.
type fakeAttachClientStream struct {
	mu       sync.Mutex
	incoming chan *workspacev1.AttachFrame
	outgoing []*workspacev1.AttachFrame
	recvErr  error
}

func newFakeAttachClientStream() *fakeAttachClientStream {
	return &fakeAttachClientStream{incoming: make(chan *workspacev1.AttachFrame, 8)}
}

func (s *fakeAttachClientStream) Send(m *workspacev1.AttachFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outgoing = append(s.outgoing, m)
	return nil
}

func (s *fakeAttachClientStream) Receive() (*workspacev1.AttachFrame, error) {
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

func (s *fakeAttachClientStream) push(m *workspacev1.AttachFrame) { s.incoming <- m }
func (s *fakeAttachClientStream) closeIncoming()                  { close(s.incoming) }
func (s *fakeAttachClientStream) sent() []*workspacev1.AttachFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*workspacev1.AttachFrame, len(s.outgoing))
	copy(out, s.outgoing)
	return out
}

// fakeAttachSession wraps a net.Pipe end with a Resize call
// recorder so the tests can assert that resize frames flow
// through the orchestrator → bridger seam.
type fakeAttachSession struct {
	net.Conn
	mu      sync.Mutex
	resizes []struct{ cols, rows uint32 }
	rerr    error
}

func (s *fakeAttachSession) Resize(cols, rows uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rerr != nil {
		return s.rerr
	}
	s.resizes = append(s.resizes, struct{ cols, rows uint32 }{cols, rows})
	return nil
}

func (s *fakeAttachSession) sentResizes() []struct{ cols, rows uint32 } {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]struct{ cols, rows uint32 }, len(s.resizes))
	copy(out, s.resizes)
	return out
}

// fakeAttachBridger satisfies WorkspaceAttachBridger: each
// OpenAttachBridge call returns the next pre-paired
// fakeAttachSession or the configured err. Captures the bridger
// request for assertions.
type fakeAttachBridger struct {
	mu       sync.Mutex
	calls    int
	lastReq  sandboxgateway.AttachRequest
	sessions []sandboxgateway.AttachSession
	err      error
}

func (b *fakeAttachBridger) OpenAttachBridge(_ context.Context, _ identity.Fingerprint, req sandboxgateway.AttachRequest) (sandboxgateway.AttachSession, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	b.lastReq = req
	if b.err != nil {
		return nil, b.err
	}
	if len(b.sessions) == 0 {
		return nil, errors.New("fakeAttachBridger: out of sessions")
	}
	s := b.sessions[0]
	b.sessions = b.sessions[1:]
	return s, nil
}

// newAttachTestHandlerWithWorkspace builds a WorkspaceCatalogHandler
// with a RUNNING workspace, returning the handler, UUID, and owner
// fingerprint for the test to drive runAttachSandbox with.
//
//nolint:dupl // structurally parallel to newForwardingTestHandler in forwardport_test.go; the bridger interface differs (Port vs Attach) and the side-by-side review is the intent.
func newAttachTestHandlerWithWorkspace(t *testing.T, br WorkspaceAttachBridger) (*WorkspaceCatalogHandler, string, identity.Fingerprint) {
	t.Helper()
	cat := workspacecatalog.NewStore(nil)
	h, err := NewWorkspaceCatalogHandler(cat, nil)
	if err != nil {
		t.Fatalf("NewWorkspaceCatalogHandler: %v", err)
	}
	h.SetAttachBridger(br)
	owner := identity.Fingerprint{0xaa, 0xbb}
	uid := "ws-uuid-attach"
	if _, err := cat.Create(workspacecatalog.CreateInput{
		UUID: uid, Name: "attach-ws", OwnerFingerprint: owner,
	}); err != nil {
		t.Fatalf("catalog.Create: %v", err)
	}
	if _, err := cat.Update(uid, workspacecatalog.MutateStatus,
		func(ws *workspacecatalog.Workspace) error {
			ws.Status.CurrentSandbox = &workspacecatalog.SandboxRef{
				UID:             "sb-running",
				NodeFingerprint: identity.Fingerprint{0xcc},
			}
			return nil
		}); err != nil {
		t.Fatalf("catalog.Update: %v", err)
	}
	return h, uid, owner
}

func attachHelloFrame(uuid string) *workspacev1.AttachFrame {
	return &workspacev1.AttachFrame{
		Body: &workspacev1.AttachFrame_Hello{
			Hello: &workspacev1.AttachHello{
				Workspace: &workspacev1.WorkspaceHandle{Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: uuid}},
				Command:   []string{testAttachShell},
				Tty:       true,
				Cols:      80, Rows: 24,
			},
		},
	}
}

// TestRunAttachSandboxSplicesBytesAndResize pins the happy path:
// Hello frame routes to OpenAttachBridge; a Data frame from the
// client lands on the session; a Resize frame propagates via
// session.Resize; bytes from the session come back as Data frames.
func TestRunAttachSandboxSplicesBytesAndResize(t *testing.T) {
	sessLocal, sessRemote := net.Pipe()
	t.Cleanup(func() { _ = sessLocal.Close() })
	session := &fakeAttachSession{Conn: sessRemote}
	br := &fakeAttachBridger{sessions: []sandboxgateway.AttachSession{session}}
	h, uid, _ := newAttachTestHandlerWithWorkspace(t, br)

	stream := newFakeAttachClientStream()
	stream.push(attachHelloFrame(uid))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- h.runAttachSandbox(ctx, identity.Fingerprint{}, stream) }()

	// client → session: bytes
	stream.push(&workspacev1.AttachFrame{
		Body: &workspacev1.AttachFrame_Data{Data: []byte("typed")},
	})
	got := make([]byte, len("typed"))
	if _, err := io.ReadFull(sessLocal, got); err != nil {
		t.Fatalf("sessLocal.Read: %v", err)
	}
	if !bytes.Equal(got, []byte("typed")) {
		t.Errorf("session got %q, want typed", got)
	}

	// client → session: resize
	stream.push(&workspacev1.AttachFrame{
		Body: &workspacev1.AttachFrame_Resize{
			Resize: &workspacev1.AttachResize{Cols: 132, Rows: 50},
		},
	})

	// session → client: bytes come back as AttachFrame{data} on the stream
	go func() { _, _ = sessLocal.Write([]byte("output")) }()
	deadline := time.Now().Add(2 * time.Second)
	//daft:lint:disable=forloopbound reason="bounded 2s; exits when matching Data frame is observed"
	for time.Now().Before(deadline) {
		sent := stream.sent()
		if len(sent) > 0 && bytes.Equal(sent[0].GetData(), []byte("output")) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(stream.sent()) == 0 {
		t.Fatal("no frames sent within 2s")
	}

	// Wait briefly for the resize to land on the session (pump
	// goroutine is async).
	deadline2 := time.Now().Add(2 * time.Second)
	//daft:lint:disable=forloopbound reason="bounded 2s; exits when resize observed on session"
	for time.Now().Before(deadline2) {
		if len(session.sentResizes()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rs := session.sentResizes(); len(rs) != 1 || rs[0].cols != 132 || rs[0].rows != 50 {
		t.Errorf("resizes = %+v, want one entry cols=132 rows=50", rs)
	}

	// Tear down.
	stream.closeIncoming()
	_ = sessLocal.Close()
	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("runAttachSandbox: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runAttachSandbox did not return within 2s")
	}

	if br.calls != 1 || br.lastReq.SandboxUID != "sb-running" ||
		!br.lastReq.TTY || br.lastReq.Cols != 80 || br.lastReq.Rows != 24 {
		t.Errorf("bridger calls=%d req=%+v, want sb-running tty=true 80x24", br.calls, br.lastReq)
	}
}

// TestRunAttachSandboxRejectsBadHello pins the protocol-violation
// branch.
func TestRunAttachSandboxRejectsBadHello(t *testing.T) {
	br := &fakeAttachBridger{}
	h, _, _ := newAttachTestHandlerWithWorkspace(t, br)
	for _, tc := range []struct {
		name  string
		first *workspacev1.AttachFrame
	}{
		{testBadHelloDataFirst, &workspacev1.AttachFrame{
			Body: &workspacev1.AttachFrame_Data{Data: []byte("nope")},
		}},
		{"hello empty workspace", &workspacev1.AttachFrame{
			Body: &workspacev1.AttachFrame_Hello{Hello: &workspacev1.AttachHello{}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := newFakeAttachClientStream()
			stream.push(tc.first)
			err := h.runAttachSandbox(context.Background(), identity.Fingerprint{}, stream)
			if !errors.Is(err, ErrAttachHelloMissing) {
				t.Errorf("err = %v, want ErrAttachHelloMissing", err)
			}
			if br.calls != 0 {
				t.Errorf("bridger called %d times; want 0", br.calls)
			}
		})
	}
}

// TestRunAttachSandboxNotRunning pins the readiness gate: a
// workspace whose Status.CurrentSandbox is nil yields
// ErrAttachWorkspaceNotRunning.
func TestRunAttachSandboxNotRunning(t *testing.T) {
	br := &fakeAttachBridger{}
	cat := workspacecatalog.NewStore(nil)
	h, err := NewWorkspaceCatalogHandler(cat, nil)
	if err != nil {
		t.Fatalf("NewWorkspaceCatalogHandler: %v", err)
	}
	h.SetAttachBridger(br)
	owner := identity.Fingerprint{0xaa}
	uid := "ws-not-running-attach"
	if _, err := cat.Create(workspacecatalog.CreateInput{
		UUID: uid, Name: "n", OwnerFingerprint: owner,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	stream := newFakeAttachClientStream()
	stream.push(attachHelloFrame(uid))
	got := h.runAttachSandbox(context.Background(), identity.Fingerprint{}, stream)
	if !errors.Is(got, ErrAttachWorkspaceNotRunning) {
		t.Errorf("err = %v, want ErrAttachWorkspaceNotRunning", got)
	}
	if br.calls != 0 {
		t.Errorf("bridger called %d times; want 0", br.calls)
	}
}

// TestRunAttachSandboxNoBridger pins the Unimplemented branch.
func TestRunAttachSandboxNoBridger(t *testing.T) {
	cat := workspacecatalog.NewStore(nil)
	h, err := NewWorkspaceCatalogHandler(cat, nil)
	if err != nil {
		t.Fatalf("NewWorkspaceCatalogHandler: %v", err)
	}
	stream := newFakeAttachClientStream()
	stream.push(attachHelloFrame("any"))
	got := h.runAttachSandbox(context.Background(), identity.Fingerprint{}, stream)
	if got == nil {
		t.Fatal("expected error when no AttachBridger configured")
	}
}

// TestRunAttachSandboxBridgerError pins the bridger-failure branch.
func TestRunAttachSandboxBridgerError(t *testing.T) {
	br := &fakeAttachBridger{err: errors.New("synthetic bridge fail")}
	h, uid, _ := newAttachTestHandlerWithWorkspace(t, br)
	stream := newFakeAttachClientStream()
	stream.push(attachHelloFrame(uid))
	err := h.runAttachSandbox(context.Background(), identity.Fingerprint{}, stream)
	if err == nil {
		t.Error("expected error when bridger fails")
	}
}

// TestRunAttachSandboxUnknownWorkspace pins resolveAttachWorkspace's
// error branch: a Hello pointing at an unknown UUID fails with a
// catalog NotFound mapping.
func TestRunAttachSandboxUnknownWorkspace(t *testing.T) {
	br := &fakeAttachBridger{}
	h, _, _ := newAttachTestHandlerWithWorkspace(t, br)
	stream := newFakeAttachClientStream()
	stream.push(attachHelloFrame("ws-does-not-exist"))
	err := h.runAttachSandbox(context.Background(), identity.Fingerprint{}, stream)
	if err == nil {
		t.Error("expected error for unknown workspace UUID")
	}
	if br.calls != 0 {
		t.Errorf("bridger called %d times; want 0 on unknown workspace", br.calls)
	}
}

// TestRunAttachSandboxFirstReceiveError pins the transport-error
// path before the Hello.
func TestRunAttachSandboxFirstReceiveError(t *testing.T) {
	br := &fakeAttachBridger{}
	h, _, _ := newAttachTestHandlerWithWorkspace(t, br)
	stream := newFakeAttachClientStream()
	stream.recvErr = errors.New("synthetic recv fail")
	err := h.runAttachSandbox(context.Background(), identity.Fingerprint{}, stream)
	if err == nil || !errors.Is(err, stream.recvErr) {
		t.Errorf("err = %v, want wrap of recvErr", err)
	}
}

// stubAttachSession is a minimal AttachSession with injectable
// errors on Write / Resize for the pump error-path tests. Read
// blocks on a channel so the SessionToClient half can be
// quiesced.
type stubAttachSession struct {
	mu     sync.Mutex
	wErr   error
	rErr   error
	rzErr  error
	readCh chan []byte
	closed bool
}

func newStubAttachSession() *stubAttachSession {
	return &stubAttachSession{readCh: make(chan []byte, 4)}
}

func (s *stubAttachSession) Read(p []byte) (int, error) {
	s.mu.Lock()
	rErr := s.rErr
	s.mu.Unlock()
	if rErr != nil {
		return 0, rErr
	}
	b, ok := <-s.readCh
	if !ok {
		return 0, io.EOF
	}
	return copy(p, b), nil
}

func (s *stubAttachSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wErr != nil {
		return 0, s.wErr
	}
	return len(p), nil
}

func (s *stubAttachSession) Resize(_, _ uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rzErr
}

func (s *stubAttachSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.readCh)
	return nil
}

func (s *stubAttachSession) LocalAddr() net.Addr               { return nil }
func (s *stubAttachSession) RemoteAddr() net.Addr              { return nil }
func (s *stubAttachSession) SetDeadline(_ time.Time) error     { return nil }
func (s *stubAttachSession) SetReadDeadline(_ time.Time) error { return nil }
func (s *stubAttachSession) SetWriteDeadline(_ time.Time) error {
	return nil
}

// TestPumpAttachClientToSessionWriteError pins that a non-clean
// session.Write error propagates out of the client→session
// pump rather than being swallowed by isAttachCleanExit.
func TestPumpAttachClientToSessionWriteError(t *testing.T) {
	want := errors.New("synthetic write fail")
	stream := newFakeAttachClientStream()
	sess := newStubAttachSession()
	sess.wErr = want
	stream.push(&workspacev1.AttachFrame{
		Body: &workspacev1.AttachFrame_Data{Data: []byte("payload")},
	})
	err := pumpAttachClientToSession(stream, sess)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestPumpAttachClientToSessionResizeError pins the resize-error
// propagation branch.
func TestPumpAttachClientToSessionResizeError(t *testing.T) {
	want := errors.New("synthetic resize fail")
	stream := newFakeAttachClientStream()
	sess := newStubAttachSession()
	sess.rzErr = want
	stream.push(&workspacev1.AttachFrame{
		Body: &workspacev1.AttachFrame_Resize{
			Resize: &workspacev1.AttachResize{Cols: 80, Rows: 24},
		},
	})
	err := pumpAttachClientToSession(stream, sess)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestPumpAttachClientToSessionIgnoresStrayHello pins that a
// stray Hello frame after the initial Receive doesn't tear the
// pump down.
func TestPumpAttachClientToSessionIgnoresStrayHello(t *testing.T) {
	stream := newFakeAttachClientStream()
	sess := newStubAttachSession()
	// Push a Hello then close the incoming chan; the pump should
	// loop past the Hello and then exit cleanly on io.EOF.
	stream.push(&workspacev1.AttachFrame{
		Body: &workspacev1.AttachFrame_Hello{Hello: &workspacev1.AttachHello{}},
	})
	stream.closeIncoming()
	if err := pumpAttachClientToSession(stream, sess); err != nil {
		t.Errorf("err = %v, want nil clean exit", err)
	}
}

// TestPumpAttachSessionToClientReadError pins that a non-clean
// session.Read error propagates verbatim out of the session→client
// pump rather than being swallowed by isAttachCleanExit.
func TestPumpAttachSessionToClientReadError(t *testing.T) {
	want := errors.New("synthetic read fail")
	sess := newStubAttachSession()
	sess.rErr = want
	stream := newFakeAttachClientStream()
	err := pumpAttachSessionToClient(stream, sess)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestPumpAttachSessionToClientSendError pins that a non-clean
// stream.Send error propagates out of the session→client pump.
func TestPumpAttachSessionToClientSendError(t *testing.T) {
	want := errors.New("synthetic send fail")
	sessLocal, sessRemote := net.Pipe()
	t.Cleanup(func() { _ = sessLocal.Close() })
	sess := &fakeAttachSession{Conn: sessRemote}
	stream := &errorSendingAttachStream{err: want}
	go func() { _, _ = sessLocal.Write([]byte("data")) }()
	err := pumpAttachSessionToClient(stream, sess)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// errorSendingAttachStream is an attachServerStream whose Send
// always returns err. Receive blocks until Close.
type errorSendingAttachStream struct {
	err error
	mu  sync.Mutex
	rch chan struct{}
}

func (s *errorSendingAttachStream) Send(*workspacev1.AttachFrame) error { return s.err }
func (s *errorSendingAttachStream) Receive() (*workspacev1.AttachFrame, error) {
	s.mu.Lock()
	if s.rch == nil {
		s.rch = make(chan struct{})
	}
	ch := s.rch
	s.mu.Unlock()
	<-ch
	return nil, io.EOF
}

// TestAttachSandboxConnectE2E pins the Connect-level entry point.
// A real bidi RPC routes through PeerFingerprintFromRequestHeader
// → runAttachSandbox and observes bytes round-tripping via the
// fake bridger.
func TestAttachSandboxConnectE2E(t *testing.T) {
	cat := workspacecatalog.NewStore(nil)
	sessLocal, sessRemote := net.Pipe()
	t.Cleanup(func() { _ = sessLocal.Close() })
	go func() {
		buf := make([]byte, 64)
		//daft:lint:disable=forloopbound reason="exits on sessLocal close"
		for {
			n, err := sessLocal.Read(buf)
			if err != nil {
				return
			}
			if _, werr := sessLocal.Write(buf[:n]); werr != nil {
				return
			}
		}
	}()
	session := &fakeAttachSession{Conn: sessRemote}
	br := &fakeAttachBridger{sessions: []sandboxgateway.AttachSession{session}}

	env := newOrchTestEnv(t, identity.RoleClient, Config{
		WorkspaceCatalog:       cat,
		WorkspaceAttachBridger: br,
	})
	owner := env.callerID.Fingerprint()
	uid := "ws-connect-attach"
	if _, err := cat.Create(workspacecatalog.CreateInput{
		UUID: uid, Name: "attach", OwnerFingerprint: owner,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := cat.Update(uid, workspacecatalog.MutateStatus,
		func(ws *workspacecatalog.Workspace) error {
			ws.Status.CurrentSandbox = &workspacecatalog.SandboxRef{
				UID: "sb-r", NodeFingerprint: identity.Fingerprint{0xcc},
			}
			return nil
		}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	client := workspacev1connect.NewWorkspaceCatalogServiceClient(env.httpc, env.baseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := client.AttachSandbox(ctx)

	if err := stream.Send(attachHelloFrame(uid)); err != nil {
		t.Fatalf("Send Hello: %v", err)
	}

	want := []byte("attach-e2e")
	if err := stream.Send(&workspacev1.AttachFrame{
		Body: &workspacev1.AttachFrame_Data{Data: want},
	}); err != nil {
		t.Fatalf("Send Data: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	//daft:lint:disable=forloopbound reason="bounded 3s; exits when matching Data is read back"
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

	if br.calls != 1 || br.lastReq.SandboxUID != "sb-r" {
		t.Errorf("bridger calls=%d req=%+v, want one call to sb-r", br.calls, br.lastReq)
	}
}
