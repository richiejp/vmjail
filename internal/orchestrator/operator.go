package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	operatorv1 "github.com/richiejp/darkaifactory/internal/genpb/operator/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/operator/v1/operatorv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
)

const (
	defaultOperatorPageSize = 50
	maxOperatorPageSize     = 200
	maxNodeLabelBytes       = 128
)

type OperatorHandler struct {
	operatorv1connect.UnimplementedOperatorServiceHandler
	peers *identity.PeerStore
	nodes *nodectrl.Registry
	ctrl  *NodeControlHandler
}

func NewOperatorHandler(peers *identity.PeerStore, nodes *nodectrl.Registry, ctrl *NodeControlHandler) (*OperatorHandler, error) {
	if peers == nil || nodes == nil || ctrl == nil {
		return nil, errors.New("orchestrator: operator handler requires peers, nodes, and node control")
	}
	return &OperatorHandler{peers: peers, nodes: nodes, ctrl: ctrl}, nil
}

func newControlHandlers(nodes *nodectrl.Registry, peers *identity.PeerStore) (*NodeControlHandler, *OperatorHandler, error) {
	ctrl, err := NewNodeControlHandler(nodes, peers)
	if err != nil {
		return nil, nil, err
	}
	operator, err := NewOperatorHandler(peers, nodes, ctrl)
	if err != nil {
		return nil, nil, err
	}
	return ctrl, operator, nil
}

func (h *OperatorHandler) authorize(header http.Header) error {
	_, err := requirePeerRole(header, h.peers, identity.RoleOperator)
	return err
}

func (h *OperatorHandler) AuthorizeNode(_ context.Context, req *connect.Request[operatorv1.AuthorizeNodeRequest]) (*connect.Response[operatorv1.NodeStatus], error) {
	if err := h.authorize(req.Header()); err != nil {
		return nil, err
	}
	fp, err := identity.ParseFingerprint(req.Msg.GetFingerprint())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	label, err := validNodeLabel(req.Msg.GetLabel())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if peer, ok := h.peers.Lookup(fp); ok && peer.Role != identity.RoleSandboxHost {
		return nil, connect.NewError(connect.CodeAlreadyExists,
			fmt.Errorf("orchestrator: fingerprint is already authorized as %s", peer.Role))
	}
	if err := h.peers.Add(identity.Peer{Fingerprint: fp, Label: label, Role: identity.RoleSandboxHost}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(h.status(fp)), nil
}

func (h *OperatorHandler) RevokeNode(_ context.Context, req *connect.Request[operatorv1.RevokeNodeRequest]) (*connect.Response[operatorv1.NodeStatus], error) {
	if err := h.authorize(req.Header()); err != nil {
		return nil, err
	}
	fp, err := identity.ParseFingerprint(req.Msg.GetFingerprint())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if peer, ok := h.peers.Lookup(fp); ok && peer.Role != identity.RoleSandboxHost {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("orchestrator: refusing to revoke non-node peer with role %s", peer.Role))
	}
	if err := h.peers.Remove(fp); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	h.ctrl.RevokeRegistration(fp)
	return connect.NewResponse(h.status(fp)), nil
}

func (h *OperatorHandler) GetNode(_ context.Context, req *connect.Request[operatorv1.GetNodeRequest]) (*connect.Response[operatorv1.NodeStatus], error) {
	if err := h.authorize(req.Header()); err != nil {
		return nil, err
	}
	fp, err := identity.ParseFingerprint(req.Msg.GetFingerprint())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(h.status(fp)), nil
}

func (h *OperatorHandler) ListNodes(_ context.Context, req *connect.Request[operatorv1.ListNodesRequest]) (*connect.Response[operatorv1.ListNodesResponse], error) {
	if err := h.authorize(req.Header()); err != nil {
		return nil, err
	}
	pageSize := int(req.Msg.GetPageSize())
	if pageSize == 0 {
		pageSize = defaultOperatorPageSize
	}
	if pageSize > maxOperatorPageSize {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("orchestrator: page_size exceeds 200"))
	}
	all := h.nodeFingerprints()
	start := sort.Search(len(all), func(i int) bool { return all[i].String() > req.Msg.GetPageToken() })
	end := min(start+pageSize, len(all))
	out := make([]*operatorv1.NodeStatus, 0, end-start)
	for _, fp := range all[start:end] {
		out = append(out, h.status(fp))
	}
	next := ""
	if end < len(all) {
		next = all[end-1].String()
	}
	return connect.NewResponse(&operatorv1.ListNodesResponse{Nodes: out, NextPageToken: next}), nil
}

func (h *OperatorHandler) nodeFingerprints() []identity.Fingerprint {
	set := make(map[identity.Fingerprint]struct{})
	for _, peer := range h.peers.List() {
		if peer.Role == identity.RoleSandboxHost {
			set[peer.Fingerprint] = struct{}{}
		}
	}
	nodes := h.nodes.Snapshot()
	for i := range nodes {
		set[nodes[i].Fingerprint] = struct{}{}
	}
	out := make([]identity.Fingerprint, 0, len(set))
	for fp := range set {
		out = append(out, fp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func (h *OperatorHandler) status(fp identity.Fingerprint) *operatorv1.NodeStatus {
	status := &operatorv1.NodeStatus{Fingerprint: fp.String()}
	if peer, ok := h.peers.Lookup(fp); ok && peer.Role == identity.RoleSandboxHost {
		status.Authorized = true
		status.Label = peer.Label
		status.ApprovedAt = timestamppb.New(peer.ApprovedAt)
	}
	if node, ok := h.nodes.Lookup(fp); ok {
		status.Connected = true
		status.Hostname = node.Hostname
		status.Version = node.Version
		status.Resources = &nodectrlv1.HostResources{Cpus: node.Resources.CPUs, MemoryBytes: node.Resources.MemoryBytes}
		status.RegisteredAt = timestamppb.New(node.RegisteredAt)
		status.LastSeenAt = timestamppb.New(node.LastSeenAt)
	}
	return status
}

func validNodeLabel(raw string) (string, error) {
	label := strings.TrimSpace(raw)
	if len(label) > maxNodeLabelBytes || !utf8.ValidString(label) {
		return "", errors.New("orchestrator: node label must be valid UTF-8 and at most 128 bytes")
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return "", errors.New("orchestrator: node label contains a control character")
		}
	}
	return label, nil
}
