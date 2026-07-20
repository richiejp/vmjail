package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"connectrpc.com/connect"

	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

// ErrAttachHelloMissing is returned by AttachSandbox when the
// first frame isn't a Hello carrying a workspace handle.
// Protocol violation; the stream is torn down.
var ErrAttachHelloMissing = errors.New("orchestrator: AttachSandbox first frame must be Hello with workspace")

// ErrAttachWorkspaceNotRunning is returned by AttachSandbox when
// the addressed workspace has no current_sandbox.
var ErrAttachWorkspaceNotRunning = errors.New("orchestrator: workspace has no current sandbox; not RUNNING")

// attachServerStream is the subset of the server-side connect
// bidi stream that AttachSandbox uses. Isolated for tests.
type attachServerStream interface {
	Send(*workspacev1.AttachFrame) error
	Receive() (*workspacev1.AttachFrame, error)
}

var _ attachServerStream = (*connect.BidiStream[workspacev1.AttachFrame, workspacev1.AttachFrame])(nil)

// AttachSandbox is the orchestrator-side handler. Reads the Hello
// frame, resolves the workspace + RUNNING gate, calls
// OpenAttachBridge to get an AttachSession, and pumps bytes +
// resize frames between the client stream and the session.
func (h *WorkspaceCatalogHandler) AttachSandbox(
	ctx context.Context,
	stream *connect.BidiStream[workspacev1.AttachFrame, workspacev1.AttachFrame],
) error {
	fp, ok := PeerFingerprintFromRequestHeader(stream.RequestHeader())
	if !ok {
		return errUnauthenticated
	}
	return h.runAttachSandbox(ctx, fp, stream)
}

// runAttachSandbox is the testable core, parameterised over the
// narrow attachServerStream + caller fingerprint.
func (h *WorkspaceCatalogHandler) runAttachSandbox(
	ctx context.Context,
	callerFP identity.Fingerprint,
	stream attachServerStream,
) error {
	if h.attachBridger == nil {
		return connect.NewError(connect.CodeUnimplemented,
			errors.New("orchestrator: AttachSandbox: no AttachBridger configured"))
	}
	hello, err := readAttachHello(stream)
	if err != nil {
		return err
	}
	ws, err := h.resolveAttachWorkspace(callerFP, hello)
	if err != nil {
		return err
	}
	session, err := h.attachBridger.OpenAttachBridge(ctx, ws.Status.CurrentSandbox.NodeFingerprint,
		sandboxgateway.AttachRequest{
			SandboxUID: ws.Status.CurrentSandbox.UID,
			Command:    hello.GetCommand(),
			TTY:        hello.GetTty(),
			Cols:       hello.GetCols(),
			Rows:       hello.GetRows(),
		})
	if err != nil {
		return mapCatalogErr(err)
	}
	defer func() { oops.Skip("orchestrator: close attach session", session.Close(), net.ErrClosed) }()
	return pumpAttach(ctx, stream, session)
}

// readAttachHello consumes the first frame from stream. Returns
// ErrAttachHelloMissing for any non-Hello or missing workspace
// handle.
func readAttachHello(stream attachServerStream) (*workspacev1.AttachHello, error) {
	msg, err := stream.Receive()
	if err != nil {
		return nil, fmt.Errorf("orchestrator: AttachSandbox first Receive: %w", err)
	}
	h := msg.GetHello()
	if h == nil || h.GetWorkspace() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, ErrAttachHelloMissing)
	}
	return h, nil
}

// resolveAttachWorkspace looks up the workspace and confirms it
// is RUNNING. Same shape as resolveForwardWorkspace.
func (h *WorkspaceCatalogHandler) resolveAttachWorkspace(callerFP identity.Fingerprint, hello *workspacev1.AttachHello) (workspacecatalog.Workspace, error) {
	ws, err := h.resolveHandle(callerFP, hello.GetWorkspace())
	if err != nil {
		return workspacecatalog.Workspace{}, err
	}
	if ws.Status.CurrentSandbox == nil || ws.Status.CurrentSandbox.UID == "" {
		return workspacecatalog.Workspace{}, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%w: workspace=%s", ErrAttachWorkspaceNotRunning, ws.Metadata.UUID))
	}
	return ws, nil
}

// pumpAttach copies bytes + resize frames between the client
// bidi stream and the attach session. Two goroutines:
//
//   - client → session: pull AttachFrames from stream; data frames
//     write to session, resize frames call session.Resize, hellos
//     are dropped (already consumed once).
//   - session → client: read bytes from session.Read; wrap as
//     AttachFrame{data} and send on the stream.
//
// First exit closes the session so the parked half unblocks.
// Returns the first non-EOF / non-closed error.
func pumpAttach(ctx context.Context, stream attachServerStream, session sandboxgateway.AttachSession) error {
	done := make(chan struct{})
	var doneOnce sync.Once
	signalDone := func() { doneOnce.Do(func() { close(done) }) }

	go func() {
		select {
		case <-ctx.Done():
			oops.Skip("orchestrator: ctx-close attach session", session.Close(), net.ErrClosed)
		case <-done:
		}
	}()

	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer signalDone()
		errCh <- pumpAttachClientToSession(stream, session)
	}()
	go func() {
		defer wg.Done()
		defer signalDone()
		errCh <- pumpAttachSessionToClient(stream, session)
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err == nil {
			continue
		}
		if oops.IsExpectedPumpExit(err) {
			continue
		}
		return err
	}
	return nil
}

// pumpAttachClientToSession reads AttachFrames from the bidi
// stream and routes each to the session: Data → session.Write;
// Resize → session.Resize; Hello → drop (orchestrator already
// consumed the initial Hello; subsequent ones are protocol noise
// and tolerated rather than enforced).
func pumpAttachClientToSession(stream attachServerStream, session sandboxgateway.AttachSession) error {
	//daft:lint:disable=forloopbound reason="exits when stream.Receive returns err (peer close / cross-cancel close)"
	for {
		msg, err := stream.Receive()
		if err != nil {
			if oops.IsExpectedPumpExit(err) {
				return nil
			}
			return err
		}
		switch body := msg.GetBody().(type) {
		case *workspacev1.AttachFrame_Data:
			if len(body.Data) == 0 {
				continue
			}
			if _, werr := session.Write(body.Data); werr != nil {
				if oops.IsExpectedPumpExit(werr) {
					return nil
				}
				return werr
			}
		case *workspacev1.AttachFrame_Resize:
			r := body.Resize
			if r == nil {
				continue
			}
			if rerr := session.Resize(r.GetCols(), r.GetRows()); rerr != nil {
				if oops.IsExpectedPumpExit(rerr) {
					return nil
				}
				return rerr
			}
		case *workspacev1.AttachFrame_Hello:
			// Already consumed; ignore.
		}
	}
}

// pumpAttachSessionToClient reads from the session and forwards
// bytes as AttachFrame{data} on the client stream.
func pumpAttachSessionToClient(stream attachServerStream, session sandboxgateway.AttachSession) error {
	buf := make([]byte, attachCopyBufSize)
	//daft:lint:disable=forloopbound reason="exits when session.Read returns err (peer close / cross-cancel close)"
	for {
		n, err := session.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&workspacev1.AttachFrame{
				Body: &workspacev1.AttachFrame_Data{Data: append([]byte(nil), buf[:n]...)},
			}); sendErr != nil {
				return sendErr
			}
		}
		if err != nil {
			var exit *sandboxgateway.AttachExitError
			if errors.As(err, &exit) {
				return stream.Send(&workspacev1.AttachFrame{
					Body: &workspacev1.AttachFrame_Exit{Exit: &workspacev1.AttachExit{
						Code: exit.Code, Error: exit.Reason,
					}},
				})
			}
			if oops.IsExpectedPumpExit(err) {
				return nil
			}
			return err
		}
	}
}

// attachCopyBufSize matches forwardCopyBufSize: large enough to
// bypass the small-buffer bufio path, small enough that a stalled
// session doesn't accumulate.
const attachCopyBufSize = 32 * 1024
