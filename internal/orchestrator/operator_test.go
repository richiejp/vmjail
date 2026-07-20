package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	operatorv1 "github.com/richiejp/darkaifactory/internal/genpb/operator/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/operator/v1/operatorv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
)

func newOperatorFixture(t *testing.T) (*OperatorHandler, *identity.PeerStore, *nodectrl.Registry, identity.Fingerprint) {
	t.Helper()
	peers, err := identity.OpenPeerStore(t.TempDir() + "/peers.json")
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	operatorID, _ := identity.Generate(identity.RoleOperator)
	operatorFP := operatorID.Fingerprint()
	if err := peers.Add(identity.Peer{Fingerprint: operatorFP, Role: identity.RoleOperator}); err != nil {
		t.Fatalf("add operator: %v", err)
	}
	nodes := nodectrl.NewRegistry(func() time.Time { return time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC) })
	ctrl, _ := NewNodeControlHandler(nodes, peers)
	h, err := NewOperatorHandler(peers, nodes, ctrl)
	if err != nil {
		t.Fatalf("NewOperatorHandler: %v", err)
	}
	return h, peers, nodes, operatorFP
}

func operatorRequest[T any](msg *T, fp identity.Fingerprint) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set(peerFingerprintHeader, fp.String())
	return req
}

func TestOperatorNodeLifecycle(t *testing.T) {
	h, peers, nodes, operatorFP := newOperatorFixture(t)
	nodeID, _ := identity.Generate(identity.RoleSandboxHost)
	nodeFP := nodeID.Fingerprint()

	authorized, err := h.AuthorizeNode(context.Background(), operatorRequest(&operatorv1.AuthorizeNodeRequest{
		Fingerprint: nodeFP.String(), Label: "  build-node  ",
	}, operatorFP))
	if err != nil {
		t.Fatalf("AuthorizeNode: %v", err)
	}
	if !authorized.Msg.GetAuthorized() || authorized.Msg.GetConnected() || authorized.Msg.GetLabel() != "build-node" {
		t.Fatalf("authorized status = %+v", authorized.Msg)
	}
	firstApproved := authorized.Msg.GetApprovedAt().AsTime()
	if _, err := h.AuthorizeNode(context.Background(), operatorRequest(&operatorv1.AuthorizeNodeRequest{
		Fingerprint: nodeFP.String(), Label: "renamed",
	}, operatorFP)); err != nil {
		t.Fatalf("idempotent AuthorizeNode: %v", err)
	}
	peer, _ := peers.Lookup(nodeFP)
	if !peer.ApprovedAt.Equal(firstApproved) || peer.Label != "renamed" {
		t.Fatalf("reauthorized peer = %+v", peer)
	}

	nodes.Register(nodeFP, "builder", "v1", nodectrl.Resources{CPUs: 8, MemoryBytes: 1024})
	got, err := h.GetNode(context.Background(), operatorRequest(&operatorv1.GetNodeRequest{Fingerprint: nodeFP.String()}, operatorFP))
	if err != nil || !got.Msg.GetConnected() || got.Msg.GetResources().GetCpus() != 8 {
		t.Fatalf("GetNode = %+v, %v", got, err)
	}

	revoked, err := h.RevokeNode(context.Background(), operatorRequest(&operatorv1.RevokeNodeRequest{Fingerprint: nodeFP.String()}, operatorFP))
	if err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}
	if revoked.Msg.GetAuthorized() || revoked.Msg.GetConnected() {
		t.Fatalf("revoked status = %+v", revoked.Msg)
	}
	if _, ok := nodes.Lookup(nodeFP); ok {
		t.Fatal("revocation left live node registered")
	}
}

func TestOperatorAuthorizationAndValidation(t *testing.T) {
	const malformedFingerprint = "bad"
	h, peers, _, operatorFP := newOperatorFixture(t)
	nodeID, _ := identity.Generate(identity.RoleSandboxHost)
	nodeFP := nodeID.Fingerprint()
	clientID, _ := identity.Generate(identity.RoleClient)
	if err := peers.Add(identity.Peer{Fingerprint: clientID.Fingerprint(), Role: identity.RoleClient}); err != nil {
		t.Fatalf("add client: %v", err)
	}

	if _, err := h.GetNode(context.Background(), connect.NewRequest(&operatorv1.GetNodeRequest{Fingerprint: nodeFP.String()})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("missing identity code = %v", connect.CodeOf(err))
	}
	if _, err := h.AuthorizeNode(context.Background(), connect.NewRequest(&operatorv1.AuthorizeNodeRequest{Fingerprint: nodeFP.String()})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("missing authorize identity code = %v", connect.CodeOf(err))
	}
	if _, err := h.RevokeNode(context.Background(), connect.NewRequest(&operatorv1.RevokeNodeRequest{Fingerprint: nodeFP.String()})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("missing revoke identity code = %v", connect.CodeOf(err))
	}
	if _, err := h.ListNodes(context.Background(), connect.NewRequest(&operatorv1.ListNodesRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("missing list identity code = %v", connect.CodeOf(err))
	}
	if _, err := h.GetNode(context.Background(), operatorRequest(&operatorv1.GetNodeRequest{Fingerprint: nodeFP.String()}, clientID.Fingerprint())); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("client identity code = %v", connect.CodeOf(err))
	}
	if _, err := h.AuthorizeNode(context.Background(), operatorRequest(&operatorv1.AuthorizeNodeRequest{Fingerprint: malformedFingerprint}, operatorFP)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad fingerprint code = %v", connect.CodeOf(err))
	}
	if _, err := h.AuthorizeNode(context.Background(), operatorRequest(&operatorv1.AuthorizeNodeRequest{Fingerprint: nodeFP.String(), Label: "bad\nlabel"}, operatorFP)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad label code = %v", connect.CodeOf(err))
	}
	if _, err := h.AuthorizeNode(context.Background(), operatorRequest(&operatorv1.AuthorizeNodeRequest{Fingerprint: clientID.Fingerprint().String()}, operatorFP)); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Fatalf("role conflict code = %v", connect.CodeOf(err))
	}
	if _, err := h.RevokeNode(context.Background(), operatorRequest(&operatorv1.RevokeNodeRequest{Fingerprint: malformedFingerprint}, operatorFP)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad revoke fingerprint code = %v", connect.CodeOf(err))
	}
	if _, err := h.RevokeNode(context.Background(), operatorRequest(&operatorv1.RevokeNodeRequest{Fingerprint: clientID.Fingerprint().String()}, operatorFP)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("non-node revoke code = %v", connect.CodeOf(err))
	}
	if _, err := h.GetNode(context.Background(), operatorRequest(&operatorv1.GetNodeRequest{Fingerprint: malformedFingerprint}, operatorFP)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad get fingerprint code = %v", connect.CodeOf(err))
	}
	if _, err := h.ListNodes(context.Background(), operatorRequest(&operatorv1.ListNodesRequest{}, operatorFP)); err != nil {
		t.Fatalf("default page size: %v", err)
	}
	if _, err := validNodeLabel(string([]byte{0xff})); err == nil {
		t.Fatal("invalid UTF-8 label accepted")
	}
	if _, err := validNodeLabel(string(make([]byte, maxNodeLabelBytes+1))); err == nil {
		t.Fatal("oversize label accepted")
	}
}

func TestOperatorPersistenceErrorsDoNotBecomeSuccess(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "peers.json")
	peers, _ := identity.OpenPeerStore(path)
	operatorID, _ := identity.Generate(identity.RoleOperator)
	if err := peers.Add(identity.Peer{Fingerprint: operatorID.Fingerprint(), Role: identity.RoleOperator}); err != nil {
		t.Fatalf("add operator: %v", err)
	}
	nodeID, _ := identity.Generate(identity.RoleSandboxHost)
	if err := peers.Add(identity.Peer{Fingerprint: nodeID.Fingerprint(), Role: identity.RoleSandboxHost}); err != nil {
		t.Fatalf("add node: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove store: %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("block store: %v", err)
	}
	nodes := nodectrl.NewRegistry(nil)
	ctrl, _ := NewNodeControlHandler(nodes, peers)
	h, _ := NewOperatorHandler(peers, nodes, ctrl)
	otherID, _ := identity.Generate(identity.RoleSandboxHost)
	if _, err := h.AuthorizeNode(context.Background(), operatorRequest(&operatorv1.AuthorizeNodeRequest{Fingerprint: otherID.Fingerprint().String()}, operatorID.Fingerprint())); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("failed authorize code = %v", connect.CodeOf(err))
	}
	if _, err := h.RevokeNode(context.Background(), operatorRequest(&operatorv1.RevokeNodeRequest{Fingerprint: nodeID.Fingerprint().String()}, operatorID.Fingerprint())); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("failed revoke code = %v", connect.CodeOf(err))
	}
}

func TestOperatorListNodesPaginatesAuthorizationUnion(t *testing.T) {
	h, peers, nodes, operatorFP := newOperatorFixture(t)
	for i := 0; i < 3; i++ {
		id, _ := identity.Generate(identity.RoleSandboxHost)
		if i < 2 {
			if err := peers.Add(identity.Peer{Fingerprint: id.Fingerprint(), Role: identity.RoleSandboxHost}); err != nil {
				t.Fatalf("add node: %v", err)
			}
		}
		if i > 0 {
			nodes.Register(id.Fingerprint(), "node", "v1", nodectrl.Resources{})
		}
	}
	first, err := h.ListNodes(context.Background(), operatorRequest(&operatorv1.ListNodesRequest{PageSize: 2}, operatorFP))
	if err != nil || len(first.Msg.GetNodes()) != 2 || first.Msg.GetNextPageToken() == "" {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	second, err := h.ListNodes(context.Background(), operatorRequest(&operatorv1.ListNodesRequest{PageSize: 2, PageToken: first.Msg.GetNextPageToken()}, operatorFP))
	if err != nil || len(second.Msg.GetNodes()) != 1 || second.Msg.GetNextPageToken() != "" {
		t.Fatalf("second page = %+v, %v", second, err)
	}
	if _, err := h.ListNodes(context.Background(), operatorRequest(&operatorv1.ListNodesRequest{PageSize: 201}, operatorFP)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("oversize page code = %v", connect.CodeOf(err))
	}
}

func TestNewOperatorHandlerRejectsMissingDependencies(t *testing.T) {
	if _, err := NewOperatorHandler(nil, nil, nil); err == nil {
		t.Fatal("expected missing dependency error")
	}
	if _, _, err := newControlHandlers(nil, identity.NewMemoryPeerStore()); err == nil {
		t.Fatal("newControlHandlers accepted nil registry")
	}
	if _, _, err := newControlHandlers(nodectrl.NewRegistry(nil), nil); err == nil {
		t.Fatal("newControlHandlers accepted nil peers")
	}
}

func TestOperatorServiceEnforcesStoredRoleOverMTLS(t *testing.T) {
	operatorEnv := newOrchTestEnv(t, identity.RoleOperator, Config{})
	operatorClient := operatorv1connect.NewOperatorServiceClient(operatorEnv.httpc, operatorEnv.baseURL)
	id, _ := identity.Generate(identity.RoleSandboxHost)
	if _, err := operatorClient.GetNode(t.Context(), connect.NewRequest(&operatorv1.GetNodeRequest{Fingerprint: id.Fingerprint().String()})); err != nil {
		t.Fatalf("operator GetNode: %v", err)
	}

	clientEnv := newOrchTestEnv(t, identity.RoleClient, Config{})
	client := operatorv1connect.NewOperatorServiceClient(clientEnv.httpc, clientEnv.baseURL)
	if _, err := client.GetNode(t.Context(), connect.NewRequest(&operatorv1.GetNodeRequest{Fingerprint: id.Fingerprint().String()})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("client GetNode code = %v, want permission denied", connect.CodeOf(err))
	}
}
