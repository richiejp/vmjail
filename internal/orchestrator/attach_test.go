package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
)

const (
	testAttachSandboxUID = "sb-attach-1"
	// testAttachShell is the default exec the unit tests assert
	// the bridger sees on attach.
	testAttachShell = "/bin/sh"
	// testBadHelloDataFirst is the table-test sub-name shared
	// between the attach + forward "first frame is Data" rejection
	// tests; defined here so goconst doesn't flag the 3rd
	// occurrence as a new literal.
	testBadHelloDataFirst = "data first"
)

// fakeAttachServerStream is the in-process serverAttachStream
// for attach unit tests — same shape as fakeServerStream but
// typed for AttachStreamMessage.
type fakeAttachServerStream struct {
	mu       sync.Mutex
	incoming chan *nodectrlv1.AttachStreamMessage
	outgoing []*nodectrlv1.AttachStreamMessage
	sendErr  error
	recvErr  error
}

func newFakeAttachServerStream() *fakeAttachServerStream {
	return &fakeAttachServerStream{incoming: make(chan *nodectrlv1.AttachStreamMessage, 4)}
}

func (s *fakeAttachServerStream) Send(m *nodectrlv1.AttachStreamMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.outgoing = append(s.outgoing, m)
	return nil
}

func (s *fakeAttachServerStream) Receive() (*nodectrlv1.AttachStreamMessage, error) {
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

func (s *fakeAttachServerStream) push(m *nodectrlv1.AttachStreamMessage) { s.incoming <- m }
func (s *fakeAttachServerStream) sent() []*nodectrlv1.AttachStreamMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*nodectrlv1.AttachStreamMessage, len(s.outgoing))
	copy(out, s.outgoing)
	return out
}

// newAttachTestHandler stands up a NodeControlHandler with one
// fingerprint pre-attached, returning the per-fingerprint event
// channel so tests can observe pushed AttachRequest events.
func newAttachTestHandler(t *testing.T) (*NodeControlHandler, identity.Fingerprint, chan *nodectrlv1.NodeEvent) {
	t.Helper()
	reg := nodectrl.NewRegistry(time.Now)
	h, err := NewNodeControlHandler(reg)
	if err != nil {
		t.Fatalf("NewNodeControlHandler: %v", err)
	}
	fp := identity.Fingerprint{0xdd, 0xee}
	events := h.attachStream(fp)
	t.Cleanup(func() { h.detachStream(fp) })
	return h, fp, events
}

type attachBridgeResult struct {
	session sandboxgateway.AttachSession
	err     error
}

// startAttachBridge kicks off OpenAttachBridge in a goroutine and
// returns a channel the test reads from once the bridge resolves.
func startAttachBridge(t *testing.T, h *NodeControlHandler, fp identity.Fingerprint) <-chan attachBridgeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	out := make(chan attachBridgeResult, 1)
	go func() {
		s, err := h.OpenAttachBridge(ctx, fp, sandboxgateway.AttachRequest{
			SandboxUID: testAttachSandboxUID,
			Command:    []string{testAttachShell, "-l"},
			TTY:        true,
			Cols:       80, Rows: 24,
		})
		out <- attachBridgeResult{s, err}
	}()
	return out
}

// awaitAttachEvent pulls the AttachRequest event and asserts its
// shape, returning the session_id.
func awaitAttachEvent(t *testing.T, events <-chan *nodectrlv1.NodeEvent) string {
	t.Helper()
	select {
	case ev := <-events:
		req := ev.GetAttach()
		if req == nil {
			t.Fatalf("event is not Attach: %+v", ev)
		}
		if req.GetSandboxUid() != testAttachSandboxUID {
			t.Errorf("sandbox_uid = %q, want %q", req.GetSandboxUid(), testAttachSandboxUID)
		}
		if !req.GetTty() {
			t.Errorf("tty = false, want true")
		}
		if req.GetCols() != 80 || req.GetRows() != 24 {
			t.Errorf("cols x rows = %dx%d, want 80x24", req.GetCols(), req.GetRows())
		}
		if got := req.GetCommand(); len(got) != 2 || got[0] != testAttachShell || got[1] != "-l" {
			t.Errorf("command = %v, want [/bin/sh -l]", got)
		}
		if req.GetSessionId() == "" {
			t.Errorf("session_id is empty")
		}
		return req.GetSessionId()
	case <-time.After(time.Second):
		t.Fatal("attach event not pushed within 1s")
	}
	return ""
}

// TestOpenAttachBridgePushesEventThenSplices pins the canonical
// happy path: OpenAttachBridge mints a session, pushes a
// well-shaped AttachRequest, and once runAttachStream hands the
// inbound stream back, the returned AttachSession round-trips
// bytes + a resize frame.
func TestOpenAttachBridgePushesEventThenSplices(t *testing.T) {
	h, fp, events := newAttachTestHandler(t)
	bridgeDone := startAttachBridge(t, h, fp)
	sessionID := awaitAttachEvent(t, events)

	stream := newFakeAttachServerStream()
	stream.push(attachHelloMsg(sessionID))
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	streamDone := make(chan error, 1)
	go func() { streamDone <- h.runAttachStream(streamCtx, stream) }()

	res := <-bridgeDone
	if res.err != nil {
		t.Fatalf("OpenAttachBridge: %v", res.err)
	}
	session := res.session
	defer func() { _ = session.Close() }()

	if _, err := session.Write([]byte("from-orch")); err != nil {
		t.Fatalf("session.Write: %v", err)
	}
	if err := session.Resize(120, 40); err != nil {
		t.Fatalf("session.Resize: %v", err)
	}
	sent := stream.sent()
	if len(sent) != 2 {
		t.Fatalf("stream sent %d frames, want 2 (data+resize)", len(sent))
	}
	if got := sent[0].GetData(); string(got) != "from-orch" {
		t.Errorf("frame 0 data = %q, want from-orch", got)
	}
	if r := sent[1].GetResize(); r == nil || r.GetCols() != 120 || r.GetRows() != 40 {
		t.Errorf("frame 1 resize = %+v, want cols=120 rows=40", r)
	}

	stream.push(&nodectrlv1.AttachStreamMessage{
		Body: &nodectrlv1.AttachStreamMessage_Data{Data: []byte("from-vm")},
	})
	got := make([]byte, len("from-vm"))
	if _, err := io.ReadFull(session, got); err != nil {
		t.Fatalf("session.Read: %v", err)
	}
	if !bytes.Equal(got, []byte("from-vm")) {
		t.Errorf("read = %q, want from-vm", got)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("session.Close: %v", err)
	}
	select {
	case err := <-streamDone:
		if err != nil {
			t.Errorf("runAttachStream: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runAttachStream did not return after Close")
	}
}

func TestOpenAttachBridgeReturnsNodeRejection(t *testing.T) {
	h, fp, events := newAttachTestHandler(t)
	bridgeDone := startAttachBridge(t, h, fp)
	sessionID := awaitAttachEvent(t, events)
	stream := newFakeAttachServerStream()
	stream.push(&nodectrlv1.AttachStreamMessage{Body: &nodectrlv1.AttachStreamMessage_Hello{
		Hello: &nodectrlv1.AttachStreamHello{SessionId: sessionID, Error: "guest CONNECT timed out"},
	}})
	if err := h.runAttachStream(t.Context(), stream); err != nil {
		t.Fatalf("runAttachStream: %v", err)
	}
	res := <-bridgeDone
	if res.err == nil || !strings.Contains(res.err.Error(), "guest CONNECT timed out") {
		t.Fatalf("OpenAttachBridge err = %v, want Node rejection", res.err)
	}
}

func TestServerAttachSessionPreservesExit(t *testing.T) {
	stream := newFakeAttachServerStream()
	stream.push(&nodectrlv1.AttachStreamMessage{Body: &nodectrlv1.AttachStreamMessage_Exit{
		Exit: &nodectrlv1.AttachStreamExit{Code: 17, Error: "exec failed"},
	}})
	session := newServerAttachSession(stream, make(chan struct{}))
	_, err := session.Read(make([]byte, 1))
	var exit *sandboxgateway.AttachExitError
	if !errors.As(err, &exit) || exit.Code != 17 || exit.Reason != "exec failed" {
		t.Fatalf("Read error = %#v, want preserved exit", err)
	}
}

func attachHelloMsg(sessionID string) *nodectrlv1.AttachStreamMessage {
	return &nodectrlv1.AttachStreamMessage{
		Body: &nodectrlv1.AttachStreamMessage_Hello{
			Hello: &nodectrlv1.AttachStreamHello{SessionId: sessionID},
		},
	}
}

// TestOpenAttachBridgeRejectsEmptySandbox covers the input
// validation branch.
func TestOpenAttachBridgeRejectsEmptySandbox(t *testing.T) {
	h, fp, _ := newAttachTestHandler(t)
	_, err := h.OpenAttachBridge(context.Background(), fp, sandboxgateway.AttachRequest{})
	if err == nil {
		t.Error("expected error for empty SandboxUID")
	}
}

// TestOpenAttachBridgeNodeNotConnected pins the no-stream branch.
func TestOpenAttachBridgeNodeNotConnected(t *testing.T) {
	reg := nodectrl.NewRegistry(time.Now)
	h, err := NewNodeControlHandler(reg)
	if err != nil {
		t.Fatalf("NewNodeControlHandler: %v", err)
	}
	fp := identity.Fingerprint{0xff}
	_, err = h.OpenAttachBridge(context.Background(), fp, sandboxgateway.AttachRequest{SandboxUID: "sb"})
	if !errors.Is(err, ErrNodeNotConnected) {
		t.Errorf("err = %v, want ErrNodeNotConnected", err)
	}
}

// TestOpenAttachBridgeTimesOut pins the no-stream-arrival branch.
func TestOpenAttachBridgeTimesOut(t *testing.T) {
	h, fp, events := newAttachTestHandler(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := h.OpenAttachBridge(ctx, fp, sandboxgateway.AttachRequest{SandboxUID: "sb"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Error("event not pushed before timeout")
	}
	h.mu.Lock()
	pending := len(h.pendingAttaches)
	h.mu.Unlock()
	if pending != 0 {
		t.Errorf("pendingAttaches has %d entries, want 0", pending)
	}
}

// TestOpenAttachBridgeBacklogSurface pins the saturated-channel
// branch.
func TestOpenAttachBridgeBacklogSurface(t *testing.T) {
	h, fp, events := newAttachTestHandler(t)
	for range cap(events) {
		events <- &nodectrlv1.NodeEvent{}
	}
	_, err := h.OpenAttachBridge(context.Background(), fp, sandboxgateway.AttachRequest{SandboxUID: "sb"})
	if !errors.Is(err, ErrSandboxEventBacklog) {
		t.Errorf("err = %v, want ErrSandboxEventBacklog", err)
	}
}

// TestRunAttachStreamRejectsBadHello pins the protocol-violation
// branch — non-Hello or empty session_id.
func TestRunAttachStreamRejectsBadHello(t *testing.T) {
	h, _, _ := newAttachTestHandler(t)
	for _, tc := range []struct {
		name string
		msg  *nodectrlv1.AttachStreamMessage
	}{
		{testBadHelloDataFirst, &nodectrlv1.AttachStreamMessage{
			Body: &nodectrlv1.AttachStreamMessage_Data{Data: []byte("nope")},
		}},
		{"empty session", &nodectrlv1.AttachStreamMessage{
			Body: &nodectrlv1.AttachStreamMessage_Hello{
				Hello: &nodectrlv1.AttachStreamHello{SessionId: ""},
			},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := newFakeAttachServerStream()
			stream.push(tc.msg)
			err := h.runAttachStream(context.Background(), stream)
			if !errors.Is(err, ErrAttachHelloMissingSession) {
				t.Errorf("err = %v, want ErrAttachHelloMissingSession", err)
			}
		})
	}
}

// TestRunAttachStreamRejectsUnknownSession pins the stale-stream
// branch.
func TestRunAttachStreamRejectsUnknownSession(t *testing.T) {
	h, _, _ := newAttachTestHandler(t)
	stream := newFakeAttachServerStream()
	stream.push(attachHelloMsg("ghost"))
	err := h.runAttachStream(context.Background(), stream)
	if !errors.Is(err, ErrUnknownAttachSession) {
		t.Errorf("err = %v, want ErrUnknownAttachSession", err)
	}
}

// TestRunAttachStreamFirstReceiveError pins the transport-error
// branch.
func TestRunAttachStreamFirstReceiveError(t *testing.T) {
	h, _, _ := newAttachTestHandler(t)
	want := errors.New("synthetic recv fail")
	stream := newFakeAttachServerStream()
	stream.recvErr = want
	err := h.runAttachStream(context.Background(), stream)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestServerAttachSessionCloseIdempotent + addr shape + write
// after close.
func TestServerAttachSessionCloseIdempotent(t *testing.T) {
	s := newServerAttachSession(newFakeAttachServerStream(), make(chan struct{}))
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestServerAttachSessionWriteAfterCloseErrsClosed(t *testing.T) {
	s := newServerAttachSession(newFakeAttachServerStream(), make(chan struct{}))
	_ = s.Close()
	if _, err := s.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Write err = %v, want net.ErrClosed", err)
	}
	if err := s.Resize(80, 24); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Resize err = %v, want net.ErrClosed", err)
	}
}

func TestServerAttachSessionAddrShape(t *testing.T) {
	s := newServerAttachSession(newFakeAttachServerStream(), make(chan struct{}))
	if s.LocalAddr().Network() != "daft-attach-stream" {
		t.Errorf("Network = %q", s.LocalAddr().Network())
	}
	if s.LocalAddr().String() != "daft-attach-stream:local" {
		t.Errorf("LocalAddr = %q", s.LocalAddr())
	}
	if s.RemoteAddr().String() != "daft-attach-stream:remote" {
		t.Errorf("RemoteAddr = %q", s.RemoteAddr())
	}
	now := time.Now()
	if err := s.SetDeadline(now); err != nil {
		t.Errorf("SetDeadline: %v", err)
	}
	if err := s.SetReadDeadline(now); err != nil {
		t.Errorf("SetReadDeadline: %v", err)
	}
	if err := s.SetWriteDeadline(now); err != nil {
		t.Errorf("SetWriteDeadline: %v", err)
	}
}

// TestServerAttachSessionReadDropsHelloAndEmptyData pins that the
// adapter's Read loop tolerates spurious Hello frames and
// zero-byte Data frames the orchestrator might see if a misbehaving
// sandboxd echoes one — Read keeps polling until real data arrives.
func TestServerAttachSessionReadDropsHelloAndEmptyData(t *testing.T) {
	stream := newFakeAttachServerStream()
	s := newServerAttachSession(stream, make(chan struct{}))
	stream.push(attachHelloMsg("ignored"))
	stream.push(&nodectrlv1.AttachStreamMessage{
		Body: &nodectrlv1.AttachStreamMessage_Data{Data: nil},
	})
	stream.push(&nodectrlv1.AttachStreamMessage{
		Body: &nodectrlv1.AttachStreamMessage_Data{Data: []byte("real")},
	})
	got := make([]byte, 4)
	if _, err := io.ReadFull(s, got); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, []byte("real")) {
		t.Errorf("Read = %q, want real", got)
	}
}

// TestServerAttachSessionReadZeroLenIsNoop matches net.Conn's
// "Read(nil) returns 0, nil" idiom.
func TestServerAttachSessionReadZeroLenIsNoop(t *testing.T) {
	s := newServerAttachSession(newFakeAttachServerStream(), make(chan struct{}))
	n, err := s.Read(nil)
	if n != 0 || err != nil {
		t.Errorf("Read(nil) = %d, %v; want 0, nil", n, err)
	}
}

// TestServerAttachSessionReadAfterCloseReturnsEOF pins that a
// Read after Close surfaces io.EOF rather than parking on the
// stream.Receive.
func TestServerAttachSessionReadAfterCloseReturnsEOF(t *testing.T) {
	s := newServerAttachSession(newFakeAttachServerStream(), make(chan struct{}))
	_ = s.Close()
	buf := make([]byte, 1)
	_, err := s.Read(buf)
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

// TestServerAttachSessionReadReceiveError pins that a transport
// error on stream.Receive propagates to the caller.
func TestServerAttachSessionReadReceiveError(t *testing.T) {
	want := errors.New("synthetic recv fail")
	stream := newFakeAttachServerStream()
	stream.recvErr = want
	s := newServerAttachSession(stream, make(chan struct{}))
	buf := make([]byte, 1)
	_, err := s.Read(buf)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestServerAttachSessionWriteSendError pins that a non-clean
// send error on stream.Send propagates to Write.
func TestServerAttachSessionWriteSendError(t *testing.T) {
	want := errors.New("synthetic send fail")
	stream := newFakeAttachServerStream()
	stream.sendErr = want
	s := newServerAttachSession(stream, make(chan struct{}))
	_, err := s.Write([]byte("x"))
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestRunAttachStreamCtxCancelTearsDown pins the ctx-cancel
// branch of runAttachStream: after the rendezvous is handed off,
// cancelling the stream's ctx returns ctx.Err.
func TestRunAttachStreamCtxCancelTearsDown(t *testing.T) {
	h, fp, events := newAttachTestHandler(t)
	bridgeDone := startAttachBridge(t, h, fp)
	sessionID := awaitAttachEvent(t, events)

	stream := newFakeAttachServerStream()
	stream.push(attachHelloMsg(sessionID))
	streamCtx, streamCancel := context.WithCancel(context.Background())
	streamDone := make(chan error, 1)
	go func() { streamDone <- h.runAttachStream(streamCtx, stream) }()

	<-bridgeDone // wait for rendezvous
	streamCancel()
	select {
	case err := <-streamDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runAttachStream did not return after ctx cancel")
	}
}
