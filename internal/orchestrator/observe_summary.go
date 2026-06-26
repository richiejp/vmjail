package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"connectrpc.com/connect"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/oops"
)

// maxObserveSummaryBytes caps a single summary snapshot read so a
// misbehaving or compromised node can't stream an unbounded body into
// the orchestrator. The summary is a small bounded-top-N JSON document
// (a few KiB in practice); 1 MiB is generous headroom that still fails
// closed.
const maxObserveSummaryBytes = 1 << 20

// ErrObserveSummaryHandleMissing is returned by ObserveSummary when the
// request carries no workspace handle.
var ErrObserveSummaryHandleMissing = errors.New("orchestrator: ObserveSummary requires a workspace handle")

// ObserveSummary is the orchestrator-side handler for
// WorkspaceCatalogService.ObserveSummary. It resolves the workspace +
// RUNNING gate, opens a one-shot summary read over the node broker (the
// ObserverTail rendezvous with the summary source selector), and
// returns the guest-written progress.Summary JSON verbatim. Unary: the
// CLI polls it for --follow.
func (h *WorkspaceCatalogHandler) ObserveSummary(
	ctx context.Context,
	req *connect.Request[workspacev1.ObserveSummaryRequest],
) (*connect.Response[workspacev1.ObserveSummaryResponse], error) {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, errUnauthenticated
	}
	data, err := h.readObserveSummary(ctx, fp, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&workspacev1.ObserveSummaryResponse{Summary: data}), nil
}

// readObserveSummary is the testable core: resolve + gate, open the
// summary bridge, read the whole (bounded) snapshot once, close.
func (h *WorkspaceCatalogHandler) readObserveSummary(
	ctx context.Context,
	callerFP identity.Fingerprint,
	req *workspacev1.ObserveSummaryRequest,
) ([]byte, error) {
	if h.observerBridger == nil {
		return nil, connect.NewError(connect.CodeUnimplemented,
			errors.New("orchestrator: ObserveSummary: no ObserverBridger configured"))
	}
	if req.GetWorkspace() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, ErrObserveSummaryHandleMissing)
	}
	uid, nodeFP, err := h.resolveRunningSandbox(callerFP, req.GetWorkspace())
	if err != nil {
		return nil, err
	}
	reader, err := h.observerBridger.OpenObserverBridge(ctx, nodeFP, uid,
		nodectrlv1.ObserverSource_OBSERVER_SOURCE_SUMMARY, false)
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	defer func() { oops.Skip("orchestrator: close summary reader", reader.Close(), net.ErrClosed) }()
	data, err := drainObserveSummary(reader)
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	return data, nil
}

// drainObserveSummary reads the whole one-shot summary snapshot from the
// node bridge, treating a clean stream close as success — EOF (possibly
// wrapped by connect when the bidi ObserverStream half-closes) or an
// expected pump-exit — mirroring pumpObserverToClient. io.ReadAll can't
// be used: it compares the terminal error against io.EOF by identity, so
// the connect-wrapped EOF would escape as a spurious "unknown: EOF" RPC
// error. The size cap is enforced inline (no io.LimitReader) so an
// oversized snapshot fails closed rather than silently truncating.
func drainObserveSummary(reader io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	chunk := make([]byte, observerCopyBufSize)
	//daft:lint:disable=forloopbound reason="exits when reader.Read returns err (clean EOF/expected-exit → data; otherwise the error)"
	for {
		n, err := reader.Read(chunk)
		if n > 0 {
			if buf.Len()+n > maxObserveSummaryBytes {
				return nil, connect.NewError(connect.CodeResourceExhausted,
					fmt.Errorf("orchestrator: ObserveSummary: snapshot exceeds %d bytes", maxObserveSummaryBytes))
			}
			buf.Write(chunk[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) || oops.IsExpectedPumpExit(err) {
				return buf.Bytes(), nil
			}
			return nil, err
		}
	}
}
