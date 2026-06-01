package orchestrator

import (
	"context"
	"errors"
	"io"
	"net"

	"connectrpc.com/connect"

	nodectrlv1 "github.com/localai-org/darkaifactory/internal/genpb/nodectrl/v1"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/oops"
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
	data, err := io.ReadAll(io.LimitReader(reader, maxObserveSummaryBytes))
	if err != nil {
		return nil, mapCatalogErr(err)
	}
	return data, nil
}
