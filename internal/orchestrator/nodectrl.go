package orchestrator

import (
	"context"
	"errors"
	"sync"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/richiejp/darkaifactory/internal/assert"
	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1/nodectrlv1connect"
	sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
)

// sandboxEventBacklog is the per-stream SandboxWatchEvent buffer
// depth. Bounded so a stalled Node can't grow the orchestrator's
// memory; a caller that overflows the buffer gets
// ErrSandboxEventBacklog and must retry or pick another Node.
const sandboxEventBacklog = 16

// ErrNodeNotConnected is returned by PublishSandboxEvent when the
// addressed fingerprint has no open RegisterNode stream. The caller
// must pick a different Node or wait for the original to reconnect.
//
// Aliased to ErrSandboxNotConnected for the duration of the B4
// transition so callers can rename in their own time.
var ErrNodeNotConnected = errors.New("orchestrator: node not connected")

// ErrSandboxNotConnected is the previous name of ErrNodeNotConnected.
// Retained as an alias so call sites that haven't been swept yet
// keep compiling. Remove in a future cleanup slice.
var ErrSandboxNotConnected = ErrNodeNotConnected

// ErrSandboxEventBacklog is returned by PublishSandboxEvent when
// the Node's stream send buffer is full — the sandboxd is alive but
// slower than the publish rate. Callers should treat this like a
// transient failure.
var ErrSandboxEventBacklog = errors.New("orchestrator: sandbox-event backlog full")

// ErrDispatchBacklog is the previous name; alias retained.
var ErrDispatchBacklog = ErrSandboxEventBacklog

// NodeControlHandler adapts the nodectrl.Registry to the
// Connect-RPC NodeControlService surface. Peer identity for
// every RPC is the mTLS leaf-cert fingerprint surfaced via
// withPeerMiddleware — there is no in-band sandbox_id.
type NodeControlHandler struct {
	nodectrlv1connect.UnimplementedNodeControlServiceHandler
	reg *nodectrl.Registry

	mu      sync.Mutex
	streams map[identity.Fingerprint]chan *nodectrlv1.NodeEvent
}

// NewSandboxControlHandler returns a handler that records
// registrations and heartbeats into reg. Returns an error if reg
// is nil — the orchestrator's wiring would be malformed.
func NewSandboxControlHandler(reg *nodectrl.Registry) (*NodeControlHandler, error) {
	if reg == nil {
		return nil, errors.New("orchestrator: nil nodectrl registry")
	}
	return &NodeControlHandler{
		reg:     reg,
		streams: map[identity.Fingerprint]chan *nodectrlv1.NodeEvent{},
	}, nil
}

// PublishSandboxEvent pushes a SandboxWatchEvent to the addressed
// Node's live RegisterNode stream. Returns ErrNodeNotConnected if
// the Node has no open stream; ErrSandboxEventBacklog if the
// per-stream buffer is full.
func (h *NodeControlHandler) PublishSandboxEvent(
	fp identity.Fingerprint,
	eventType sandboxv1.EventType,
	sandbox *sandboxv1.Sandbox,
) error {
	assert.True(sandbox != nil,
		"PublishSandboxEvent requires a non-nil Sandbox; a wiring bug here would push a nil envelope onto the stream and crash the sandboxd")
	uid := sandbox.GetMetadata().GetUid()
	assert.True(uid != "",
		"PublishSandboxEvent requires a non-empty Sandbox.metadata.uid; the orchestrator scheduler is the sole caller and must mint one before calling")
	h.mu.Lock()
	ch, ok := h.streams[fp]
	h.mu.Unlock()
	if !ok {
		return ErrNodeNotConnected
	}
	ev := &nodectrlv1.NodeEvent{
		Event: &nodectrlv1.NodeEvent_Sandbox{
			Sandbox: &nodectrlv1.SandboxWatchEvent{
				Type:    eventType,
				Sandbox: sandbox,
			},
		},
	}
	select {
	case ch <- ev:
		return nil
	default:
		return ErrSandboxEventBacklog
	}
}

func (h *NodeControlHandler) attachStream(fp identity.Fingerprint) chan *nodectrlv1.NodeEvent {
	ch := make(chan *nodectrlv1.NodeEvent, sandboxEventBacklog)
	h.mu.Lock()
	h.streams[fp] = ch
	h.mu.Unlock()
	return ch
}

func (h *NodeControlHandler) detachStream(fp identity.Fingerprint) {
	h.mu.Lock()
	delete(h.streams, fp)
	h.mu.Unlock()
}

// RegisterNode records the caller's host-resource advertisement
// and holds the server-streamed channel open. The first event is
// a SubscriptionReady carrying the accepted-at timestamp; further
// events (Dispatch) land in Slice A4. The entry is released when
// the stream closes (caller disconnect, ctx cancel).
func (h *NodeControlHandler) RegisterNode(
	ctx context.Context,
	req *connect.Request[nodectrlv1.RegisterNodeRequest],
	stream *connect.ServerStream[nodectrlv1.NodeEvent],
) error {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated,
			errors.New("orchestrator: no mTLS peer identity"))
	}

	// Wire-protocol negotiation runs before any state mutation so a
	// wire-incompatible peer never appears in the live registry.
	// Missing/zero ProtocolVersion is treated as major=0 and refused
	// — every shipped client sets the field.
	pv := req.Msg.GetProtocolVersion()
	if err := nodectrl.CheckProtocolCompatibility(
		pv.GetMajor(), pv.GetMinor(), pv.GetPatch(),
	); err != nil {
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}

	resources := resourcesFromProto(req.Msg.GetResources())
	acceptedAt := h.reg.Register(fp, req.Msg.GetVersion(), resources)
	defer h.reg.Forget(fp)

	events := h.attachStream(fp)
	defer h.detachStream(fp)

	ready := &nodectrlv1.NodeEvent{
		Event: &nodectrlv1.NodeEvent_Ready{
			Ready: &nodectrlv1.SubscriptionReady{
				AcceptedAt:      timestamppb.New(acceptedAt),
				ProtocolVersion: localProtocolVersionProto(),
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
		"RegisterNode requires a cancellable context; Connect-RPC handlers always receive one — Background here means a wiring bug")
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
// recorded time. ErrUnknownNode is mapped to FailedPrecondition
// so the client can re-RegisterNode without retrying the
// heartbeat blindly.
func (h *NodeControlHandler) Heartbeat(
	_ context.Context,
	req *connect.Request[nodectrlv1.HeartbeatRequest],
) (*connect.Response[nodectrlv1.HeartbeatResponse], error) {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated,
			errors.New("orchestrator: no mTLS peer identity"))
	}
	// Registry.Heartbeat returns only ErrUnknownNode or nil —
	// the FailedPrecondition path is the only error mapping we
	// need. Add explicit cases here when the registry grows new
	// error sentinels.
	seenAt, err := h.reg.Heartbeat(fp, resourcesFromProto(req.Msg.GetCurrent()))
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&nodectrlv1.HeartbeatResponse{
		SeenAt: timestamppb.New(seenAt),
	}), nil
}

// ListNodes returns a snapshot of every live registration in
// the orchestrator's registry. Read-only; the only side-effect is
// the peer-identity check shared with the other RPCs on this
// service.
func (h *NodeControlHandler) ListNodes(
	_ context.Context,
	req *connect.Request[nodectrlv1.ListNodesRequest],
) (*connect.Response[nodectrlv1.ListNodesResponse], error) {
	if _, ok := PeerFingerprintFromRequestHeader(req.Header()); !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated,
			errors.New("orchestrator: no mTLS peer identity"))
	}
	entries := h.reg.Snapshot()
	out := make([]*nodectrlv1.Node, len(entries))
	for i := range entries {
		out[i] = entryToProto(entries[i])
	}
	return connect.NewResponse(&nodectrlv1.ListNodesResponse{
		Nodes: out,
	}), nil
}

// resourcesFromProto converts a (possibly nil) wire HostResources
// to the registry's native struct. nil → zero value.
func resourcesFromProto(p *nodectrlv1.HostResources) nodectrl.Resources {
	if p == nil {
		return nodectrl.Resources{}
	}
	return nodectrl.Resources{
		CPUs:        p.GetCpus(),
		MemoryBytes: p.GetMemoryBytes(),
	}
}

// localProtocolVersionProto builds the wire ProtocolVersion message
// from this build's compiled-in constants.
func localProtocolVersionProto() *nodectrlv1.ProtocolVersion {
	return &nodectrlv1.ProtocolVersion{
		Major: nodectrl.ProtocolVersionMajor,
		Minor: nodectrl.ProtocolVersionMinor,
		Patch: nodectrl.ProtocolVersionPatch,
	}
}

// entryToProto projects a registry Entry onto its wire form.
func entryToProto(e nodectrl.Entry) *nodectrlv1.Node {
	return &nodectrlv1.Node{
		Fingerprint: e.Fingerprint.String(),
		Version:     e.Version,
		Resources: &nodectrlv1.HostResources{
			Cpus:        e.Resources.CPUs,
			MemoryBytes: e.Resources.MemoryBytes,
		},
		RegisteredAt: timestamppb.New(e.RegisteredAt),
		LastSeenAt:   timestamppb.New(e.LastSeenAt),
	}
}
