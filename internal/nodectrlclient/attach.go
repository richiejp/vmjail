package nodectrlclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"connectrpc.com/connect"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
)

// AttachBridger is the sandboxd-side seam the Node-side handler
// dispatches AttachRequest events to: open an attach session
// against the running sandbox (production: vsock to daft-agent —
// Slice D2b) and return an AttachSession the dispatcher splices
// to the AttachStream RPC.
//
// sandboxgateway.AttachBridger satisfies this implicitly. Defined
// here (rather than imported) for the same reason PortBridger is:
// this package stays transport-focused, the bridger is the
// side-effect surface this transport hands work to.
type AttachBridger interface {
	Attach(ctx context.Context, req sandboxgateway.AttachRequest) (sandboxgateway.AttachSession, error)
}

// handleAttach responds to a single AttachRequest event:
//
//  1. Dials AttachStream on the orchestrator.
//  2. Sends the Hello frame echoing session_id.
//  3. Opens an attach session via the configured AttachBridger.
//  4. Pumps bytes + resize frames between stream and session
//     until either side closes.
func (c *Client) handleAttach(ctx context.Context, req *nodectrlv1.AttachRequest) error {
	if c.cfg.AttachBridger == nil {
		return errors.New("nodectrlclient: AttachRequest received without an AttachBridger wired in Config")
	}
	return runAttach(ctx, req, c.ctrl.AttachStream(ctx), c.cfg.AttachBridger)
}

// runAttach is the pure-logic core split out for testability.
func runAttach(
	ctx context.Context,
	req *nodectrlv1.AttachRequest,
	stream attachStreamRPC,
	bridger AttachBridger,
) error {
	defer func() { oops.Skip("nodectrlclient: close AttachStream response", stream.CloseResponse(), io.EOF) }()
	if err := stream.Send(&nodectrlv1.AttachStreamMessage{
		Body: &nodectrlv1.AttachStreamMessage_Hello{
			Hello: &nodectrlv1.AttachStreamHello{SessionId: req.GetSessionId()},
		},
	}); err != nil {
		return fmt.Errorf("nodectrlclient: AttachStream Hello: %w", err)
	}
	session, err := bridger.Attach(ctx, sandboxgateway.AttachRequest{
		SessionID:  req.GetSessionId(),
		SandboxUID: req.GetSandboxUid(),
		Command:    req.GetCommand(),
		TTY:        req.GetTty(),
		Cols:       req.GetCols(),
		Rows:       req.GetRows(),
	})
	if err != nil {
		return fmt.Errorf("nodectrlclient: bridger.Attach sandbox=%s: %w", req.GetSandboxUid(), err)
	}
	defer func() { oops.Skip("nodectrlclient: close attach session", session.Close(), net.ErrClosed) }()
	return pumpAttachStream(stream, session)
}

// pumpAttachStream copies bytes + resize frames between the bidi
// AttachStream and the local AttachSession. Two halves, mirrored
// from the orchestrator-side pump.
func pumpAttachStream(stream attachStreamRPC, session sandboxgateway.AttachSession) error {
	done := make(chan struct{})
	var doneOnce sync.Once
	signalDone := func() { doneOnce.Do(func() { close(done) }) }

	// Watcher: tear both halves down on first goroutine exit so the
	// other goroutine isn't parked.
	go func() {
		<-done
		oops.Skip("nodectrlclient: close attach session on teardown", session.Close(), net.ErrClosed)
		oops.Skip("nodectrlclient: close attach stream recv", stream.CloseResponse(), io.EOF)
	}()

	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer signalDone()
		errCh <- pumpAttachStreamToSession(stream, session)
	}()
	go func() {
		defer wg.Done()
		defer signalDone()
		errCh <- pumpAttachSessionToStream(stream, session)
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

// pumpAttachStreamToSession reads frames from the bidi stream and
// routes data → session.Write, resize → session.Resize.
func pumpAttachStreamToSession(stream attachStreamRPC, session sandboxgateway.AttachSession) error {
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
		case *nodectrlv1.AttachStreamMessage_Data:
			if len(body.Data) == 0 {
				continue
			}
			if _, werr := session.Write(body.Data); werr != nil {
				if oops.IsExpectedPumpExit(werr) {
					return nil
				}
				return werr
			}
		case *nodectrlv1.AttachStreamMessage_Resize:
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
		case *nodectrlv1.AttachStreamMessage_Hello:
			// Subsequent Hello frames are dropped — only the
			// initial outgoing one is meaningful.
		}
	}
}

// pumpAttachSessionToStream reads from the AttachSession's
// net.Conn surface and wraps each chunk as an
// AttachStreamMessage{data} on the bidi stream. Per-Read
// append-copy is required because protobuf marshals []byte by
// reference and io.Copy-style buffer reuse would overwrite the
// in-flight Send.
func pumpAttachSessionToStream(stream attachStreamRPC, session sandboxgateway.AttachSession) error {
	buf := make([]byte, attachStreamBufSize)
	//daft:lint:disable=forloopbound reason="exits when session.Read returns err (peer close / cross-cancel close)"
	for {
		n, err := session.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&nodectrlv1.AttachStreamMessage{
				Body: &nodectrlv1.AttachStreamMessage_Data{Data: append([]byte(nil), buf[:n]...)},
			}); sendErr != nil {
				return sendErr
			}
		}
		if err != nil {
			var exit *sandboxgateway.AttachExitError
			if errors.As(err, &exit) {
				return stream.Send(&nodectrlv1.AttachStreamMessage{
					Body: &nodectrlv1.AttachStreamMessage_Exit{Exit: &nodectrlv1.AttachStreamExit{
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

// attachStreamBufSize matches the orchestrator-side copy buffer:
// 32 KiB is big enough to bypass bufio's small-buffer path and
// small enough to keep a stalled session from accumulating.
const attachStreamBufSize = 32 * 1024

// attachStreamRPC is the subset of the generated connect bidi
// stream the dispatcher needs. Isolated for tests.
type attachStreamRPC interface {
	Send(*nodectrlv1.AttachStreamMessage) error
	Receive() (*nodectrlv1.AttachStreamMessage, error)
	CloseRequest() error
	CloseResponse() error
}

var _ attachStreamRPC = (*connect.BidiStreamForClient[nodectrlv1.AttachStreamMessage, nodectrlv1.AttachStreamMessage])(nil)
