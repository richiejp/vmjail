package nodectrlclient

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
)

const (
	testSession   = "sess-aaaa"
	testWorkspace = "ws-bbbb"
	testPort      = uint32(8080)
)

// fakePortStream is an in-process portStreamRPC: tests push frames
// into `incoming` and observe what the code-under-test sent via
// `outgoing`. CloseRequest / CloseResponse flip the matching
// boolean so tests can assert close ordering.
type fakePortStream struct {
	mu       sync.Mutex
	incoming chan *nodectrlv1.PortStreamMessage
	outgoing []*nodectrlv1.PortStreamMessage
	closeReq bool
	closeRsp bool
	recvErr  error
	sendErr  error
}

func newFakePortStream() *fakePortStream {
	return &fakePortStream{incoming: make(chan *nodectrlv1.PortStreamMessage, 4)}
}

func (s *fakePortStream) Send(m *nodectrlv1.PortStreamMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.outgoing = append(s.outgoing, m)
	return nil
}

func (s *fakePortStream) Receive() (*nodectrlv1.PortStreamMessage, error) {
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

func (s *fakePortStream) CloseRequest() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeReq = true
	return nil
}

func (s *fakePortStream) CloseResponse() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeRsp = true
	close(s.incoming)
	return nil
}

func (s *fakePortStream) push(m *nodectrlv1.PortStreamMessage) { s.incoming <- m }
func (s *fakePortStream) sent() []*nodectrlv1.PortStreamMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*nodectrlv1.PortStreamMessage, len(s.outgoing))
	copy(out, s.outgoing)
	return out
}

type fakeBridger struct {
	mu      sync.Mutex
	calls   int
	wsSeen  string
	prtSeen uint32
	doErr   error
	// onConn lets the test inject behaviour against the conn the
	// runOpenPort hands us (e.g. write a byte, read a byte) before
	// returning.
	onConn func(net.Conn) error
}

func (b *fakeBridger) Bridge(_ context.Context, ws string, port uint32, conn net.Conn) error {
	b.mu.Lock()
	b.calls++
	b.wsSeen = ws
	b.prtSeen = port
	onConn := b.onConn
	doErr := b.doErr
	b.mu.Unlock()
	if onConn != nil {
		if err := onConn(conn); err != nil {
			return err
		}
	}
	return doErr
}

// TestRunOpenPortSendsHelloAndDispatchesBridge pins the canonical
// happy path: Hello frame is sent with the request's session_id,
// then Bridger.Bridge fires with the announced workspace and port.
func TestRunOpenPortSendsHelloAndDispatchesBridge(t *testing.T) {
	s := newFakePortStream()
	b := &fakeBridger{}
	req := &nodectrlv1.OpenPortRequest{
		SessionId:     testSession,
		WorkspaceUuid: testWorkspace,
		ContainerPort: testPort,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runOpenPort(ctx, req, s, b); err != nil {
		t.Fatalf("runOpenPort: %v", err)
	}
	sent := s.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1: %+v", len(sent), sent)
	}
	hello := sent[0].GetHello()
	if hello == nil {
		t.Fatalf("first message is not Hello: %+v", sent[0])
	}
	if hello.GetSessionId() != testSession {
		t.Errorf("Hello.session_id = %q, want %q", hello.GetSessionId(), testSession)
	}
	if b.calls != 1 || b.wsSeen != testWorkspace || b.prtSeen != testPort {
		t.Errorf("bridger calls=%d ws=%q port=%d, want 1 %q %d",
			b.calls, b.wsSeen, b.prtSeen, testWorkspace, testPort)
	}
	if !s.closeRsp {
		t.Error("CloseResponse not invoked")
	}
}

// TestRunOpenPortHelloSendError pins that a transport-level Send
// failure on the Hello frame surfaces verbatim — without it the
// orchestrator's pending session would silently hang.
func TestRunOpenPortHelloSendError(t *testing.T) {
	want := errors.New("synthetic send fail")
	s := newFakePortStream()
	s.sendErr = want
	b := &fakeBridger{}
	req := &nodectrlv1.OpenPortRequest{SessionId: testSession}
	err := runOpenPort(context.Background(), req, s, b)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
	if b.calls != 0 {
		t.Errorf("bridger called %d times; expected 0 after Hello error", b.calls)
	}
}

// TestRunOpenPortBridgeError pins that bridger errors propagate
// wrapped with the (workspace, port) labels for log triage.
func TestRunOpenPortBridgeError(t *testing.T) {
	want := errors.New("synthetic bridge fail")
	s := newFakePortStream()
	b := &fakeBridger{doErr: want}
	req := &nodectrlv1.OpenPortRequest{
		SessionId:     testSession,
		WorkspaceUuid: testWorkspace,
		ContainerPort: testPort,
	}
	err := runOpenPort(context.Background(), req, s, b)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestPortStreamConnReadWrite covers the round-trip through the
// portStreamConn adapter: Write encodes a Data frame, Read decodes
// inbound Data frames and serves the bytes through net.Conn.Read.
func TestPortStreamConnReadWrite(t *testing.T) {
	s := newFakePortStream()
	conn := newPortStreamConn(s)
	// Write encodes one Data frame.
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sent := s.sent()
	if len(sent) != 1 || string(sent[0].GetData()) != "hello" {
		t.Fatalf("sent = %+v, want one Data frame 'hello'", sent)
	}
	// Read decodes one inbound Data frame.
	s.push(&nodectrlv1.PortStreamMessage{
		Body: &nodectrlv1.PortStreamMessage_Data{Data: []byte("world")},
	})
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf) != "world" {
		t.Errorf("Read = %q, want %q", buf, "world")
	}
}

// TestPortStreamConnReadResidualBuffer pins that a frame larger
// than the caller's buffer is served across multiple Reads without
// dropping bytes.
func TestPortStreamConnReadResidualBuffer(t *testing.T) {
	s := newFakePortStream()
	conn := newPortStreamConn(s)
	s.push(&nodectrlv1.PortStreamMessage{
		Body: &nodectrlv1.PortStreamMessage_Data{Data: []byte("abcdef")},
	})
	first := make([]byte, 3)
	if n, err := conn.Read(first); err != nil || n != 3 {
		t.Fatalf("first Read = %d, %v; want 3, nil", n, err)
	}
	second := make([]byte, 3)
	if n, err := conn.Read(second); err != nil || n != 3 {
		t.Fatalf("second Read = %d, %v; want 3, nil", n, err)
	}
	if got := string(first) + string(second); got != "abcdef" {
		t.Errorf("reassembled = %q, want abcdef", got)
	}
}

// TestPortStreamConnReadEOF pins the end-of-stream surface: the
// fake stream's Receive returns io.EOF after CloseResponse, the
// conn surfaces that to the caller.
func TestPortStreamConnReadEOF(t *testing.T) {
	s := newFakePortStream()
	conn := newPortStreamConn(s)
	_ = s.CloseResponse() // closes incoming chan
	buf := make([]byte, 1)
	_, err := conn.Read(buf)
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

// TestPortStreamConnWriteAfterCloseErrsClosed pins that a Write
// after Close returns net.ErrClosed rather than racing the
// underlying stream's own error.
func TestPortStreamConnWriteAfterCloseErrsClosed(t *testing.T) {
	s := newFakePortStream()
	conn := newPortStreamConn(s)
	if err := conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := conn.Write([]byte("x"))
	if !errors.Is(err, net.ErrClosed) {
		t.Errorf("err = %v, want net.ErrClosed", err)
	}
}

// TestPortStreamConnSkipsNonDataFrames pins the spurious-Hello
// branch: an extra Hello after the handshake produces no payload,
// the conn loops to the next Receive.
func TestPortStreamConnSkipsNonDataFrames(t *testing.T) {
	s := newFakePortStream()
	conn := newPortStreamConn(s)
	s.push(&nodectrlv1.PortStreamMessage{
		Body: &nodectrlv1.PortStreamMessage_Hello{
			Hello: &nodectrlv1.PortStreamHello{SessionId: "extra"},
		},
	})
	s.push(&nodectrlv1.PortStreamMessage{
		Body: &nodectrlv1.PortStreamMessage_Data{Data: []byte("ok")},
	})
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf) != "ok" {
		t.Errorf("Read = %q, want ok", buf)
	}
}

// TestPortStreamConnAddrShape pins the cosmetic Addr surface used
// in trace lines and error messages so a future refactor of the
// adapter doesn't silently break log grep-ability.
func TestPortStreamConnAddrShape(t *testing.T) {
	conn := newPortStreamConn(newFakePortStream())
	if conn.LocalAddr().Network() != "daft-port-stream" {
		t.Errorf("Network = %q", conn.LocalAddr().Network())
	}
	if conn.LocalAddr().String() != "daft-port-stream:local" {
		t.Errorf("LocalAddr = %q", conn.LocalAddr().String())
	}
	if conn.RemoteAddr().String() != "daft-port-stream:remote" {
		t.Errorf("RemoteAddr = %q", conn.RemoteAddr().String())
	}
	if err := conn.SetDeadline(time.Now()); err != nil {
		t.Errorf("SetDeadline: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now()); err != nil {
		t.Errorf("SetReadDeadline: %v", err)
	}
	if err := conn.SetWriteDeadline(time.Now()); err != nil {
		t.Errorf("SetWriteDeadline: %v", err)
	}
}

// TestHandleOpenPortRejectsMissingBridger pins the misconfiguration
// branch: an OpenPortRequest arriving when no Bridger is wired
// surfaces a clear "no Bridger" error rather than dispatching the
// stream into the void.
func TestHandleOpenPortRejectsMissingBridger(t *testing.T) {
	c := &Client{cfg: Config{}}
	err := c.handleOpenPort(context.Background(),
		&nodectrlv1.OpenPortRequest{SessionId: testSession})
	if err == nil {
		t.Fatal("expected error when Bridger is nil")
	}
	if !contains(err.Error(), "Bridger") {
		t.Errorf("err %q should mention Bridger", err)
	}
}

// TestDispatchEventRoutesOpenPort pins that dispatchEvent finds
// the open-port branch and triggers handleOpenPort. The handler is
// invoked in a goroutine for liveness — we wait on the bridger's
// observation rather than the dispatchEvent return.
//
//nolint:dupl // structurally parallel to TestDispatchEventRoutesAttach in attach_test.go; the two event variants are different and side-by-side review is the intent.
func TestDispatchEventRoutesOpenPort(t *testing.T) {
	// dispatchEvent dials c.ctrl.OpenPortStream, which we can't
	// hit without a real Connect server. Instead, exercise the
	// Bridger-nil branch through dispatchEvent so we still cover
	// the "OpenPort variant detected and handed off" path.
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
		Event: &nodectrlv1.NodeEvent_OpenPort{
			OpenPort: &nodectrlv1.OpenPortRequest{SessionId: testSession},
		},
	}
	if err := c.dispatchEvent(context.Background(), msg); err != nil {
		t.Fatalf("dispatchEvent: %v", err)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("handleOpenPort error was not logged within 2s")
	}
}

// TestPortStreamConnCloseIsIdempotent pins the second-Close
// no-op: the closeOnce CompareAndSwap path covers the case where
// Bridge returns and the deferred Close runs after the user has
// already closed.
func TestPortStreamConnCloseIsIdempotent(t *testing.T) {
	conn := newPortStreamConn(newFakePortStream())
	if err := conn.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Errorf("second Close: %v, want nil", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// writerFunc is an io.Writer adapter for tests that just want to
// fire a side-effect on Write.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
