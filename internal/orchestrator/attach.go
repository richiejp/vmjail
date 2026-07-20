package orchestrator

import (
	"context"
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
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
)

// DefaultOpenAttachHandshakeTimeout caps how long OpenAttachBridge
// waits for the sandboxd to dial AttachStream after the
// AttachRequest event has been pushed. Above this the caller
// times out; the orphaned event will surface as a stream-level
// error on the sandboxd side when its handler errors.
const DefaultOpenAttachHandshakeTimeout = 10 * time.Second

// ErrUnknownAttachSession is returned by AttachStream when the
// Hello frame's session_id does not match any pending bridge
// request. Either a stale sandboxd dial or a spurious open.
var ErrUnknownAttachSession = errors.New("orchestrator: unknown Attach session")

// ErrAttachHelloMissingSession is returned by AttachStream when
// the first frame isn't a Hello with a non-empty session_id.
// Protocol violation; the stream is torn down.
var ErrAttachHelloMissingSession = errors.New("orchestrator: AttachStream first frame must be Hello with session_id")

// pendingAttach is the rendezvous slot OpenAttachBridge parks on
// while waiting for the matching sandboxd-initiated AttachStream
// to arrive.
type pendingAttach struct {
	arrived chan attachStreamArrival
}

type attachStreamArrival struct {
	stream  serverAttachStream
	release chan struct{}
}

// serverAttachStream is the subset of the server-side
// connect.BidiStream surface this package consumes. Isolated so
// tests can drive it in-process.
type serverAttachStream interface {
	Send(*nodectrlv1.AttachStreamMessage) error
	Receive() (*nodectrlv1.AttachStreamMessage, error)
}

var _ serverAttachStream = (*connect.BidiStream[nodectrlv1.AttachStreamMessage, nodectrlv1.AttachStreamMessage])(nil)

// OpenAttachBridge mints a session, pushes an AttachRequest event
// on the addressed Node's RegisterNode stream, waits for the
// sandboxd to dial AttachStream with the matching session_id,
// and returns an AttachSession wrapping the resulting bidi byte
// channel + window-resize side-channel.
//
// Returns ErrNodeNotConnected when the Node has no open
// RegisterNode stream, ErrSandboxEventBacklog when the event
// channel is full, ctx.Err() on timeout, or the underlying error
// from the wait.
func (h *NodeControlHandler) OpenAttachBridge(
	ctx context.Context,
	fp identity.Fingerprint,
	req sandboxgateway.AttachRequest,
) (sandboxgateway.AttachSession, error) {
	if req.SandboxUID == "" {
		return nil, errors.New("orchestrator: OpenAttachBridge: empty SandboxUID")
	}
	sessionID, err := mintSessionID()
	if err != nil {
		return nil, fmt.Errorf("orchestrator: mint session id: %w", err)
	}
	pending := &pendingAttach{arrived: make(chan attachStreamArrival, 1)}
	h.mu.Lock()
	h.pendingAttaches[sessionID] = pending
	h.mu.Unlock()
	handedOff := false
	defer func() {
		if !handedOff {
			h.mu.Lock()
			delete(h.pendingAttaches, sessionID)
			h.mu.Unlock()
		}
	}()

	if err := h.pushAttachRequest(fp, sessionID, req); err != nil {
		return nil, err
	}

	waitCtx, cancel := waitContextWithFallback(ctx, DefaultOpenAttachHandshakeTimeout)
	defer cancel()
	select {
	case arrival := <-pending.arrived:
		handedOff = true
		return newServerAttachSession(arrival.stream, arrival.release), nil
	case <-waitCtx.Done():
		return nil, fmt.Errorf("orchestrator: wait for AttachStream session=%s: %w", sessionID, waitCtx.Err())
	}
}

// pushAttachRequest emits the AttachRequest event on the Node's
// RegisterNode stream channel. Shares the same backlog semantics
// as PublishSandboxEvent.
func (h *NodeControlHandler) pushAttachRequest(
	fp identity.Fingerprint,
	sessionID string,
	req sandboxgateway.AttachRequest,
) error {
	h.mu.Lock()
	ch, ok := h.streams[fp]
	h.mu.Unlock()
	if !ok {
		return ErrNodeNotConnected
	}
	ev := &nodectrlv1.NodeEvent{
		Event: &nodectrlv1.NodeEvent_Attach{
			Attach: &nodectrlv1.AttachRequest{
				SessionId:  sessionID,
				SandboxUid: req.SandboxUID,
				Command:    req.Command,
				Tty:        req.TTY,
				Cols:       req.Cols,
				Rows:       req.Rows,
			},
		},
	}
	select {
	case ch.events <- ev:
		return nil
	default:
		return ErrSandboxEventBacklog
	}
}

// AttachStream is the orchestrator-side handler for the bidi
// RPC the sandboxd dials in response to an AttachRequest event.
// Mirrors OpenPortStream: read Hello → look up the pending
// rendezvous → hand off → block on release → return.
func (h *NodeControlHandler) AttachStream(
	ctx context.Context,
	stream *connect.BidiStream[nodectrlv1.AttachStreamMessage, nodectrlv1.AttachStreamMessage],
) error {
	if _, err := h.requireNode(stream.RequestHeader()); err != nil {
		return err
	}
	return h.runAttachStream(ctx, stream)
}

//nolint:dupl // structurally parallel to runOpenPortStream in openport.go; proto types differ (AttachStreamMessage vs PortStreamMessage) and merging would need generics that obscure type checking. Side-by-side review is the intent.
func (h *NodeControlHandler) runAttachStream(ctx context.Context, stream serverAttachStream) error {
	msg, err := stream.Receive()
	if err != nil {
		return fmt.Errorf("orchestrator: AttachStream first Receive: %w", err)
	}
	hello := msg.GetHello()
	if hello == nil || hello.GetSessionId() == "" {
		return connect.NewError(connect.CodeInvalidArgument, ErrAttachHelloMissingSession)
	}
	sessionID := hello.GetSessionId()
	h.mu.Lock()
	pending, ok := h.pendingAttaches[sessionID]
	if ok {
		delete(h.pendingAttaches, sessionID)
	}
	h.mu.Unlock()
	if !ok {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%w: %s", ErrUnknownAttachSession, sessionID))
	}
	release := make(chan struct{})
	pending.arrived <- attachStreamArrival{stream: stream, release: release}
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// serverAttachSession adapts a serverAttachStream as an
// AttachSession (net.Conn + Resize). The conn surface is symmetric
// with serverPortStreamConn — Read decodes data frames, Write
// encodes data frames, Close signals the AttachStream handler to
// return. Resize sends an AttachStreamResize frame on the same
// stream; the sandboxd's bridger forwards it to the in-VM PTY.
type serverAttachSession struct {
	stream  serverAttachStream
	release chan struct{}

	closed    atomic.Bool
	closeOnce sync.Once
	readMu    sync.Mutex
	readBuf   []byte
	writeMu   sync.Mutex
}

func newServerAttachSession(stream serverAttachStream, release chan struct{}) *serverAttachSession {
	return &serverAttachSession{stream: stream, release: release}
}

func (c *serverAttachSession) Read(p []byte) (int, error) {
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
		// Drop any non-data frame the sandbox might emit (the
		// agent never sends resize back, but be defensive).
		data := msg.GetData()
		if len(data) == 0 {
			continue
		}
		c.readBuf = data
	}
	n := copy(p, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}

func (c *serverAttachSession) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.stream.Send(&nodectrlv1.AttachStreamMessage{
		Body: &nodectrlv1.AttachStreamMessage_Data{Data: p},
	}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Resize forwards a window-resize control frame on the bidi
// stream. The writeMu serialises against in-flight Data sends so a
// resize never interleaves halfway through a data frame.
func (c *serverAttachSession) Resize(cols, rows uint32) error {
	if c.closed.Load() {
		return net.ErrClosed
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.stream.Send(&nodectrlv1.AttachStreamMessage{
		Body: &nodectrlv1.AttachStreamMessage_Resize{
			Resize: &nodectrlv1.AttachStreamResize{Cols: cols, Rows: rows},
		},
	})
}

func (c *serverAttachSession) Close() error {
	c.closed.Store(true)
	c.closeOnce.Do(func() { close(c.release) })
	return nil
}

func (c *serverAttachSession) LocalAddr() net.Addr  { return serverAttachAddr("local") }
func (c *serverAttachSession) RemoteAddr() net.Addr { return serverAttachAddr("remote") }

func (c *serverAttachSession) SetDeadline(_ time.Time) error      { return nil }
func (c *serverAttachSession) SetReadDeadline(_ time.Time) error  { return nil }
func (c *serverAttachSession) SetWriteDeadline(_ time.Time) error { return nil }

type serverAttachAddr string

func (serverAttachAddr) Network() string  { return "daft-attach-stream" }
func (a serverAttachAddr) String() string { return "daft-attach-stream:" + string(a) }
