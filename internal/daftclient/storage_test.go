package daftclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/clientconfig"
	sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/workspacesync"
)

type storageCatalogHandler struct {
	workspacev1connect.UnimplementedWorkspaceCatalogServiceHandler
	resource    *workspacev1.Workspace
	deleteCalls int
	getErr      error
	deleteErr   error
}

func (h *storageCatalogHandler) Get(
	_ context.Context,
	_ *connect.Request[workspacev1.GetWorkspaceRequest],
) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	if h.getErr != nil {
		return nil, h.getErr
	}
	return connect.NewResponse(&workspacev1.WorkspaceResponse{Workspace: h.resource}), nil
}

func (h *storageCatalogHandler) Delete(
	_ context.Context,
	req *connect.Request[workspacev1.DeleteWorkspaceRequest],
) (*connect.Response[workspacev1.DeleteWorkspaceResponse], error) {
	if h.deleteErr != nil {
		return nil, h.deleteErr
	}
	if req.Msg.GetExpectedGeneration() != h.resource.GetMetadata().GetGeneration() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("stale"))
	}
	h.deleteCalls++
	return connect.NewResponse(&workspacev1.DeleteWorkspaceResponse{}), nil
}

type storageNodeHandler struct {
	workspacev1connect.UnimplementedWorkspaceServiceHandler
	manifests     map[string]*workspacev1.GetManifestResponse
	rejectCalls   int
	unfreezeCalls int
	deleteCalls   int
	manifestErr   error
	rejectErr     error
	diskErr       error
	storageErr    error
	unfreezeErr   error
	deleteErr     error
}

func (h *storageNodeHandler) GetManifest(
	_ context.Context,
	req *connect.Request[workspacev1.GetManifestRequest],
) (*connect.Response[workspacev1.GetManifestResponse], error) {
	if h.manifestErr != nil {
		return nil, h.manifestErr
	}
	manifest, ok := h.manifests[req.Msg.GetWorkspace()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("missing"))
	}
	return connect.NewResponse(manifest), nil
}

func (h *storageNodeHandler) RejectManifest(
	_ context.Context,
	req *connect.Request[workspacev1.RejectManifestRequest],
) (*connect.Response[workspacev1.ManifestControlResponse], error) {
	if h.rejectErr != nil {
		return nil, h.rejectErr
	}
	h.rejectCalls++
	return connect.NewResponse(&workspacev1.ManifestControlResponse{
		ManifestDigest: "restored-" + req.Msg.GetManifestDigest(), Generation: req.Msg.GetGeneration() - 1,
		CurrentManifestPresent: true,
	}), nil
}

func (h *storageNodeHandler) GetWorkspaceDiskStatus(
	context.Context,
	*connect.Request[workspacev1.GetWorkspaceDiskStatusRequest],
) (*connect.Response[workspacev1.GetWorkspaceDiskStatusResponse], error) {
	if h.diskErr != nil {
		return nil, h.diskErr
	}
	return connect.NewResponse(&workspacev1.GetWorkspaceDiskStatusResponse{
		UsedBytes: 7, AvailableBytes: 13, AllowanceBytes: 20, Frozen: true, Running: true,
		ProtectedBytes: 11, CurrentManifests: []*workspacev1.ManifestExpectation{{
			Workspace: "workspace\x00/work", ManifestDigest: "sandbox", Generation: 2,
		}},
	}), nil
}

func (h *storageNodeHandler) GetStorageStatus(
	context.Context,
	*connect.Request[workspacev1.GetStorageStatusRequest],
) (*connect.Response[workspacev1.GetStorageStatusResponse], error) {
	if h.storageErr != nil {
		return nil, h.storageErr
	}
	return connect.NewResponse(&workspacev1.GetStorageStatusResponse{
		UsageBytes: 80, ProtectedBytes: 30, RecoverableBytes: 40, QueuedBytes: 10,
		ChunkCount: 8, QueuedCount: 1, HardBytes: 100, SoftBytes: 85, TargetBytes: 75, ExpirySeconds: 180,
	}), nil
}

func (h *storageNodeHandler) UnfreezeWorkspaceDisk(
	_ context.Context,
	req *connect.Request[workspacev1.UnfreezeWorkspaceDiskRequest],
) (*connect.Response[workspacev1.GetWorkspaceDiskStatusResponse], error) {
	if h.unfreezeErr != nil {
		return nil, h.unfreezeErr
	}
	h.unfreezeCalls++
	if len(req.Msg.GetExpectedManifests()) != 1 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("expectations"))
	}
	return connect.NewResponse(&workspacev1.GetWorkspaceDiskStatusResponse{
		UsedBytes: 7, AvailableBytes: 13, AllowanceBytes: 20, Running: true,
	}), nil
}

func (h *storageNodeHandler) DeleteWorkspaceData(
	context.Context,
	*connect.Request[workspacev1.DeleteWorkspaceDataRequest],
) (*connect.Response[workspacev1.DeleteWorkspaceDataResponse], error) {
	if h.deleteErr != nil {
		return nil, h.deleteErr
	}
	h.deleteCalls++
	return connect.NewResponse(&workspacev1.DeleteWorkspaceDataResponse{}), nil
}

type storageClientRig struct {
	client  *Client
	catalog *storageCatalogHandler
	node    *storageNodeHandler
}

func newStorageClientRig(t *testing.T) *storageClientRig {
	t.Helper()
	nodeIdentity, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatal(err)
	}
	const uuid = "00000000-0000-4000-8000-000000000001"
	catalogHandler := &storageCatalogHandler{resource: &workspacev1.Workspace{
		Metadata: &workspacev1.WorkspaceMetadata{Uuid: uuid, Generation: 4},
		Spec: &workspacev1.WorkspaceSpec{
			TargetSandboxFingerprint: nodeIdentity.Fingerprint().String(),
			Mounts:                   []*sandboxv1.Mount{{Target: "/work"}, {Target: "/readonly", ReadOnly: true}, {Target: "/empty"}},
		},
	}}
	catalogMux := http.NewServeMux()
	catalogPath, catalogHTTP := workspacev1connect.NewWorkspaceCatalogServiceHandler(catalogHandler)
	catalogMux.Handle(catalogPath, catalogHTTP)
	catalogServer := httptest.NewServer(catalogMux)
	t.Cleanup(catalogServer.Close)

	nodeHandler := &storageNodeHandler{manifests: map[string]*workspacev1.GetManifestResponse{
		uuid + "\x00/work": {ManifestDigest: "sandbox", Generation: 2},
	}}
	nodeMux := http.NewServeMux()
	nodePath, nodeHTTP := workspacev1connect.NewWorkspaceServiceHandler(nodeHandler)
	nodeMux.Handle(nodePath, nodeHTTP)
	nodeServer := httptest.NewServer(nodeMux)
	t.Cleanup(nodeServer.Close)
	nodeClient := workspacev1connect.NewWorkspaceServiceClient(nodeServer.Client(), nodeServer.URL)

	savedDial := dialWorkspaceService
	t.Cleanup(func() { dialWorkspaceService = savedDial })
	dialWorkspaceService = func(
		context.Context,
		workspacesync.PushConfig,
		string,
		identity.Fingerprint,
	) (workspacev1connect.WorkspaceServiceClient, func() error, error) {
		return nodeClient, func() error { return nil }, nil
	}
	clientIdentity, err := identity.Generate(identity.RoleClient)
	if err != nil {
		t.Fatal(err)
	}
	return &storageClientRig{
		client: &Client{
			Resolved:  clientconfig.Resolved{OrchestratorURL: "https://orchestrator.invalid"},
			Workspace: workspacev1connect.NewWorkspaceCatalogServiceClient(catalogServer.Client(), catalogServer.URL),
			identity:  clientIdentity, peers: identity.NewMemoryPeerStore(),
		},
		catalog: catalogHandler, node: nodeHandler,
	}
}

func TestWorkspaceStorageControlsUseBoundedExactNodeOperations(t *testing.T) {
	rig := newStorageClientRig(t)
	const uuid = "00000000-0000-4000-8000-000000000001"
	status, err := rig.client.WorkspaceStorageStatus(t.Context(), uuid)
	if err != nil || status.DiskUsedBytes != 7 || status.WorkspaceProtectedBytes != 11 || len(status.Manifests) != 1 || status.ProtectedBytes != 30 || status.HardBytes != 100 || !status.DiskFrozen {
		t.Fatalf("WorkspaceStorageStatus = %#v, %v", status, err)
	}
	transitions, err := rig.client.RejectWorkspaceChanges(t.Context(), uuid)
	if err != nil || len(transitions) != 1 || transitions[0].Digest != "restored-sandbox" || rig.node.rejectCalls != 1 {
		t.Fatalf("RejectWorkspaceChanges = %#v, %v, calls %d", transitions, err, rig.node.rejectCalls)
	}
	unfrozen, err := rig.client.UnfreezeWorkspace(t.Context(), uuid)
	if err != nil || unfrozen.DiskFrozen || !unfrozen.DiskRunning || rig.node.unfreezeCalls != 1 {
		t.Fatalf("UnfreezeWorkspace = %#v, %v, calls %d", unfrozen, err, rig.node.unfreezeCalls)
	}
	if err := rig.client.DeleteWorkspace(t.Context(), uuid); err != nil || rig.node.deleteCalls != 1 || rig.catalog.deleteCalls != 1 {
		t.Fatalf("DeleteWorkspace = %v, node %d, catalog %d", err, rig.node.deleteCalls, rig.catalog.deleteCalls)
	}
}

func TestDeleteWorkspaceWithoutTargetSkipsNode(t *testing.T) {
	rig := newStorageClientRig(t)
	rig.catalog.resource.Spec.TargetSandboxFingerprint = ""
	if err := rig.client.DeleteWorkspace(t.Context(), rig.catalog.resource.GetMetadata().GetUuid()); err != nil {
		t.Fatal(err)
	}
	if rig.node.deleteCalls != 0 || rig.catalog.deleteCalls != 1 {
		t.Fatalf("delete calls node=%d catalog=%d", rig.node.deleteCalls, rig.catalog.deleteCalls)
	}
}

func TestWorkspaceStorageControlsValidateClientAndTarget(t *testing.T) {
	var nilClient *Client
	if _, err := nilClient.WorkspaceStorageStatus(t.Context(), "workspace"); err == nil {
		t.Fatal("nil client status succeeded")
	}
	rig := newStorageClientRig(t)
	if _, err := rig.client.WorkspaceStorageStatus(t.Context(), ""); err == nil {
		t.Fatal("empty UUID status succeeded")
	}
	rig.catalog.resource.Spec.TargetSandboxFingerprint = "invalid"
	if _, err := rig.client.WorkspaceStorageStatus(t.Context(), rig.catalog.resource.GetMetadata().GetUuid()); err == nil {
		t.Fatal("invalid target status succeeded")
	}
}

func TestWorkspaceStorageControlsPropagateNodeAndCatalogFailures(t *testing.T) {
	rig := newStorageClientRig(t)
	uuid := rig.catalog.resource.GetMetadata().GetUuid()
	want := connect.NewError(connect.CodeUnavailable, errors.New("unavailable"))
	rig.node.diskErr = want
	if _, err := rig.client.WorkspaceStorageStatus(t.Context(), uuid); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("disk status error = %v", err)
	}
	rig.node.diskErr = nil
	rig.node.storageErr = want
	if _, err := rig.client.WorkspaceStorageStatus(t.Context(), uuid); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("CAS status error = %v", err)
	}
	rig.node.storageErr = nil
	rig.node.manifestErr = want
	if _, err := rig.client.RejectWorkspaceChanges(t.Context(), uuid); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("manifest inspection error = %v", err)
	}
	if _, err := rig.client.UnfreezeWorkspace(t.Context(), uuid); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("unfreeze inspection error = %v", err)
	}
	if err := rig.client.DeleteWorkspace(t.Context(), uuid); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("delete inspection error = %v", err)
	}
	rig.node.manifestErr = nil
	rig.node.rejectErr = want
	if _, err := rig.client.RejectWorkspaceChanges(t.Context(), uuid); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("reject error = %v", err)
	}
	rig.node.rejectErr = nil
	rig.node.unfreezeErr = want
	if _, err := rig.client.UnfreezeWorkspace(t.Context(), uuid); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("unfreeze error = %v", err)
	}
	rig.node.unfreezeErr = nil
	rig.node.deleteErr = want
	if err := rig.client.DeleteWorkspace(t.Context(), uuid); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("Node delete error = %v", err)
	}
	rig.node.deleteErr = nil
	rig.catalog.deleteErr = want
	if err := rig.client.DeleteWorkspace(t.Context(), uuid); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("catalog delete error = %v", err)
	}
	rig.catalog.deleteErr = nil
	rig.catalog.getErr = want
	if _, err := rig.client.WorkspaceStorageStatus(t.Context(), uuid); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("catalog Get error = %v", err)
	}
}

func TestWorkspaceStorageControlsPropagateDialFailure(t *testing.T) {
	rig := newStorageClientRig(t)
	want := errors.New("dial failed")
	dialWorkspaceService = func(
		context.Context,
		workspacesync.PushConfig,
		string,
		identity.Fingerprint,
	) (workspacev1connect.WorkspaceServiceClient, func() error, error) {
		return nil, nil, want
	}
	uuid := rig.catalog.resource.GetMetadata().GetUuid()
	if _, err := rig.client.WorkspaceStorageStatus(t.Context(), uuid); !errors.Is(err, want) {
		t.Fatalf("status dial error = %v", err)
	}
	if _, err := rig.client.RejectWorkspaceChanges(t.Context(), uuid); !errors.Is(err, want) {
		t.Fatalf("reject dial error = %v", err)
	}
	if _, err := rig.client.UnfreezeWorkspace(t.Context(), uuid); !errors.Is(err, want) {
		t.Fatalf("unfreeze dial error = %v", err)
	}
	if err := rig.client.DeleteWorkspace(t.Context(), uuid); !errors.Is(err, want) {
		t.Fatalf("delete dial error = %v", err)
	}
}
