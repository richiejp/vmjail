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

	nodectrlv1 "github.com/localai-org/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/nodectrl"
)

const (
	testOpenPortWorkspace = "ws-aaaa"
	testOpenPortPort      = uint32(8080)
)

// fakeServerStream is an in-process serverPortStream for tests:
// inbound frames live on `incoming`, outbound on `outgoing`. Send
// records the frame; Receive blocks on incoming.
type fakeServerStream struct {
	mu       sync.Mutex
	incoming chan *nodectrlv1.PortStreamMessage
	outgoing []*nodectrlv1.PortStreamMessage
	sendErr  error
	recvErr  error
}

func newFakeServerStream() *fakeServerStream {
	return &fakeServerStream{incoming: make(chan *nodectrlv1.PortStreamMessage, 4)}
}

func (s *fakeServerStream) Send(m *nodectrlv1.PortStreamMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.outgoing = append(s.outgoing, m)
	return nil
}

func (s *fakeServerStream) Receive() (*nodectrlv1.PortStreamMessage, error) {
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

func (s *fakeServerStream) push(m *nodectrlv1.PortStreamMessage) { s.incoming <- m }
func (s *fakeServerStream) sent() []*nodectrlv1.PortStreamMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*nodectrlv1.PortStreamMessage, len(s.outgoing))
	copy(out, s.outgoing)
	return out
}

// newTestHandler builds a NodeControlHandler with a registered
// fingerprint and an attached event stream, returning both.
func newTestHandler(t *testing.T) (*NodeControlHandler, identity.Fingerprint, chan *nodectrlv1.NodeEvent) {
	t.Helper()
	reg := nodectrl.NewRegistry(time.Now)
	h, err := NewNodeControlHandler(reg)
	if err != nil {
		t.Fatalf("NewNodeControlHandler: %v", err)
	}
	fp := identity.Fingerprint{0xaa, 0xbb}
	events := h.attachStream(fp)
	t.Cleanup(func() { h.detachStream(fp) })
	return h, fp, events
}

type bridgeResult struct {
	conn net.Conn
	err  error
}

// startBridge kicks off OpenPortBridge in a goroutine and returns
// a channel the test reads from once the bridge resolves.
func startBridge(t *testing.T, h *NodeControlHandler, fp identity.Fingerprint) <-chan bridgeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	out := make(chan bridgeResult, 1)
	go func() {
		conn, err := h.OpenPortBridge(ctx, fp, testOpenPortWorkspace, testOpenPortPort)
		out <- bridgeResult{conn, err}
	}()
	return out
}

// awaitOpenPortEvent pulls the OpenPortRequest event the Node
// would have observed and verifies its shape. Returns the
// session_id for the test to echo back through Hello.
func awaitOpenPortEvent(t *testing.T, events <-chan *nodectrlv1.NodeEvent) string {
	t.Helper()
	select {
	case ev := <-events:
		req := ev.GetOpenPort()
		if req == nil {
			t.Fatalf("event is not OpenPort: %+v", ev)
		}
		if req.GetWorkspaceUuid() != testOpenPortWorkspace || req.GetContainerPort() != testOpenPortPort {
			t.Errorf("event = %+v, want workspace=%s port=%d", req, testOpenPortWorkspace, testOpenPortPort)
		}
		if req.GetSessionId() == "" {
			t.Errorf("event session_id is empty")
		}
		return req.GetSessionId()
	case <-time.After(time.Second):
		t.Fatal("event not pushed within 1s")
	}
	return ""
}

// TestOpenPortBridgePushesEventThenSplices pins the canonical
// happy path: OpenPortBridge mints a session and pushes a
// well-formed OpenPortRequest on the addressed Node's event
// stream; once runOpenPortStream consumes it and hands back the
// stream, OpenPortBridge returns a usable net.Conn for byte
// round-trip.
func TestOpenPortBridgePushesEventThenSplices(t *testing.T) {
	h, fp, events := newTestHandler(t)
	bridgeDone := startBridge(t, h, fp)
	sessionID := awaitOpenPortEvent(t, events)

	stream := newFakeServerStream()
	stream.push(helloMsg(sessionID))
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	streamDone := make(chan error, 1)
	go func() { streamDone <- h.runOpenPortStream(streamCtx, stream) }()

	res := <-bridgeDone
	if res.err != nil {
		t.Fatalf("OpenPortBridge: %v", res.err)
	}
	conn := res.conn
	defer func() { _ = conn.Close() }()

	roundTripBytes(t, conn, stream)

	if err := conn.Close(); err != nil {
		t.Fatalf("conn.Close: %v", err)
	}
	select {
	case err := <-streamDone:
		if err != nil {
			t.Errorf("runOpenPortStream: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runOpenPortStream did not return within 1s of Close")
	}
}

// helloMsg builds a Hello frame carrying the given session id.
func helloMsg(sessionID string) *nodectrlv1.PortStreamMessage {
	return &nodectrlv1.PortStreamMessage{
		Body: &nodectrlv1.PortStreamMessage_Hello{
			Hello: &nodectrlv1.PortStreamHello{SessionId: sessionID},
		},
	}
}

// roundTripBytes drives a write-then-read cycle through the
// bridged conn and asserts both directions land where expected.
func roundTripBytes(t *testing.T, conn net.Conn, stream *fakeServerStream) {
	t.Helper()
	if _, err := conn.Write([]byte("from-orch")); err != nil {
		t.Fatalf("conn.Write: %v", err)
	}
	sent := stream.sent()
	if len(sent) != 1 || string(sent[0].GetData()) != "from-orch" {
		t.Errorf("stream sent = %+v, want one Data frame 'from-orch'", sent)
	}
	stream.push(&nodectrlv1.PortStreamMessage{
		Body: &nodectrlv1.PortStreamMessage_Data{Data: []byte("from-node")},
	})
	got := make([]byte, len("from-node"))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("conn.Read: %v", err)
	}
	if !bytes.Equal(got, []byte("from-node")) {
		t.Errorf("read = %q, want from-node", got)
	}
}

// TestOpenPortBridgeNodeNotConnected pins the no-stream branch:
// without a RegisterNode stream open for the fingerprint, the
// bridge surfaces ErrNodeNotConnected before doing any further
// work.
func TestOpenPortBridgeNodeNotConnected(t *testing.T) {
	reg := nodectrl.NewRegistry(time.Now)
	h, err := NewNodeControlHandler(reg)
	if err != nil {
		t.Fatalf("NewNodeControlHandler: %v", err)
	}
	fp := identity.Fingerprint{0xcc}
	_, err = h.OpenPortBridge(context.Background(), fp, testOpenPortWorkspace, testOpenPortPort)
	if !errors.Is(err, ErrNodeNotConnected) {
		t.Errorf("err = %v, want ErrNodeNotConnected", err)
	}
}

// TestOpenPortBridgeTimesOut pins the no-stream-arrival branch:
// when the sandboxd never dials OpenPortStream, the bridge
// returns the ctx error and cleans up the pending entry.
func TestOpenPortBridgeTimesOut(t *testing.T) {
	h, fp, events := newTestHandler(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := h.OpenPortBridge(ctx, fp, testOpenPortWorkspace, testOpenPortPort)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	// The event was still pushed — drain it so the test cleans up.
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Error("event not pushed before timeout")
	}
	// Pending entry should be gone after the bridge returned.
	h.mu.Lock()
	pending := len(h.pendingPorts)
	h.mu.Unlock()
	if pending != 0 {
		t.Errorf("pendingPorts has %d entries, want 0", pending)
	}
}

// TestOpenPortBridgeRejectsEmptyInputs covers the input validation
// branches — empty workspace / zero port.
func TestOpenPortBridgeRejectsEmptyInputs(t *testing.T) {
	h, fp, _ := newTestHandler(t)
	if _, err := h.OpenPortBridge(context.Background(), fp, "", testOpenPortPort); err == nil {
		t.Error("expected error for empty workspaceUUID")
	}
	if _, err := h.OpenPortBridge(context.Background(), fp, testOpenPortWorkspace, 0); err == nil {
		t.Error("expected error for zero containerPort")
	}
}

// TestOpenPortBridgeBacklogSurface pins the full-event-buffer
// branch: a stalled Node whose event channel is saturated yields
// ErrSandboxEventBacklog rather than blocking the bridge.
func TestOpenPortBridgeBacklogSurface(t *testing.T) {
	h, fp, events := newTestHandler(t)
	// Fill the per-stream buffer.
	for range cap(events) {
		events <- &nodectrlv1.NodeEvent{}
	}
	_, err := h.OpenPortBridge(context.Background(), fp, testOpenPortWorkspace, testOpenPortPort)
	if !errors.Is(err, ErrSandboxEventBacklog) {
		t.Errorf("err = %v, want ErrSandboxEventBacklog", err)
	}
}

// TestRunOpenPortStreamRejectsBadHello pins the protocol-violation
// branch: a first frame that isn't a Hello (or has an empty
// session_id) tears the stream down with InvalidArgument.
func TestRunOpenPortStreamRejectsBadHello(t *testing.T) {
	h, _, _ := newTestHandler(t)
	for _, tc := range []struct {
		name string
		msg  *nodectrlv1.PortStreamMessage
	}{
		{"data instead of hello", &nodectrlv1.PortStreamMessage{
			Body: &nodectrlv1.PortStreamMessage_Data{Data: []byte("nope")},
		}},
		{"empty session", &nodectrlv1.PortStreamMessage{
			Body: &nodectrlv1.PortStreamMessage_Hello{
				Hello: &nodectrlv1.PortStreamHello{SessionId: ""},
			},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := newFakeServerStream()
			stream.push(tc.msg)
			err := h.runOpenPortStream(context.Background(), stream)
			if !errors.Is(err, ErrHelloMissingSession) {
				t.Errorf("err = %v, want ErrHelloMissingSession", err)
			}
		})
	}
}

// TestRunOpenPortStreamRejectsUnknownSession pins the stale-stream
// branch: a sandboxd dialing OpenPortStream for a session the
// orchestrator no longer remembers (because the bridge timed out)
// gets FailedPrecondition with ErrUnknownPortSession.
func TestRunOpenPortStreamRejectsUnknownSession(t *testing.T) {
	h, _, _ := newTestHandler(t)
	stream := newFakeServerStream()
	stream.push(&nodectrlv1.PortStreamMessage{
		Body: &nodectrlv1.PortStreamMessage_Hello{
			Hello: &nodectrlv1.PortStreamHello{SessionId: "ghost-session"},
		},
	})
	err := h.runOpenPortStream(context.Background(), stream)
	if !errors.Is(err, ErrUnknownPortSession) {
		t.Errorf("err = %v, want ErrUnknownPortSession", err)
	}
}

// TestRunOpenPortStreamFirstReceiveError pins the transport-error
// branch: a stream that fails before the Hello arrives surfaces a
// wrapped error rather than silently consuming.
func TestRunOpenPortStreamFirstReceiveError(t *testing.T) {
	h, _, _ := newTestHandler(t)
	want := errors.New("synthetic recv fail")
	stream := newFakeServerStream()
	stream.recvErr = want
	err := h.runOpenPortStream(context.Background(), stream)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestServerPortStreamConnCloseIsIdempotent + concurrent: a
// second Close must not panic on close-of-closed-channel.
func TestServerPortStreamConnCloseIsIdempotent(t *testing.T) {
	conn := newServerPortStreamConn(newFakeServerStream(), make(chan struct{}))
	if err := conn.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestServerPortStreamConnReadResidualBuffer pins multi-Read
// retrieval of a single large Data frame.
func TestServerPortStreamConnReadResidualBuffer(t *testing.T) {
	stream := newFakeServerStream()
	conn := newServerPortStreamConn(stream, make(chan struct{}))
	stream.push(&nodectrlv1.PortStreamMessage{
		Body: &nodectrlv1.PortStreamMessage_Data{Data: []byte("abcdef")},
	})
	first := make([]byte, 3)
	if n, err := conn.Read(first); err != nil || n != 3 {
		t.Fatalf("first Read = %d, %v", n, err)
	}
	second := make([]byte, 3)
	if n, err := conn.Read(second); err != nil || n != 3 {
		t.Fatalf("second Read = %d, %v", n, err)
	}
	if string(first)+string(second) != "abcdef" {
		t.Errorf("got %q+%q, want abcdef", first, second)
	}
}

// TestServerPortStreamConnWriteAfterCloseErrsClosed pins the
// closed-after-Write surface so a stray Write doesn't quietly
// race the underlying stream's own error.
func TestServerPortStreamConnWriteAfterCloseErrsClosed(t *testing.T) {
	conn := newServerPortStreamConn(newFakeServerStream(), make(chan struct{}))
	_ = conn.Close()
	_, err := conn.Write([]byte("x"))
	if !errors.Is(err, net.ErrClosed) {
		t.Errorf("err = %v, want net.ErrClosed", err)
	}
}

// TestServerPortStreamConnAddrShape pins the cosmetic Addr surface
// trace lines and log scanners depend on.
func TestServerPortStreamConnAddrShape(t *testing.T) {
	conn := newServerPortStreamConn(newFakeServerStream(), make(chan struct{}))
	if conn.LocalAddr().Network() != "daft-port-stream" {
		t.Errorf("Network = %q", conn.LocalAddr().Network())
	}
	if conn.LocalAddr().String() != "daft-port-stream:local" {
		t.Errorf("LocalAddr = %q", conn.LocalAddr().String())
	}
	if conn.RemoteAddr().String() != "daft-port-stream:remote" {
		t.Errorf("RemoteAddr = %q", conn.RemoteAddr().String())
	}
	now := time.Now()
	if err := conn.SetDeadline(now); err != nil {
		t.Errorf("SetDeadline: %v", err)
	}
	if err := conn.SetReadDeadline(now); err != nil {
		t.Errorf("SetReadDeadline: %v", err)
	}
	if err := conn.SetWriteDeadline(now); err != nil {
		t.Errorf("SetWriteDeadline: %v", err)
	}
}

// TestMintSessionIDUnique sanity-checks the rendezvous-token
// generator: two consecutive mints must differ.
func TestMintSessionIDUnique(t *testing.T) {
	a, err := mintSessionID()
	if err != nil {
		t.Fatalf("mintSessionID: %v", err)
	}
	b, err := mintSessionID()
	if err != nil {
		t.Fatalf("mintSessionID: %v", err)
	}
	if a == b {
		t.Errorf("two mints returned the same id %q", a)
	}
	if len(a) < 32 {
		t.Errorf("session id shorter than expected: %d bytes", len(a))
	}
}
