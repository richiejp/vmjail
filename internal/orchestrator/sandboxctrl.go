package orchestrator

import (
	"context"
	"errors"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/richiejp/darkaifactory/internal/assert"
	sandboxctrlv1 "github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1/sandboxctrlv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/sandboxctrl"
)

// dispatchBacklog is the per-stream Dispatch buffer depth. Bounded
// so a stalled sandbox can't grow the orchestrator's memory; a
// caller that overflows the buffer gets ErrDispatchBacklog and must
// retry or pick another sandbox.
const dispatchBacklog = 16

// ErrSandboxNotConnected is returned by Dispatch when the addressed
// fingerprint has no open RegisterSandbox stream. The caller must
// pick a different sandbox or wait for the original to reconnect.
var ErrSandboxNotConnected = errors.New("orchestrator: sandbox not connected")

// ErrDispatchBacklog is returned by Dispatch when the sandbox's
// stream send buffer is full — the sandboxd is alive but slower
// than the dispatch rate. The orchestrator scheduler should treat
// this like a transient failure.
var ErrDispatchBacklog = errors.New("orchestrator: dispatch backlog full")

// SandboxControlHandler adapts the sandboxctrl.Registry to the
// Connect-RPC SandboxControlService surface. Peer identity for
// every RPC is the mTLS leaf-cert fingerprint surfaced via
// withPeerMiddleware — there is no in-band sandbox_id.
type SandboxControlHandler struct {
	sandboxctrlv1connect.UnimplementedSandboxControlServiceHandler
	reg *sandboxctrl.Registry

	mu      sync.Mutex
	streams map[identity.Fingerprint]chan *sandboxctrlv1.RegisterEvent
}

// NewSandboxControlHandler returns a handler that records
// registrations and heartbeats into reg. Returns an error if reg
// is nil — the orchestrator's wiring would be malformed.
func NewSandboxControlHandler(reg *sandboxctrl.Registry) (*SandboxControlHandler, error) {
	if reg == nil {
		return nil, errors.New("orchestrator: nil sandboxctrl registry")
	}
	return &SandboxControlHandler{
		reg:     reg,
		streams: map[identity.Fingerprint]chan *sandboxctrlv1.RegisterEvent{},
	}, nil
}

// Dispatch pushes a Dispatch event to the addressed sandbox's
// live RegisterSandbox stream. Returns ErrSandboxNotConnected if
// the sandbox has no open stream; ErrDispatchBacklog if the
// per-stream buffer is full.
func (h *SandboxControlHandler) Dispatch(
	fp identity.Fingerprint,
	dispatchID string,
	spec *sandboxctrlv1.SandboxSpec,
) error {
	h.mu.Lock()
	ch, ok := h.streams[fp]
	h.mu.Unlock()
	if !ok {
		return ErrSandboxNotConnected
	}
	ev := &sandboxctrlv1.RegisterEvent{
		Event: &sandboxctrlv1.RegisterEvent_Dispatch{
			Dispatch: &sandboxctrlv1.Dispatch{
				DispatchId: dispatchID,
				Spec:       spec,
			},
		},
	}
	select {
	case ch <- ev:
		return nil
	default:
		return ErrDispatchBacklog
	}
}

func (h *SandboxControlHandler) attachStream(fp identity.Fingerprint) chan *sandboxctrlv1.RegisterEvent {
	ch := make(chan *sandboxctrlv1.RegisterEvent, dispatchBacklog)
	h.mu.Lock()
	h.streams[fp] = ch
	h.mu.Unlock()
	return ch
}

func (h *SandboxControlHandler) detachStream(fp identity.Fingerprint) {
	h.mu.Lock()
	delete(h.streams, fp)
	h.mu.Unlock()
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

	events := h.attachStream(fp)
	defer h.detachStream(fp)

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

	// Stream loop: forward Dispatch events from the per-stream
	// channel until ctx fires. stream.Send is not safe for
	// concurrent use, so this single goroutine is the only writer.
	done := ctx.Done()
	assert.True(done != nil,
		"RegisterSandbox requires a cancellable context; Connect-RPC handlers always receive one — Background here means a wiring bug")
	//daft:lint:disable=forloopbound reason="exits on ctx.Done or stream.Send error"
	for {
		select {
		case <-done:
			return nil
		case ev := <-events:
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
	}
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

// ListSandboxes returns a snapshot of every live registration in
// the orchestrator's registry. Read-only; the only side-effect is
// the peer-identity check shared with the other RPCs on this
// service.
func (h *SandboxControlHandler) ListSandboxes(
	_ context.Context,
	req *connect.Request[sandboxctrlv1.ListSandboxesRequest],
) (*connect.Response[sandboxctrlv1.ListSandboxesResponse], error) {
	if _, ok := PeerFingerprintFromRequestHeader(req.Header()); !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated,
			errors.New("orchestrator: no mTLS peer identity"))
	}
	entries := h.reg.Snapshot()
	out := make([]*sandboxctrlv1.SandboxRegistration, len(entries))
	for i := range entries {
		out[i] = entryToProto(entries[i])
	}
	return connect.NewResponse(&sandboxctrlv1.ListSandboxesResponse{
		Sandboxes: out,
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

// entryToProto projects a registry Entry onto its wire form.
func entryToProto(e sandboxctrl.Entry) *sandboxctrlv1.SandboxRegistration {
	return &sandboxctrlv1.SandboxRegistration{
		Fingerprint: e.Fingerprint.String(),
		Version:     e.Version,
		Resources: &sandboxctrlv1.HostResources{
			Cpus:        e.Resources.CPUs,
			MemoryBytes: e.Resources.MemoryBytes,
		},
		RegisteredAt: timestamppb.New(e.RegisteredAt),
		LastSeenAt:   timestamppb.New(e.LastSeenAt),
	}
}
