package daftclient

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/workspace"
	"github.com/richiejp/darkaifactory/internal/workspacesync"
)

// WorkspaceStorageStatus combines one Workspace disk with bounded Node CAS
// accounting. It intentionally contains no chunk list.
type WorkspaceStorageStatus struct {
	WorkspaceUUID           string
	DiskUsedBytes           uint64
	DiskFreeBytes           uint64
	DiskBytes               uint64
	DiskFrozen              bool
	DiskRunning             bool
	WorkspaceProtectedBytes uint64
	Manifests               []ManifestTransition
	CASUsageBytes           uint64
	ProtectedBytes          uint64
	RecoverableBytes        uint64
	QueuedBytes             uint64
	ChunkCount              uint64
	QueuedCount             uint64
	HardBytes               uint64
	SoftBytes               uint64
	TargetBytes             uint64
	ExpirySeconds           int64
}

// ManifestTransition reports the exact manifest restored by rejection.
type ManifestTransition struct {
	Workspace  string
	Digest     string
	Generation uint64
	Present    bool
}

// WorkspaceStorageStatus returns bounded disk and Node CAS accounting.
func (c *Client) WorkspaceStorageStatus(ctx context.Context, uuid string) (WorkspaceStorageStatus, error) {
	resource, node, err := c.workspaceTarget(ctx, uuid)
	if err != nil {
		return WorkspaceStorageStatus{}, err
	}
	client, closeRelay, err := c.dialWorkspaceNode(ctx, resource.GetMetadata().GetUuid(), node)
	if err != nil {
		return WorkspaceStorageStatus{}, err
	}
	defer func() { _ = closeRelay() }()
	disk, err := client.GetWorkspaceDiskStatus(ctx, connect.NewRequest(&workspacev1.GetWorkspaceDiskStatusRequest{
		WorkspaceUuid: resource.GetMetadata().GetUuid(),
	}))
	if err != nil {
		return WorkspaceStorageStatus{}, fmt.Errorf("daft: workspace disk status: %w", err)
	}
	storage, err := client.GetStorageStatus(ctx, connect.NewRequest(&workspacev1.GetStorageStatusRequest{}))
	if err != nil {
		return WorkspaceStorageStatus{}, fmt.Errorf("daft: node storage status: %w", err)
	}
	result := WorkspaceStorageStatus{
		WorkspaceUUID: resource.GetMetadata().GetUuid(),
		DiskUsedBytes: disk.Msg.GetUsedBytes(), DiskFreeBytes: disk.Msg.GetAvailableBytes(),
		DiskBytes: disk.Msg.GetAllowanceBytes(), DiskFrozen: disk.Msg.GetFrozen(), DiskRunning: disk.Msg.GetRunning(),
		WorkspaceProtectedBytes: disk.Msg.GetProtectedBytes(),
		CASUsageBytes:           storage.Msg.GetUsageBytes(), ProtectedBytes: storage.Msg.GetProtectedBytes(),
		RecoverableBytes: storage.Msg.GetRecoverableBytes(), QueuedBytes: storage.Msg.GetQueuedBytes(),
		ChunkCount: storage.Msg.GetChunkCount(), QueuedCount: storage.Msg.GetQueuedCount(),
		HardBytes: storage.Msg.GetHardBytes(), SoftBytes: storage.Msg.GetSoftBytes(),
		TargetBytes: storage.Msg.GetTargetBytes(), ExpirySeconds: storage.Msg.GetExpirySeconds(),
	}
	for _, manifest := range disk.Msg.GetCurrentManifests() {
		result.Manifests = append(result.Manifests, ManifestTransition{
			Workspace: manifest.GetWorkspace(), Digest: manifest.GetManifestDigest(),
			Generation: manifest.GetGeneration(), Present: true,
		})
	}
	return result, nil
}

// RejectWorkspaceChanges deletes the stopped retained disk and rejects each
// exact current writable-mount manifest.
func (c *Client) RejectWorkspaceChanges(ctx context.Context, uuid string) ([]ManifestTransition, error) {
	resource, node, err := c.workspaceTarget(ctx, uuid)
	if err != nil {
		return nil, err
	}
	client, closeRelay, err := c.dialWorkspaceNode(ctx, resource.GetMetadata().GetUuid(), node)
	if err != nil {
		return nil, err
	}
	defer func() { _ = closeRelay() }()
	expected, err := currentManifestExpectations(ctx, client, resource)
	if err != nil {
		return nil, err
	}
	results := make([]ManifestTransition, 0, len(expected))
	for _, item := range expected {
		response, callErr := client.RejectManifest(ctx, connect.NewRequest(&workspacev1.RejectManifestRequest{
			Workspace: item.GetWorkspace(), ManifestDigest: item.GetManifestDigest(), Generation: item.GetGeneration(),
		}))
		if callErr != nil {
			return nil, fmt.Errorf("daft: reject workspace changes %s: %w", item.GetWorkspace(), callErr)
		}
		results = append(results, ManifestTransition{
			Workspace: item.GetWorkspace(), Digest: response.Msg.GetManifestDigest(),
			Generation: response.Msg.GetGeneration(), Present: response.Msg.GetCurrentManifestPresent(),
		})
	}
	return results, nil
}

// UnfreezeWorkspace resumes a storage-frozen running disk only while every
// current writable manifest still matches the caller's observation.
func (c *Client) UnfreezeWorkspace(ctx context.Context, uuid string) (WorkspaceStorageStatus, error) {
	resource, node, err := c.workspaceTarget(ctx, uuid)
	if err != nil {
		return WorkspaceStorageStatus{}, err
	}
	client, closeRelay, err := c.dialWorkspaceNode(ctx, resource.GetMetadata().GetUuid(), node)
	if err != nil {
		return WorkspaceStorageStatus{}, err
	}
	defer func() { _ = closeRelay() }()
	expected, err := currentManifestExpectations(ctx, client, resource)
	if err != nil {
		return WorkspaceStorageStatus{}, err
	}
	response, err := client.UnfreezeWorkspaceDisk(ctx, connect.NewRequest(&workspacev1.UnfreezeWorkspaceDiskRequest{
		WorkspaceUuid: resource.GetMetadata().GetUuid(), ExpectedManifests: expected,
	}))
	if err != nil {
		return WorkspaceStorageStatus{}, fmt.Errorf("daft: unfreeze workspace: %w", err)
	}
	result := WorkspaceStorageStatus{
		WorkspaceUUID: resource.GetMetadata().GetUuid(), DiskUsedBytes: response.Msg.GetUsedBytes(),
		DiskFreeBytes: response.Msg.GetAvailableBytes(), DiskBytes: response.Msg.GetAllowanceBytes(),
		DiskFrozen: response.Msg.GetFrozen(), DiskRunning: response.Msg.GetRunning(),
		WorkspaceProtectedBytes: response.Msg.GetProtectedBytes(),
	}
	for _, manifest := range response.Msg.GetCurrentManifests() {
		result.Manifests = append(result.Manifests, ManifestTransition{
			Workspace: manifest.GetWorkspace(), Digest: manifest.GetManifestDigest(),
			Generation: manifest.GetGeneration(), Present: true,
		})
	}
	return result, nil
}

// ResizeWorkspaceDisk grows the exact currently observed retained disk. The
// Node handles running qcow2/virtio-blk/XFS growth and stopped qcow2 growth.
func (c *Client) ResizeWorkspaceDisk(ctx context.Context, uuid string, diskBytes uint64) (WorkspaceStorageStatus, error) {
	resource, node, err := c.workspaceTarget(ctx, uuid)
	if err != nil {
		return WorkspaceStorageStatus{}, err
	}
	client, closeRelay, err := c.dialWorkspaceNode(ctx, resource.GetMetadata().GetUuid(), node)
	if err != nil {
		return WorkspaceStorageStatus{}, err
	}
	defer func() { _ = closeRelay() }()
	expected, err := currentManifestExpectations(ctx, client, resource)
	if err != nil {
		return WorkspaceStorageStatus{}, err
	}
	response, err := client.ResizeWorkspaceDisk(ctx, connect.NewRequest(&workspacev1.ResizeWorkspaceDiskRequest{
		WorkspaceUuid: resource.GetMetadata().GetUuid(), DiskBytes: diskBytes, ExpectedManifests: expected,
	}))
	if err != nil {
		return WorkspaceStorageStatus{}, fmt.Errorf("daft: resize workspace disk: %w", err)
	}
	return workspaceDiskStatus(response.Msg), nil
}

func workspaceDiskStatus(response *workspacev1.GetWorkspaceDiskStatusResponse) WorkspaceStorageStatus {
	result := WorkspaceStorageStatus{
		WorkspaceUUID: response.GetWorkspaceUuid(), DiskUsedBytes: response.GetUsedBytes(),
		DiskFreeBytes: response.GetAvailableBytes(), DiskBytes: response.GetAllowanceBytes(),
		DiskFrozen: response.GetFrozen(), DiskRunning: response.GetRunning(),
		WorkspaceProtectedBytes: response.GetProtectedBytes(),
	}
	for _, manifest := range response.GetCurrentManifests() {
		result.Manifests = append(result.Manifests, ManifestTransition{
			Workspace: manifest.GetWorkspace(), Digest: manifest.GetManifestDigest(),
			Generation: manifest.GetGeneration(), Present: true,
		})
	}
	return result
}

// DeleteWorkspace durably removes Node data before deleting the exact catalog
// generation. Never-targeted Workspaces have no Node data to contact.
func (c *Client) DeleteWorkspace(ctx context.Context, uuid string) error {
	resource, err := c.getWorkspace(ctx, uuid)
	if err != nil {
		return err
	}
	target := resource.GetSpec().GetTargetSandboxFingerprint()
	if target != "" {
		node, parseErr := identity.ParseFingerprint(target)
		if parseErr != nil {
			return fmt.Errorf("daft: parse target fingerprint: %w", parseErr)
		}
		client, closeRelay, dialErr := c.dialWorkspaceNode(ctx, resource.GetMetadata().GetUuid(), node)
		if dialErr != nil {
			return dialErr
		}
		expected, expectationErr := currentManifestExpectations(ctx, client, resource)
		if expectationErr == nil {
			_, expectationErr = client.DeleteWorkspaceData(ctx, connect.NewRequest(&workspacev1.DeleteWorkspaceDataRequest{
				WorkspaceUuid: resource.GetMetadata().GetUuid(), ExpectedManifests: expected,
			}))
		}
		_ = closeRelay()
		if expectationErr != nil {
			return fmt.Errorf("daft: delete workspace Node data: %w", expectationErr)
		}
	}
	_, err = c.Workspace.Delete(ctx, connect.NewRequest(&workspacev1.DeleteWorkspaceRequest{
		Handle:             workspaceUUIDHandle(resource.GetMetadata().GetUuid()),
		ExpectedGeneration: resource.GetMetadata().GetGeneration(),
	}))
	if err != nil {
		return fmt.Errorf("daft: delete workspace catalog record: %w", err)
	}
	return nil
}

func (c *Client) workspaceTarget(ctx context.Context, uuid string) (*workspacev1.Workspace, identity.Fingerprint, error) {
	resource, err := c.getWorkspace(ctx, uuid)
	if err != nil {
		return nil, identity.Fingerprint{}, err
	}
	target := resource.GetSpec().GetTargetSandboxFingerprint()
	if target == "" {
		return nil, identity.Fingerprint{}, errors.New("daft: workspace has no target node")
	}
	node, err := identity.ParseFingerprint(target)
	if err != nil {
		return nil, identity.Fingerprint{}, fmt.Errorf("daft: parse target fingerprint: %w", err)
	}
	return resource, node, nil
}

func (c *Client) getWorkspace(ctx context.Context, uuid string) (*workspacev1.Workspace, error) {
	if c == nil || c.Workspace == nil || c.identity == nil || c.peers == nil {
		return nil, errors.New("daft: initialized client is required")
	}
	if uuid == "" {
		return nil, errors.New("daft: workspace UUID is required")
	}
	response, err := c.Workspace.Get(ctx, connect.NewRequest(&workspacev1.GetWorkspaceRequest{Handle: workspaceUUIDHandle(uuid)}))
	if err != nil {
		return nil, fmt.Errorf("daft: get workspace: %w", err)
	}
	return response.Msg.GetWorkspace(), nil
}

func (c *Client) dialWorkspaceNode(
	ctx context.Context,
	uuid string,
	node identity.Fingerprint,
) (workspacev1connect.WorkspaceServiceClient, func() error, error) {
	return dialWorkspaceService(ctx, workspacesync.PushConfig{
		Orchestrator: c.Resolved.OrchestratorURL, Identity: c.identity, Peers: c.peers,
	}, uuid, node)
}

//nolint:gochecknoglobals // Narrow transport seam keeps storage controls testable without a live broker.
var dialWorkspaceService = workspacesync.DialWorkspaceService

func currentManifestExpectations(
	ctx context.Context,
	client workspacev1connect.WorkspaceServiceClient,
	resource *workspacev1.Workspace,
) ([]*workspacev1.ManifestExpectation, error) {
	expected := make([]*workspacev1.ManifestExpectation, 0, len(resource.GetSpec().GetMounts()))
	for _, mount := range resource.GetSpec().GetMounts() {
		if mount.GetReadOnly() {
			continue
		}
		key := workspace.SyncKey(resource.GetMetadata().GetUuid(), mount.GetTarget())
		current, err := client.GetManifest(ctx, connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: key}))
		if connect.CodeOf(err) == connect.CodeNotFound {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("daft: inspect workspace manifest %s: %w", mount.GetTarget(), err)
		}
		expected = append(expected, &workspacev1.ManifestExpectation{
			Workspace: key, ManifestDigest: current.Msg.GetManifestDigest(), Generation: current.Msg.GetGeneration(),
		})
	}
	return expected, nil
}

func workspaceUUIDHandle(uuid string) *workspacev1.WorkspaceHandle {
	return &workspacev1.WorkspaceHandle{Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: uuid}}
}
