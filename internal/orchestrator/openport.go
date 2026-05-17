package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
)

// DefaultOpenPortHandshakeTimeout caps how long OpenPortBridge
// waits for the sandboxd to dial OpenPortStream after the
// OpenPortRequest event has been pushed. Above this the caller
// times out; the orphaned event will eventually surface as a
// stream-level error on the sandboxd side when its handler errors.
const DefaultOpenPortHandshakeTimeout = 10 * time.Second

// ErrUnknownPortSession is returned by OpenPortStream when the
// Hello frame's session_id does not match any pending bridge
// request. Indicates a sandboxd dialing a stale event after the
// caller already gave up, or — more concerning — a peer trying to
// spuriously open a stream without an orchestrator-issued event.
var ErrUnknownPortSession = errors.New("orchestrator: unknown OpenPort session")

// ErrHelloMissingSession is returned by OpenPortStream when the
// first frame isn't a Hello with a non-empty session_id. Protocol
// violation; the stream is torn down.
var ErrHelloMissingSession = errors.New("orchestrator: OpenPortStream first frame must be Hello with session_id")

// pendingPort is the rendezvous slot OpenPortBridge parks on
// while waiting for the matching sandboxd-initiated
// OpenPortStream to arrive.
type pendingPort struct {
	arrived chan portStreamArrival
}

// portStreamArrival is what OpenPortStream hands back to the
// waiting OpenPortBridge: the live bidi stream plus a release
// channel the bridge closes to signal "done with the stream",
// which lets the OpenPortStream handler return and tear the
// underlying HTTP/2 stream down.
type portStreamArrival struct {
	stream  serverPortStream
	release chan struct{}
}

// serverPortStream is the subset of the server-side
// connect.BidiStream surface this package consumes. Isolated as
// an interface so tests can plug in an in-process stub.
type serverPortStream interface {
	Send(*nodectrlv1.PortStreamMessage) error
	Receive() (*nodectrlv1.PortStreamMessage, error)
}

// Compile-time assertion that the connect-generated server stream
// satisfies serverPortStream.
var _ serverPortStream = (*connect.BidiStream[nodectrlv1.PortStreamMessage, nodectrlv1.PortStreamMessage])(nil)

// OpenPortBridge mints a session, pushes an OpenPortRequest event
// on the addressed Node's RegisterNode stream, waits for the
// sandboxd to dial OpenPortStream with the matching session_id,
// and returns a net.Conn wrapping the resulting byte channel.
//
// ctx's deadline (if any) bounds the wait; without one the call
// uses DefaultOpenPortHandshakeTimeout.
//
// Returns ErrNodeNotConnected when the Node has no open
// RegisterNode stream, ErrSandboxEventBacklog when the event
// channel is full, ctx.Err() on timeout, or the underlying error
// from the wait.
func (h *NodeControlHandler) OpenPortBridge(
	ctx context.Context,
	fp identity.Fingerprint,
	workspaceUUID string,
	containerPort uint32,
) (net.Conn, error) {
	if workspaceUUID == "" {
		return nil, errors.New("orchestrator: OpenPortBridge: empty workspaceUUID")
	}
	if containerPort == 0 {
		return nil, errors.New("orchestrator: OpenPortBridge: zero containerPort")
	}
	sessionID, err := mintSessionID()
	if err != nil {
		return nil, fmt.Errorf("orchestrator: mint session id: %w", err)
	}
	pending := &pendingPort{arrived: make(chan portStreamArrival, 1)}
	h.mu.Lock()
	h.pendingPorts[sessionID] = pending
	h.mu.Unlock()
	// Defer cleanup until we've handed the stream off; if the
	// stream never arrives we want the map entry gone.
	handedOff := false
	defer func() {
		if !handedOff {
			h.mu.Lock()
			delete(h.pendingPorts, sessionID)
			h.mu.Unlock()
		}
	}()

	if err := h.pushOpenPortRequest(fp, sessionID, workspaceUUID, containerPort); err != nil {
		return nil, err
	}

	waitCtx, cancel := waitContext(ctx)
	defer cancel()
	select {
	case arrival := <-pending.arrived:
		handedOff = true
		return newServerPortStreamConn(arrival.stream, arrival.release), nil
	case <-waitCtx.Done():
		return nil, fmt.Errorf("orchestrator: wait for OpenPortStream session=%s: %w", sessionID, waitCtx.Err())
	}
}

// waitContext returns ctx as-is if it already carries a deadline;
// otherwise it derives a child context with DefaultOpenPortHandshakeTimeout
// so callers passing context.Background don't park forever waiting
// for a sandboxd that will never dial.
func waitContext(parent context.Context) (context.Context, context.CancelFunc) {
	if _, ok := parent.Deadline(); ok {
		return parent, func() {}
	}
	return context.WithTimeout(parent, DefaultOpenPortHandshakeTimeout)
}

// pushOpenPortRequest emits the OpenPortRequest event on the
// Node's RegisterNode stream channel. Shares the same buffer +
// backlog semantics as PublishSandboxEvent so a stalled Node
// surfaces as ErrSandboxEventBacklog rather than blocking.
func (h *NodeControlHandler) pushOpenPortRequest(
	fp identity.Fingerprint,
	sessionID, workspaceUUID string,
	containerPort uint32,
) error {
	h.mu.Lock()
	ch, ok := h.streams[fp]
	h.mu.Unlock()
	if !ok {
		return ErrNodeNotConnected
	}
	ev := &nodectrlv1.NodeEvent{
		Event: &nodectrlv1.NodeEvent_OpenPort{
			OpenPort: &nodectrlv1.OpenPortRequest{
				SessionId:     sessionID,
				WorkspaceUuid: workspaceUUID,
				ContainerPort: containerPort,
			},
		},
	}
	select {
	case ch <- ev:
		return nil
	default:
		return ErrSandboxEventBacklog
	}
}

// OpenPortStream is the orchestrator-side handler for the bidi
// RPC the sandboxd dials in response to an OpenPortRequest event.
//
//  1. Read the first frame; must be a PortStreamHello with the
//     session_id the event announced.
//  2. Look the session up in pendingPorts; if absent, the bridge
//     either timed out or never existed — reject.
//  3. Hand the stream off to OpenPortBridge via the arrival
//     channel, then block until that bridge closes its release
//     signal. Returning here closes the underlying HTTP/2 stream
//     and signals EOF to the sandboxd.
func (h *NodeControlHandler) OpenPortStream(
	ctx context.Context,
	stream *connect.BidiStream[nodectrlv1.PortStreamMessage, nodectrlv1.PortStreamMessage],
) error {
	return h.runOpenPortStream(ctx, stream)
}

// runOpenPortStream is the testable core, taking the narrow
// serverPortStream interface so in-process stubs can drive it
// without standing up a real Connect server.
//
//nolint:dupl // structurally parallel to runAttachStream in attach.go; proto types differ and a generic merge would obscure type checking. Side-by-side review is the intent.
func (h *NodeControlHandler) runOpenPortStream(ctx context.Context, stream serverPortStream) error {
	msg, err := stream.Receive()
	if err != nil {
		return fmt.Errorf("orchestrator: OpenPortStream first Receive: %w", err)
	}
	hello := msg.GetHello()
	if hello == nil || hello.GetSessionId() == "" {
		return connect.NewError(connect.CodeInvalidArgument, ErrHelloMissingSession)
	}
	sessionID := hello.GetSessionId()
	h.mu.Lock()
	pending, ok := h.pendingPorts[sessionID]
	if ok {
		delete(h.pendingPorts, sessionID)
	}
	h.mu.Unlock()
	if !ok {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%w: %s", ErrUnknownPortSession, sessionID))
	}
	release := make(chan struct{})
	pending.arrived <- portStreamArrival{stream: stream, release: release}
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// mintSessionID returns a fresh 32-byte random base64-url token.
// Same flavour as workspacebroker session ids.
func mintSessionID() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// serverPortStreamConn adapts a serverPortStream as a net.Conn for
// the OpenPortBridge caller (typically the C2 HTTP ingress
// listener). Write encodes a Data frame; Read decodes inbound
// Data frames with residual buffering; Close signals the
// OpenPortStream handler to return.
//
// Deadlines are no-ops in v1 — the caller's ctx and stream-level
// lifetime handle timeouts.
type serverPortStreamConn struct {
	stream  serverPortStream
	release chan struct{}

	closed    atomic.Bool
	closeOnce sync.Once
	readMu    sync.Mutex
	readBuf   []byte
	writeMu   sync.Mutex
}

func newServerPortStreamConn(stream serverPortStream, release chan struct{}) *serverPortStreamConn {
	return &serverPortStreamConn{stream: stream, release: release}
}

//nolint:dupl // structurally parallel to forwardFrameConn.Read in forwardport.go; proto types differ (PortStreamMessage vs ForwardFrame) so factoring this out would require generics that obscure type checking. Both adapters live in this package and are kept side-by-side for review.
func (c *serverPortStreamConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	for len(c.readBuf) == 0 {
		if c.closed.Load() {
			return 0, io.EOF
		}
		msg, err := c.stream.Receive()
		if err != nil {
			return 0, err
		}
		c.readBuf = msg.GetData()
	}
	n := copy(p, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}

func (c *serverPortStreamConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.stream.Send(&nodectrlv1.PortStreamMessage{
		Body: &nodectrlv1.PortStreamMessage_Data{Data: p},
	}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close marks the conn closed and signals OpenPortStream to return.
// Idempotent — a second Close is a no-op (sync.Once gates the
// channel close).
func (c *serverPortStreamConn) Close() error {
	c.closed.Store(true)
	c.closeOnce.Do(func() { close(c.release) })
	return nil
}

func (c *serverPortStreamConn) LocalAddr() net.Addr  { return serverPortStreamAddr("local") }
func (c *serverPortStreamConn) RemoteAddr() net.Addr { return serverPortStreamAddr("remote") }

func (c *serverPortStreamConn) SetDeadline(_ time.Time) error      { return nil }
func (c *serverPortStreamConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *serverPortStreamConn) SetWriteDeadline(_ time.Time) error { return nil }

type serverPortStreamAddr string

func (serverPortStreamAddr) Network() string  { return "daft-port-stream" }
func (a serverPortStreamAddr) String() string { return "daft-port-stream:" + string(a) }
