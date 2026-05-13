package orchestrator

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/richiejp/darkaifactory/internal/assert"
	sandboxctrlv1 "github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1/sandboxctrlv1connect"
	"github.com/richiejp/darkaifactory/internal/sandboxctrl"
)

// SandboxControlHandler adapts the sandboxctrl.Registry to the
// Connect-RPC SandboxControlService surface. Peer identity for
// every RPC is the mTLS leaf-cert fingerprint surfaced via
// withPeerMiddleware — there is no in-band sandbox_id.
type SandboxControlHandler struct {
	sandboxctrlv1connect.UnimplementedSandboxControlServiceHandler
	reg *sandboxctrl.Registry
}

// NewSandboxControlHandler returns a handler that records
// registrations and heartbeats into reg. Returns an error if reg
// is nil — the orchestrator's wiring would be malformed.
func NewSandboxControlHandler(reg *sandboxctrl.Registry) (*SandboxControlHandler, error) {
	if reg == nil {
		return nil, errors.New("orchestrator: nil sandboxctrl registry")
	}
	return &SandboxControlHandler{reg: reg}, nil
}

// RegisterSandbox records the caller's host-resource advertisement
// and holds the server-streamed channel open. The first event is
// a SubscriptionReady carrying the accepted-at timestamp; further
// events (Dispatch) land in Slice A4. The entry is released when
// the stream closes (caller disconnect, ctx cancel).
func (h *SandboxControlHandler) RegisterSandbox(
	ctx context.Context,
	req *connect.Request[sandboxctrlv1.RegisterSandboxRequest],
	stream *connect.ServerStream[sandboxctrlv1.RegisterEvent],
) error {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated,
			errors.New("orchestrator: no mTLS peer identity"))
	}

	resources := resourcesFromProto(req.Msg.GetResources())
	acceptedAt := h.reg.Register(fp, req.Msg.GetVersion(), resources)
	defer h.reg.Forget(fp)

	ready := &sandboxctrlv1.RegisterEvent{
		Event: &sandboxctrlv1.RegisterEvent_Ready{
			Ready: &sandboxctrlv1.SubscriptionReady{
				AcceptedAt: timestamppb.New(acceptedAt),
			},
		},
	}
	if err := stream.Send(ready); err != nil {
		return err
	}

	// Cancellability is required: the only termination signal for
	// this stream in A1 is ctx.Done (Slice A4 will add Dispatch
	// pushes from the orchestrator). Background context here would
	// keep the handler goroutine alive forever.
	done := ctx.Done()
	assert.True(done != nil,
		"RegisterSandbox requires a cancellable context; Connect-RPC handlers always receive one — Background here means a wiring bug")
	<-done
	return nil
}

// Heartbeat refreshes the caller's LastSeenAt and returns the
// recorded time. ErrUnknownSandbox is mapped to FailedPrecondition
// so the client can re-RegisterSandbox without retrying the
// heartbeat blindly.
func (h *SandboxControlHandler) Heartbeat(
	_ context.Context,
	req *connect.Request[sandboxctrlv1.HeartbeatRequest],
) (*connect.Response[sandboxctrlv1.HeartbeatResponse], error) {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated,
			errors.New("orchestrator: no mTLS peer identity"))
	}
	// Registry.Heartbeat returns only ErrUnknownSandbox or nil —
	// the FailedPrecondition path is the only error mapping we
	// need. Add explicit cases here when the registry grows new
	// error sentinels.
	seenAt, err := h.reg.Heartbeat(fp, resourcesFromProto(req.Msg.GetCurrent()))
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&sandboxctrlv1.HeartbeatResponse{
		SeenAt: timestamppb.New(seenAt),
	}), nil
}

// resourcesFromProto converts a (possibly nil) wire HostResources
// to the registry's native struct. nil → zero value.
func resourcesFromProto(p *sandboxctrlv1.HostResources) sandboxctrl.Resources {
	if p == nil {
		return sandboxctrl.Resources{}
	}
	return sandboxctrl.Resources{
		CPUs:        p.GetCpus(),
		MemoryBytes: p.GetMemoryBytes(),
	}
}
