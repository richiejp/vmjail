package nodectrlclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
)

const (
	testAttachSession = "sess-attach"
	testAttachSandbox = "sb-attach"
)

// fakeAttachStream is the in-process attachStreamRPC counterpart
// to fakePortStream, typed for AttachStreamMessage.
type fakeAttachStream struct {
	mu       sync.Mutex
	incoming chan *nodectrlv1.AttachStreamMessage
	outgoing []*nodectrlv1.AttachStreamMessage
	closeReq bool
	closeRsp bool
	recvErr  error
	sendErr  error
}

func newFakeAttachStream() *fakeAttachStream {
	return &fakeAttachStream{incoming: make(chan *nodectrlv1.AttachStreamMessage, 4)}
}

func (s *fakeAttachStream) Send(m *nodectrlv1.AttachStreamMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.outgoing = append(s.outgoing, m)
	return nil
}

func (s *fakeAttachStream) Receive() (*nodectrlv1.AttachStreamMessage, error) {
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

func (s *fakeAttachStream) CloseRequest() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeReq = true
	return nil
}

func (s *fakeAttachStream) CloseResponse() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closeRsp {
		return nil
	}
	s.closeRsp = true
	close(s.incoming)
	return nil
}

func (s *fakeAttachStream) push(m *nodectrlv1.AttachStreamMessage) { s.incoming <- m }
func (s *fakeAttachStream) sent() []*nodectrlv1.AttachStreamMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*nodectrlv1.AttachStreamMessage, len(s.outgoing))
	copy(out, s.outgoing)
	return out
}

// fakeAttachSession is the bridger-returned AttachSession seen
// from the sandboxd side of the rendezvous. Wraps a net.Pipe end
// with a Resize recorder.
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

// fakeAttachBridger satisfies AttachBridger.
type fakeAttachBridger struct {
	mu       sync.Mutex
	calls    int
	lastReq  sandboxgateway.AttachRequest
	sessions []sandboxgateway.AttachSession
	err      error
}

// waitForDataFrame polls the fake stream's outbound buffer until
// a Data frame matching want appears, or the deadline expires.
// Returns true on success.
func waitForDataFrame(s *fakeAttachStream, want []byte, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	//daft:lint:disable=forloopbound reason="bounded by wall-clock deadline; exits on first match"
	for time.Now().Before(deadline) {
		for _, m := range s.sent() {
			if bytes.Equal(m.GetData(), want) {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func (b *fakeAttachBridger) Attach(_ context.Context, req sandboxgateway.AttachRequest) (sandboxgateway.AttachSession, error) {
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

// TestRunAttachSendsHelloAndDispatches pins the canonical happy
// path: Hello sent with the right session_id; AttachBridger
// invoked with the announced request.
func TestRunAttachSendsHelloAndDispatches(t *testing.T) {
	sessLocal, sessRemote := net.Pipe()
	t.Cleanup(func() { _ = sessLocal.Close() })
	session := &fakeAttachSession{Conn: sessRemote}
	br := &fakeAttachBridger{sessions: []sandboxgateway.AttachSession{session}}
	s := newFakeAttachStream()
	req := &nodectrlv1.AttachRequest{
		SessionId:  testAttachSession,
		SandboxUid: testAttachSandbox,
		Command:    []string{"/bin/sh"},
		Tty:        true,
		Cols:       80, Rows: 24,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- runAttach(ctx, req, s, br) }()

	// Close the session and stream so the pump exits.
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = sessLocal.Close()
		_ = s.CloseResponse()
	}()

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("runAttach: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runAttach did not return within 2s")
	}

	sent := s.sent()
	if len(sent) < 1 {
		t.Fatalf("sent %d frames; want at least 1 (Hello)", len(sent))
	}
	hello := sent[0].GetHello()
	if hello == nil {
		t.Fatalf("first frame is not Hello: %+v", sent[0])
	}
	if hello.GetSessionId() != testAttachSession {
		t.Errorf("Hello.session_id = %q, want %q", hello.GetSessionId(), testAttachSession)
	}
	if br.calls != 1 || br.lastReq.SandboxUID != testAttachSandbox || !br.lastReq.TTY {
		t.Errorf("bridger calls=%d req=%+v, want one tty=true call to %s",
			br.calls, br.lastReq, testAttachSandbox)
	}
}

// TestRunAttachHelloSendError pins that a transport-level Send
// failure on Hello surfaces verbatim.
func TestRunAttachHelloSendError(t *testing.T) {
	want := errors.New("synthetic send fail")
	s := newFakeAttachStream()
	s.sendErr = want
	br := &fakeAttachBridger{}
	err := runAttach(context.Background(),
		&nodectrlv1.AttachRequest{SessionId: testAttachSession}, s, br)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
	if br.calls != 0 {
		t.Errorf("bridger called %d times; want 0 after Hello error", br.calls)
	}
}

// TestRunAttachBridgerError pins bridger.Attach error propagation.
func TestRunAttachBridgerError(t *testing.T) {
	want := errors.New("synthetic attach fail")
	s := newFakeAttachStream()
	br := &fakeAttachBridger{err: want}
	err := runAttach(context.Background(),
		&nodectrlv1.AttachRequest{SessionId: testAttachSession, SandboxUid: testAttachSandbox}, s, br)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestPumpAttachStreamRoundTrip pins the bidi pump: data flowing
// each way + a resize frame land where expected.
func TestPumpAttachStreamRoundTrip(t *testing.T) {
	sessLocal, sessRemote := net.Pipe()
	session := &fakeAttachSession{Conn: sessRemote}
	s := newFakeAttachStream()

	pumpDone := make(chan error, 1)
	go func() { pumpDone <- pumpAttachStream(s, session) }()

	// stream → session: data frame
	s.push(&nodectrlv1.AttachStreamMessage{
		Body: &nodectrlv1.AttachStreamMessage_Data{Data: []byte("input")},
	})
	got := make([]byte, 5)
	if _, err := io.ReadFull(sessLocal, got); err != nil {
		t.Fatalf("sessLocal.Read: %v", err)
	}
	if string(got) != "input" {
		t.Errorf("session received %q, want input", got)
	}

	// stream → session: resize frame
	s.push(&nodectrlv1.AttachStreamMessage{
		Body: &nodectrlv1.AttachStreamMessage_Resize{
			Resize: &nodectrlv1.AttachStreamResize{Cols: 100, Rows: 30},
		},
	})
	deadline := time.Now().Add(2 * time.Second)
	//daft:lint:disable=forloopbound reason="bounded 2s; exits when resize observed"
	for time.Now().Before(deadline) {
		if len(session.sentResizes()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rs := session.sentResizes(); len(rs) != 1 || rs[0].cols != 100 || rs[0].rows != 30 {
		t.Errorf("resizes = %+v, want one cols=100 rows=30", rs)
	}

	// session → stream: bytes wrap as Data frames
	go func() { _, _ = sessLocal.Write([]byte("output")) }()
	if !waitForDataFrame(s, []byte("output"), 2*time.Second) {
		t.Fatal("output frame not observed within 2s")
	}

	_ = sessLocal.Close()
	_ = s.CloseResponse()
	select {
	case err := <-pumpDone:
		if err != nil {
			t.Errorf("pumpAttachStream: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pumpAttachStream did not return within 2s")
	}
}

// TestPumpAttachStreamSkipsHelloAndEmptyData pins that subsequent
// Hello frames and zero-byte Data frames are tolerated rather
// than triggering errors.
func TestPumpAttachStreamSkipsHelloAndEmptyData(t *testing.T) {
	sessLocal, sessRemote := net.Pipe()
	session := &fakeAttachSession{Conn: sessRemote}
	s := newFakeAttachStream()

	pumpDone := make(chan error, 1)
	go func() { pumpDone <- pumpAttachStream(s, session) }()

	s.push(&nodectrlv1.AttachStreamMessage{
		Body: &nodectrlv1.AttachStreamMessage_Hello{
			Hello: &nodectrlv1.AttachStreamHello{SessionId: "extra"},
		},
	})
	s.push(&nodectrlv1.AttachStreamMessage{
		Body: &nodectrlv1.AttachStreamMessage_Data{Data: nil},
	})
	s.push(&nodectrlv1.AttachStreamMessage{
		Body: &nodectrlv1.AttachStreamMessage_Data{Data: []byte("real")},
	})
	buf := make([]byte, 4)
	if _, err := io.ReadFull(sessLocal, buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf) != "real" {
		t.Errorf("got %q, want real", buf)
	}

	_ = sessLocal.Close()
	_ = s.CloseResponse()
	<-pumpDone
}

// stubAttachSession is a minimal AttachSession for pump
// error-path tests on the sandboxd side. Write / Resize fail
// injectably; Read blocks on a closeable channel.
type stubAttachSession struct {
	mu     sync.Mutex
	wErr   error
	rzErr  error
	readCh chan []byte
	closed bool
}

func newStubAttachSession() *stubAttachSession {
	return &stubAttachSession{readCh: make(chan []byte, 4)}
}

func (s *stubAttachSession) Read(p []byte) (int, error) {
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

func (s *stubAttachSession) LocalAddr() net.Addr                { return nil }
func (s *stubAttachSession) RemoteAddr() net.Addr               { return nil }
func (s *stubAttachSession) SetDeadline(_ time.Time) error      { return nil }
func (s *stubAttachSession) SetReadDeadline(_ time.Time) error  { return nil }
func (s *stubAttachSession) SetWriteDeadline(_ time.Time) error { return nil }

// TestPumpAttachStreamToSessionWriteError pins that a non-clean
// session.Write error propagates out of the stream→session pump.
func TestPumpAttachStreamToSessionWriteError(t *testing.T) {
	want := errors.New("synthetic write fail")
	stream := newFakeAttachStream()
	sess := newStubAttachSession()
	sess.wErr = want
	stream.push(&nodectrlv1.AttachStreamMessage{
		Body: &nodectrlv1.AttachStreamMessage_Data{Data: []byte("payload")},
	})
	err := pumpAttachStreamToSession(stream, sess)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestPumpAttachStreamToSessionResizeError pins the resize-error
// propagation branch.
func TestPumpAttachStreamToSessionResizeError(t *testing.T) {
	want := errors.New("synthetic resize fail")
	stream := newFakeAttachStream()
	sess := newStubAttachSession()
	sess.rzErr = want
	stream.push(&nodectrlv1.AttachStreamMessage{
		Body: &nodectrlv1.AttachStreamMessage_Resize{
			Resize: &nodectrlv1.AttachStreamResize{Cols: 80, Rows: 24},
		},
	})
	err := pumpAttachStreamToSession(stream, sess)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestPumpAttachSessionToStreamSendError pins that a non-clean
// stream.Send error propagates out of the session→stream pump.
func TestPumpAttachSessionToStreamSendError(t *testing.T) {
	want := errors.New("synthetic send fail")
	sessLocal, sessRemote := net.Pipe()
	t.Cleanup(func() { _ = sessLocal.Close() })
	sess := &fakeAttachSession{Conn: sessRemote}
	stream := &errorSendingNodeAttachStream{err: want}
	go func() { _, _ = sessLocal.Write([]byte("data")) }()
	err := pumpAttachSessionToStream(stream, sess)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// errorSendingNodeAttachStream is an attachStreamRPC whose Send
// always fails; Receive blocks until Close.
type errorSendingNodeAttachStream struct {
	err error
}

func (s *errorSendingNodeAttachStream) Send(*nodectrlv1.AttachStreamMessage) error { return s.err }
func (s *errorSendingNodeAttachStream) Receive() (*nodectrlv1.AttachStreamMessage, error) {
	return nil, io.EOF
}
func (s *errorSendingNodeAttachStream) CloseRequest() error  { return nil }
func (s *errorSendingNodeAttachStream) CloseResponse() error { return nil }

// TestHandleAttachRejectsMissingBridger pins the misconfiguration
// branch.
func TestHandleAttachRejectsMissingBridger(t *testing.T) {
	c := &Client{cfg: Config{}}
	err := c.handleAttach(context.Background(),
		&nodectrlv1.AttachRequest{SessionId: testAttachSession})
	if err == nil {
		t.Fatal("expected error when AttachBridger is nil")
	}
	if !contains(err.Error(), "AttachBridger") {
		t.Errorf("err %q should mention AttachBridger", err)
	}
}

// TestDispatchEventRoutesAttach pins that dispatchEvent demuxes
// AttachRequest events into handleAttach. The Bridger-nil branch
// is exercised so the error gets logged via the configured Stderr.
//
//nolint:dupl // structurally parallel to TestDispatchEventRoutesOpenPort; merging would obscure which event variant is being routed. Side-by-side review is the intent.
func TestDispatchEventRoutesAttach(t *testing.T) {
	called := make(chan struct{}, 1)
	c := &Client{cfg: Config{
		Stderr: writerFunc(func(_ []byte) (int, error) {
			select {
			case called <- struct{}{}:
			default:
			}
			return 0, nil
		}),
	}}
	msg := &nodectrlv1.NodeEvent{
		Event: &nodectrlv1.NodeEvent_Attach{
			Attach: &nodectrlv1.AttachRequest{SessionId: testAttachSession},
		},
	}
	if err := c.dispatchEvent(context.Background(), msg); err != nil {
		t.Fatalf("dispatchEvent: %v", err)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("handleAttach error was not logged within 2s")
	}
}
