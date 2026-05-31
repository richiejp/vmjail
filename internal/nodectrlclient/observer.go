package nodectrlclient

import (
	"context"
	"errors"
	"fmt"
	"io"

	"connectrpc.com/connect"

	nodectrlv1 "github.com/localai-org/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/localai-org/darkaifactory/internal/observertail"
	"github.com/localai-org/darkaifactory/internal/oops"
)

// ObserverResolver maps a sandbox_uid to its host-side observer-log
// path. sandboxgateway.ObserverRegistry satisfies it implicitly;
// declared here (not imported) so this package stays transport-focused
// and the registry is just the side-effect surface it reads from.
type ObserverResolver interface {
	Resolve(sandboxUID string) (string, error)
}

// observerTailFunc is the file-tail seam (production:
// observertail.Stream), injected so runObserver is testable without a
// real growing file.
type observerTailFunc func(ctx context.Context, path string, follow bool, w io.Writer) error

// handleObserver responds to a single ObserverRequest event:
//
//  1. Dials ObserverStream on the orchestrator.
//  2. Sends the Hello frame echoing session_id.
//  3. Resolves the sandbox's observer-log path.
//  4. Streams the log (dump, or tail -f when follow) as data frames
//     until the file ends or the orchestrator closes its side.
func (c *Client) handleObserver(ctx context.Context, req *nodectrlv1.ObserverRequest) error {
	if c.cfg.ObserverResolver == nil {
		return errors.New("nodectrlclient: ObserverRequest received without an ObserverResolver wired in Config")
	}
	return runObserver(ctx, req, c.ctrl.ObserverStream(ctx), c.cfg.ObserverResolver, observertail.Stream)
}

// runObserver is the pure-logic core split out for testability.
func runObserver(
	ctx context.Context,
	req *nodectrlv1.ObserverRequest,
	stream observerStreamRPC,
	resolver ObserverResolver,
	tail observerTailFunc,
) error {
	defer func() { oops.Skip("nodectrlclient: close ObserverStream response", stream.CloseResponse(), io.EOF) }()
	if err := stream.Send(&nodectrlv1.ObserverStreamMessage{
		Body: &nodectrlv1.ObserverStreamMessage_Hello{
			Hello: &nodectrlv1.ObserverStreamHello{SessionId: req.GetSessionId()},
		},
	}); err != nil {
		return fmt.Errorf("nodectrlclient: ObserverStream Hello: %w", err)
	}
	// Close the send direction on every exit path (resolve error, tail
	// error, or a completed dump) so the orchestrator's reader sees EOF
	// rather than blocking forever. Runs before the response close above.
	defer func() { oops.Skip("nodectrlclient: close ObserverStream request", stream.CloseRequest(), io.EOF) }()

	path, err := resolver.Resolve(req.GetSandboxUid())
	if err != nil {
		return fmt.Errorf("nodectrlclient: resolve observer sandbox=%s: %w", req.GetSandboxUid(), err)
	}

	// Stop the tail when the orchestrator closes its side (the client
	// hit Ctrl-C). The orchestrator never sends data on this stream, so
	// any Receive result — frame or error — is the stop signal.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go watchObserverCancel(stream, cancel)

	tailErr := tail(ctx, path, req.GetFollow(), observerFrameWriter{stream: stream})
	if tailErr != nil && !oops.IsExpectedPumpExit(tailErr) {
		return tailErr
	}
	return nil
}

// watchObserverCancel cancels the tail as soon as the orchestrator
// closes (or sends anything on) the return direction.
func watchObserverCancel(stream observerStreamRPC, cancel context.CancelFunc) {
	defer cancel()
	//daft:lint:disable=forloopbound reason="exits when stream.Receive returns err (orchestrator/client close); cancel() then unblocks the tail"
	for {
		if _, err := stream.Receive(); err != nil {
			return
		}
	}
}

// observerFrameWriter adapts the bidi stream to io.Writer: each Write
// is wrapped as an ObserverStreamMessage{data}. The append-copy is
// required because observertail reuses its read buffer across Writes
// and protobuf marshals []byte by reference.
type observerFrameWriter struct {
	stream observerStreamRPC
}

func (o observerFrameWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := o.stream.Send(&nodectrlv1.ObserverStreamMessage{
		Body: &nodectrlv1.ObserverStreamMessage_Data{Data: append([]byte(nil), p...)},
	}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// observerStreamRPC is the subset of the generated connect bidi
// stream the handler needs. Isolated for tests.
type observerStreamRPC interface {
	Send(*nodectrlv1.ObserverStreamMessage) error
	Receive() (*nodectrlv1.ObserverStreamMessage, error)
	CloseRequest() error
	CloseResponse() error
}

var _ observerStreamRPC = (*connect.BidiStreamForClient[nodectrlv1.ObserverStreamMessage, nodectrlv1.ObserverStreamMessage])(nil)
