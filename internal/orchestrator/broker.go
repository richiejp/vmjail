package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	workspacebrokerv1 "github.com/richiejp/darkaifactory/internal/genpb/workspacebroker/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspacebroker/v1/workspacebrokerv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/workspacebroker"
)

// notifChanBuffer is the per-subscriber backlog. Several events
// can pile up between two Recv() calls when a stream is briefly
// slow; the matcher drops a subscriber that can't keep up (see
// matcher.go's trySend), so this is just a smoothing window, not
// a queue.
const notifChanBuffer = 8

// BrokerHandler implements workspacebroker.BrokerServiceHandler
// by adapting the generic workspacebroker.Matcher to the
// Connect-RPC interface. Peer identity for every RPC is the
// mTLS leaf-cert fingerprint surfaced via withPeerMiddleware.
type BrokerHandler struct {
	workspacebrokerv1connect.UnimplementedBrokerServiceHandler
	matcher         *workspacebroker.Matcher
	relayEndpointFn func() string
}

// NewBrokerHandler returns a handler whose RequestAttach
// responses use relayEndpointFn to resolve the orchestrator's
// `host:port`. The function is called once per RequestAttach so
// it can return the actual bound address even if the handler
// was constructed before Listen.
func NewBrokerHandler(m *workspacebroker.Matcher, relayEndpointFn func() string) (*BrokerHandler, error) {
	if m == nil {
		return nil, errors.New("orchestrator: nil matcher")
	}
	if relayEndpointFn == nil {
		return nil, errors.New("orchestrator: nil relay endpoint resolver")
	}
	return &BrokerHandler{matcher: m, relayEndpointFn: relayEndpointFn}, nil
}

// RegisterServe subscribes a user-host's `daft workspace serve`
// and pushes PendingAttach events down the stream until the
// caller's context is cancelled. Identity is taken from the
// mTLS leaf cert; workspace name comes from the request body.
func (h *BrokerHandler) RegisterServe(
	ctx context.Context,
	req *connect.Request[workspacebrokerv1.RegisterServeRequest],
	stream *connect.ServerStream[workspacebrokerv1.RegistrationEvent],
) error {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated,
			errors.New("orchestrator: no mTLS peer identity"))
	}
	workspace := req.Msg.GetWorkspace()
	if workspace == "" {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New("orchestrator: empty workspace name"))
	}

	notif := make(chan *workspacebroker.PendingAttach, notifChanBuffer)
	cancel, err := h.matcher.SubscribeServe(
		workspacebroker.ServeKey{Identity: fp, Workspace: workspace}, notif,
	)
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	defer cancel()

	return pumpAttachEvents(ctx, notif, stream)
}

// RegisterSandbox subscribes a sandbox-host's control channel.
// Identity (sandbox owner) is the mTLS fingerprint; sandbox_id
// is supplied by the caller and must be unique within the
// orchestrator at any given time (last-register-wins).
func (h *BrokerHandler) RegisterSandbox(
	ctx context.Context,
	req *connect.Request[workspacebrokerv1.RegisterSandboxRequest],
	stream *connect.ServerStream[workspacebrokerv1.RegistrationEvent],
) error {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated,
			errors.New("orchestrator: no mTLS peer identity"))
	}
	sandboxID := req.Msg.GetSandboxId()
	if sandboxID == "" {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New("orchestrator: empty sandbox_id"))
	}

	notif := make(chan *workspacebroker.PendingAttach, notifChanBuffer)
	cancel, err := h.matcher.SubscribeSandbox(sandboxID, fp, notif)
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	defer cancel()

	return pumpAttachEvents(ctx, notif, stream)
}

// RequestAttach pairs a registered serve with a registered
// sandbox and returns the session_id + relay endpoint. The
// caller (always a user-host running `daft workspace attach`)
// must own both the serve identity and the sandbox; we enforce
// caller == serve_fingerprint here and the matcher enforces
// caller == sandbox owner.
func (h *BrokerHandler) RequestAttach(
	ctx context.Context,
	req *connect.Request[workspacebrokerv1.RequestAttachRequest],
) (*connect.Response[workspacebrokerv1.RequestAttachResponse], error) {
	caller, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated,
			errors.New("orchestrator: no mTLS peer identity"))
	}
	serveFP, err := identity.ParseFingerprint(req.Msg.GetServeFingerprint())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("orchestrator: parse serve_fingerprint: %w", err))
	}
	if caller != serveFP {
		return nil, connect.NewError(connect.CodePermissionDenied,
			errors.New("orchestrator: caller is not the serve owner"))
	}
	workspace := req.Msg.GetWorkspace()
	sandboxID := req.Msg.GetSandboxId()
	if workspace == "" || sandboxID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("orchestrator: workspace and sandbox_id are required"))
	}

	sid, err := h.matcher.RequestAttach(
		caller,
		workspacebroker.ServeKey{Identity: serveFP, Workspace: workspace},
		sandboxID,
	)
	if err != nil {
		return nil, mapMatcherErr(err)
	}
	return connect.NewResponse(&workspacebrokerv1.RequestAttachResponse{
		SessionId:     sid,
		RelayEndpoint: h.relayEndpointFn(),
	}), nil
}

// Relay carries the byte-splice between the two peers of a
// brokered session. The orchestrator never sees plaintext — the
// payloads inside the splice are end-to-end mTLS between client
// and sandbox-host.
//
// Both peers must dial Relay independently after RequestAttach
// has minted a session_id and notified them. The first message on
// each side must be a RelayHello carrying that session_id; all
// subsequent messages are Data frames that the orchestrator copies
// to the partner's stream.
func (h *BrokerHandler) Relay(
	ctx context.Context,
	stream *connect.BidiStream[workspacebrokerv1.RelayMessage, workspacebrokerv1.RelayMessage],
) error {
	fp, ok := PeerFingerprintFromRequestHeader(stream.RequestHeader())
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated,
			errors.New("orchestrator: no mTLS peer identity"))
	}
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New("orchestrator: first relay message must be Hello"))
	}
	sessionID := hello.GetSessionId()
	if sessionID == "" {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New("orchestrator: empty session_id"))
	}

	adapter := &connectRelayAdapter{stream: stream}
	pairing, err := h.matcher.Sessions().Pair(ctx, sessionID, fp, adapter)
	if err != nil {
		return mapSessionErr(err)
	}
	defer pairing.Close()

	return workspacebroker.Splice(ctx, adapter, pairing.Partner, pairing.Done)
}

// connectRelayAdapter wraps the generated Connect bidi stream as a
// workspacebroker.DataStream. RecvData ignores Hello frames (a
// well-behaved peer only sends Hello as the first message, which
// is consumed by the Relay handler before splicing starts; an
// extra Hello mid-stream is treated as an empty Data frame for
// robustness).
type connectRelayAdapter struct {
	stream *connect.BidiStream[workspacebrokerv1.RelayMessage, workspacebrokerv1.RelayMessage]
}

func (a *connectRelayAdapter) RecvData() ([]byte, error) {
	msg, err := a.stream.Receive()
	if err != nil {
		return nil, err
	}
	return msg.GetData(), nil
}

func (a *connectRelayAdapter) SendData(data []byte) error {
	return a.stream.Send(&workspacebrokerv1.RelayMessage{
		Body: &workspacebrokerv1.RelayMessage_Data{Data: data},
	})
}

func mapSessionErr(err error) error {
	switch {
	case errors.Is(err, workspacebroker.ErrNoSession):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, workspacebroker.ErrWrongPeer):
		return connect.NewError(connect.CodePermissionDenied, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}

// pumpAttachEvents forwards PendingAttach values from notif to
// the Connect server stream until ctx is cancelled. The first
// event sent is a SubscriptionReady — without it the client's
// stream-open call would hang waiting for response headers
// (Connect-RPC over HTTP/2 only flushes them on first Send).
// A closed notif channel ends the stream cleanly.
func pumpAttachEvents(
	ctx context.Context,
	notif <-chan *workspacebroker.PendingAttach,
	stream *connect.ServerStream[workspacebrokerv1.RegistrationEvent],
) error {
	ready := &workspacebrokerv1.RegistrationEvent{
		Event: &workspacebrokerv1.RegistrationEvent_Ready{
			Ready: &workspacebrokerv1.SubscriptionReady{},
		},
	}
	if err := stream.Send(ready); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-notif:
			if !ok {
				return nil
			}
			msg := &workspacebrokerv1.RegistrationEvent{
				Event: &workspacebrokerv1.RegistrationEvent_Attach{
					Attach: &workspacebrokerv1.PendingAttach{
						SessionId:       ev.SessionID,
						PeerFingerprint: ev.PeerFingerprint.String(),
						Workspace:       ev.Workspace,
						SandboxId:       ev.SandboxID,
					},
				},
			}
			if err := stream.Send(msg); err != nil {
				return err
			}
		}
	}
}

func mapMatcherErr(err error) error {
	switch {
	case errors.Is(err, workspacebroker.ErrNoServe):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, workspacebroker.ErrNoSandbox):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, workspacebroker.ErrSubscriberGone):
		return connect.NewError(connect.CodeUnavailable, err)
	default:
		return connect.NewError(connect.CodeInternal, err)
	}
}
