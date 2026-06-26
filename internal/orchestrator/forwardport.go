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

	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

// ErrForwardHelloMissing is returned by ForwardPort when the
// first frame isn't a Hello or the Hello has no workspace handle.
// Protocol violation; the stream is torn down.
var ErrForwardHelloMissing = errors.New("orchestrator: ForwardPort first frame must be Hello with workspace + container_port")

// ErrForwardWorkspaceNotRunning is returned by ForwardPort when
// the addressed workspace has no current_sandbox (it isn't
// RUNNING). The caller must Launch the workspace before
// forwarding to it.
var ErrForwardWorkspaceNotRunning = errors.New("orchestrator: workspace has no current sandbox; not RUNNING")

// forwardServerStream is the subset of the server-side connect
// bidi stream surface ForwardPort needs. Isolated for tests.
type forwardServerStream interface {
	Send(*workspacev1.ForwardFrame) error
	Receive() (*workspacev1.ForwardFrame, error)
}

// Compile-time check that the generated server stream satisfies
// the narrow interface.
var _ forwardServerStream = (*connect.BidiStream[workspacev1.ForwardFrame, workspacev1.ForwardFrame])(nil)

// ForwardPort is the orchestrator-side handler for the bidi RPC
// a user-host CLI dials to tunnel local TCP traffic into a
// running workspace. The handler:
//
//  1. Reads the first frame; must be a Hello carrying a workspace
//     handle and a non-zero container_port.
//  2. Resolves the handle against the catalog (scoped to the
//     caller's mTLS identity for name lookups) and confirms
//     status.current_sandbox is set.
//  3. Calls portBridger.OpenPortBridge for (node, workspace_uuid,
//     container_port) to get a net.Conn into the sandboxd.
//  4. io.Copy splices the bidi stream with that net.Conn until
//     either side closes.
func (h *WorkspaceCatalogHandler) ForwardPort(
	ctx context.Context,
	stream *connect.BidiStream[workspacev1.ForwardFrame, workspacev1.ForwardFrame],
) error {
	fp, ok := PeerFingerprintFromRequestHeader(stream.RequestHeader())
	if !ok {
		return errUnauthenticated
	}
	return h.runForwardPort(ctx, fp, stream)
}

// runForwardPort is the testable core, parameterised over the
// narrow forwardServerStream interface and the caller fingerprint.
func (h *WorkspaceCatalogHandler) runForwardPort(
	ctx context.Context,
	callerFP identity.Fingerprint,
	stream forwardServerStream,
) error {
	if h.portBridger == nil {
		return connect.NewError(connect.CodeUnimplemented,
			errors.New("orchestrator: ForwardPort: no PortBridger configured"))
	}
	hello, err := readForwardHello(stream)
	if err != nil {
		return err
	}
	ws, err := h.resolveForwardWorkspace(callerFP, hello)
	if err != nil {
		return err
	}
	downstream, err := h.portBridger.OpenPortBridge(ctx,
		ws.Status.CurrentSandbox.NodeFingerprint,
		ws.Metadata.UUID, hello.GetContainerPort())
	if err != nil {
		return mapCatalogErr(err)
	}
	defer func() { oops.Skip("orchestrator: close forward downstream", downstream.Close(), net.ErrClosed) }()
	upstream := newForwardFrameConn(stream)
	defer func() { oops.Skip("orchestrator: close forward upstream", upstream.Close(), net.ErrClosed) }()
	return spliceConns(ctx, upstream, downstream)
}

// readForwardHello consumes the first frame from stream. Returns
// ErrForwardHelloMissing for any non-Hello, missing handle, or
// zero container_port.
func readForwardHello(stream forwardServerStream) (*workspacev1.ForwardHello, error) {
	msg, err := stream.Receive()
	if err != nil {
		return nil, fmt.Errorf("orchestrator: ForwardPort first Receive: %w", err)
	}
	h := msg.GetHello()
	if h == nil || h.GetWorkspace() == nil || h.GetContainerPort() == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, ErrForwardHelloMissing)
	}
	return h, nil
}

// resolveForwardWorkspace looks up the workspace by hello.handle
// (UUID-first; name lookups are scoped to callerFP) and confirms
// the workspace is running (current_sandbox set).
func (h *WorkspaceCatalogHandler) resolveForwardWorkspace(callerFP identity.Fingerprint, hello *workspacev1.ForwardHello) (workspacecatalog.Workspace, error) {
	ws, err := h.resolveHandle(callerFP, hello.GetWorkspace())
	if err != nil {
		return workspacecatalog.Workspace{}, err
	}
	if ws.Status.CurrentSandbox == nil || ws.Status.CurrentSandbox.UID == "" {
		return workspacecatalog.Workspace{}, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%w: workspace=%s", ErrForwardWorkspaceNotRunning, ws.Metadata.UUID))
	}
	return ws, nil
}

// spliceConns runs io.Copy in both directions and returns the
// first non-EOF / non-closed error. Closes both conns on ctx.Done
// so a hung tunnel doesn't outlive its request.
func spliceConns(ctx context.Context, a, b net.Conn) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			oops.Skip("orchestrator: ctx-close a", a.Close(), net.ErrClosed)
			oops.Skip("orchestrator: ctx-close b", b.Close(), net.ErrClosed)
		case <-done:
		}
	}()
	var wg sync.WaitGroup
	wg.Add(2)
	errCh := make(chan error, 2)
	go func() {
		defer wg.Done()
		_, err := io.Copy(b, a)
		errCh <- err
	}()
	go func() {
		defer wg.Done()
		_, err := io.Copy(a, b)
		errCh <- err
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			return err
		}
	}
	return nil
}

// forwardFrameConn adapts a forwardServerStream as a net.Conn so
// io.Copy can splice it with the downstream Bridge conn. Same
// structure as the OpenPort-side serverPortStreamConn but typed
// for ForwardFrame.
type forwardFrameConn struct {
	stream    forwardServerStream
	closed    atomic.Bool
	closeOnce sync.Once
	readMu    sync.Mutex
	readBuf   []byte
	writeMu   sync.Mutex
	// closeCh is closed by Close so a parent context can observe
	// teardown if it cares to.
	closeCh chan struct{}
}

func newForwardFrameConn(stream forwardServerStream) *forwardFrameConn {
	return &forwardFrameConn{stream: stream, closeCh: make(chan struct{})}
}

//nolint:dupl // structurally parallel to serverPortStreamConn.Read in openport.go; proto types differ (ForwardFrame vs PortStreamMessage) so factoring this out would require generics that obscure type checking. Both adapters live in this package and are kept side-by-side for review.
func (c *forwardFrameConn) Read(p []byte) (int, error) {
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

func (c *forwardFrameConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.stream.Send(&workspacev1.ForwardFrame{
		Body: &workspacev1.ForwardFrame_Data{Data: p},
	}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *forwardFrameConn) Close() error {
	c.closed.Store(true)
	c.closeOnce.Do(func() { close(c.closeCh) })
	return nil
}

func (c *forwardFrameConn) LocalAddr() net.Addr  { return forwardAddr("local") }
func (c *forwardFrameConn) RemoteAddr() net.Addr { return forwardAddr("remote") }

func (c *forwardFrameConn) SetDeadline(_ time.Time) error      { return nil }
func (c *forwardFrameConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *forwardFrameConn) SetWriteDeadline(_ time.Time) error { return nil }

type forwardAddr string

func (forwardAddr) Network() string  { return "daft-forward-port" }
func (a forwardAddr) String() string { return "daft-forward-port:" + string(a) }
