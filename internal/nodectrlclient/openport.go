package nodectrlclient

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
	"github.com/richiejp/darkaifactory/internal/oops"
)

// PortBridger is the interface the Node-side handler dispatches
// OpenPortRequest events to: look the (workspaceUUID, port) tuple
// up in a local routing table, dial whatever backend serves it,
// and pump bytes through conn until either side closes.
//
// sandboxgateway.Bridger satisfies this implicitly. Defined here
// (rather than imported from sandboxgateway) to keep this package
// transport-focused — the bridger is the side-effect surface this
// transport hands work to.
type PortBridger interface {
	Bridge(ctx context.Context, workspaceUUID string, port uint32, upstream net.Conn) error
}

// handleOpenPort responds to a single OpenPortRequest event:
//
//  1. Dials OpenPortStream on the orchestrator (a fresh bidi RPC
//     on the same outer mTLS conn).
//  2. Hands off to runOpenPort which sends Hello + bridges through
//     the configured PortBridger.
//
// Returning a non-nil error tears down the RegisterNode stream;
// the caller's dispatchEvent decides whether to surface it.
func (c *Client) handleOpenPort(ctx context.Context, req *nodectrlv1.OpenPortRequest) error {
	if c.cfg.Bridger == nil {
		return errors.New("nodectrlclient: OpenPortRequest received without a Bridger wired in Config")
	}
	return runOpenPort(ctx, req, c.ctrl.OpenPortStream(ctx), c.cfg.Bridger)
}

// runOpenPort is the pure-logic core: send Hello, wrap stream as
// a net.Conn, hand off to bridger.Bridge. Split out from
// handleOpenPort so unit tests can drive it with an in-process
// portStreamRPC stub instead of standing up a real Connect server.
func runOpenPort(
	ctx context.Context,
	req *nodectrlv1.OpenPortRequest,
	stream portStreamRPC,
	bridger PortBridger,
) error {
	defer func() { oops.Skip("nodectrlclient: close OpenPortStream response", stream.CloseResponse(), io.EOF) }()
	if err := stream.Send(&nodectrlv1.PortStreamMessage{
		Body: &nodectrlv1.PortStreamMessage_Hello{
			Hello: &nodectrlv1.PortStreamHello{SessionId: req.GetSessionId()},
		},
	}); err != nil {
		return fmt.Errorf("nodectrlclient: OpenPortStream Hello: %w", err)
	}
	conn := newPortStreamConn(stream)
	defer func() { oops.Skip("nodectrlclient: close port stream conn", conn.Close(), net.ErrClosed) }()
	if err := bridger.Bridge(ctx, req.GetWorkspaceUuid(), req.GetContainerPort(), conn); err != nil {
		return fmt.Errorf("nodectrlclient: bridge workspace=%s port=%d: %w",
			req.GetWorkspaceUuid(), req.GetContainerPort(), err)
	}
	return nil
}

// portStreamConn adapts a connect bidi stream of PortStreamMessage
// as a net.Conn. The first inbound Hello frame has already been
// consumed by handleOpenPort's caller (in the symmetric receive on
// the orchestrator side); from here on we read GetData frames and
// write payload bytes wrapped in PortStreamMessage{Data:...}.
//
// Deadlines are no-ops in v1 — the caller's ctx and the
// orchestrator-side session TTL handle most timeouts. Read/Write
// are independently safe to call from one goroutine each, matching
// net.Conn's convention.
type portStreamConn struct {
	stream  portStreamRPC
	closed  atomic.Bool
	readMu  sync.Mutex
	readBuf []byte
	writeMu sync.Mutex
}

// portStreamRPC is the subset of the generated connect bidi stream
// the conn needs; isolated as an interface so tests can plug in an
// in-process stub without dragging the connect machinery in.
type portStreamRPC interface {
	Send(*nodectrlv1.PortStreamMessage) error
	Receive() (*nodectrlv1.PortStreamMessage, error)
	CloseRequest() error
	CloseResponse() error
}

func newPortStreamConn(stream portStreamRPC) *portStreamConn {
	return &portStreamConn{stream: stream}
}

// Read returns up to len(p) bytes. A single PortStreamMessage may
// be larger than p; the residual is buffered and served on
// subsequent Reads. Returns io.EOF when Receive returns io.EOF.
func (c *portStreamConn) Read(p []byte) (int, error) {
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
		// A spurious Hello frame after the handshake is treated as
		// the empty frame it morally is. Any non-Data frame after
		// connect produces no payload bytes — loop again.
		c.readBuf = msg.GetData()
	}
	n := copy(p, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}

// Write sends p as a single Data frame. Returns net.ErrClosed if
// the conn has been closed locally.
func (c *portStreamConn) Write(p []byte) (int, error) {
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

// Close marks the conn as closed and half-closes the send side of
// the underlying bidi stream so the peer sees EOF promptly.
func (c *portStreamConn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	return c.stream.CloseRequest()
}

func (c *portStreamConn) LocalAddr() net.Addr  { return portStreamAddr("local") }
func (c *portStreamConn) RemoteAddr() net.Addr { return portStreamAddr("remote") }

// SetDeadline / SetReadDeadline / SetWriteDeadline are no-ops in
// v1. Stream-level timeouts ride on the caller's ctx; once C2 is
// in we can add real per-direction deadlines.
func (c *portStreamConn) SetDeadline(_ time.Time) error      { return nil }
func (c *portStreamConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *portStreamConn) SetWriteDeadline(_ time.Time) error { return nil }

type portStreamAddr string

func (portStreamAddr) Network() string  { return "daft-port-stream" }
func (a portStreamAddr) String() string { return "daft-port-stream:" + string(a) }

// Compile-time assertion that the connect-generated client stream
// satisfies the portStreamRPC interface we drive in tests.
var _ portStreamRPC = (*connect.BidiStreamForClient[nodectrlv1.PortStreamMessage, nodectrlv1.PortStreamMessage])(nil)
