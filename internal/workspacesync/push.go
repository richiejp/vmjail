// Package workspacesync synchronizes explicitly rooted host projects with a
// Workspace's assigned Node.
package workspacesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/brokerclient"
	"github.com/richiejp/darkaifactory/internal/cas"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/workspace"
)

const (
	projectConfigPath = ".daft/config.json"
	maxConfigBytes    = 1 << 20
	maxMounts         = 256
	maxPathBytes      = 4096
)

type PushConfig struct {
	Orchestrator  string
	ProjectDir    string
	WorkspaceUUID string
	Identity      *identity.Identity
	Peers         *identity.PeerStore
	Store         *cas.Store
	Catalog       Catalog
	DialNode      DialNode
}

type Catalog interface {
	Get(context.Context, *connect.Request[workspacev1.GetWorkspaceRequest]) (*connect.Response[workspacev1.WorkspaceResponse], error)
}

type DialNode func(context.Context, PushConfig, string, identity.Fingerprint) (workspace.PushClient, func() error, error)

type MountResult struct {
	Local   string
	Remote  string
	Entries int
}

type PushResult struct {
	ProjectDir string
	Mounts     []MountResult
}

type projectConfig struct {
	Mounts []mount `json:"mounts"`
}

type mount struct {
	Local  string `json:"local"`
	Remote string `json:"remote"`
}

// Push uploads every dir-local mount recorded by the explicitly named
// project. Mount roots must remain inside ProjectDir after symlink resolution.
func Push(ctx context.Context, cfg PushConfig) (PushResult, error) {
	root, mounts, err := loadProject(cfg.ProjectDir)
	if err != nil {
		return PushResult{}, err
	}
	if err := validateConfig(cfg); err != nil {
		return PushResult{}, err
	}
	if len(mounts) == 0 {
		return PushResult{ProjectDir: root}, nil
	}
	uuid, node, err := resolveTarget(ctx, cfg.Catalog, cfg.WorkspaceUUID)
	if err != nil {
		return PushResult{}, err
	}
	dial := cfg.DialNode
	if dial == nil {
		dial = dialNode
	}
	client, closeRelay, err := dial(ctx, cfg, uuid, node)
	if err != nil {
		return PushResult{}, err
	}
	defer func() { _ = closeRelay() }()

	result := PushResult{ProjectDir: root, Mounts: make([]MountResult, 0, len(mounts))}
	for _, declared := range mounts {
		local, err := confinedMount(root, declared.Local)
		if err != nil {
			return PushResult{}, err
		}
		manifest, err := workspace.BuildManifest(local, cfg.Store, workspace.BuildOptions{})
		if err != nil {
			return PushResult{}, fmt.Errorf("workspace push: build manifest for %s: %w", local, err)
		}
		if err := workspace.Push(ctx, client, cfg.Store, workspace.SyncKey(uuid, declared.Remote), manifest); err != nil {
			return PushResult{}, fmt.Errorf("workspace push: %s -> %s: %w", local, declared.Remote, err)
		}
		result.Mounts = append(result.Mounts, MountResult{
			Local: local, Remote: declared.Remote, Entries: len(manifest.GetEntries()),
		})
	}
	return result, nil
}

func validateConfig(cfg PushConfig) error {
	switch {
	case cfg.WorkspaceUUID == "":
		return errors.New("workspace push: workspace UUID is required")
	case cfg.Orchestrator == "":
		return errors.New("workspace push: orchestrator URL is required")
	case cfg.Identity == nil:
		return errors.New("workspace push: identity is required")
	case cfg.Peers == nil:
		return errors.New("workspace push: peer store is required")
	case cfg.Store == nil:
		return errors.New("workspace push: CAS store is required")
	case cfg.Catalog == nil:
		return errors.New("workspace push: catalog client is required")
	default:
		return nil
	}
}

func loadProject(projectDir string) (string, []mount, error) {
	if projectDir == "" {
		return "", nil, errors.New("workspace push: project directory is required")
	}
	root, err := filepath.Abs(projectDir)
	if err != nil {
		return "", nil, fmt.Errorf("workspace push: resolve project directory: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", nil, fmt.Errorf("workspace push: resolve project directory symlinks: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", nil, fmt.Errorf("workspace push: stat project directory: %w", err)
	}
	if !info.IsDir() {
		return "", nil, errors.New("workspace push: project path is not a directory")
	}
	configPath, err := confinedExistingPath(root, filepath.Join(root, projectConfigPath))
	if err != nil {
		return "", nil, fmt.Errorf("workspace push: resolve project config: %w", err)
	}
	configInfo, err := os.Stat(configPath)
	if err != nil {
		return "", nil, fmt.Errorf("workspace push: stat project config: %w", err)
	}
	if configInfo.Size() > maxConfigBytes {
		return "", nil, errors.New("workspace push: project config exceeds 1 MiB")
	}
	data, err := os.ReadFile(configPath) // #nosec G304 -- path is confined above.
	if err != nil {
		return "", nil, fmt.Errorf("workspace push: read project config: %w", err)
	}
	var config projectConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return "", nil, fmt.Errorf("workspace push: parse project config: %w", err)
	}
	if len(config.Mounts) > maxMounts {
		return "", nil, fmt.Errorf("workspace push: project config exceeds %d mounts", maxMounts)
	}
	for _, declared := range config.Mounts {
		if len(declared.Local) > maxPathBytes || len(declared.Remote) > maxPathBytes {
			return "", nil, errors.New("workspace push: mount path exceeds 4096 bytes")
		}
		if !workspace.SanitisePath(strings.TrimPrefix(declared.Remote, "/")) || !strings.HasPrefix(declared.Remote, "/") {
			return "", nil, fmt.Errorf("workspace push: invalid remote mount path %q", declared.Remote)
		}
	}
	return root, config.Mounts, nil
}

func confinedMount(root, local string) (string, error) {
	if local == "" {
		return "", fmt.Errorf("workspace push: mount path %q escapes project root", local)
	}
	path := local
	if !filepath.IsAbs(path) {
		if !filepath.IsLocal(path) {
			return "", fmt.Errorf("workspace push: mount path %q escapes project root", local)
		}
		path = filepath.Join(root, path)
	}
	path, err := confinedExistingPath(root, path)
	if err != nil {
		return "", fmt.Errorf("workspace push: resolve mount %q: %w", local, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("workspace push: stat mount %q: %w", local, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace push: mount %q is not a directory", local)
	}
	return path, nil
}

func confinedExistingPath(root, path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes project root")
	}
	return resolved, nil
}

func resolveTarget(
	ctx context.Context,
	catalog Catalog,
	uuid string,
) (string, identity.Fingerprint, error) {
	handle := &workspacev1.WorkspaceHandle{Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: uuid}}
	response, err := catalog.Get(ctx, connect.NewRequest(&workspacev1.GetWorkspaceRequest{Handle: handle}))
	if err != nil {
		return "", identity.Fingerprint{}, fmt.Errorf("workspace push: get workspace: %w", err)
	}
	actualUUID := response.Msg.GetWorkspace().GetMetadata().GetUuid()
	target := response.Msg.GetWorkspace().GetSpec().GetTargetSandboxFingerprint()
	if target == "" {
		return "", identity.Fingerprint{}, errors.New("workspace push: workspace has no target node")
	}
	fingerprint, err := identity.ParseFingerprint(target)
	if err != nil {
		return "", identity.Fingerprint{}, fmt.Errorf("workspace push: parse target fingerprint: %w", err)
	}
	return actualUUID, fingerprint, nil
}

func dialNode(
	ctx context.Context,
	cfg PushConfig,
	serveKey string,
	node identity.Fingerprint,
) (workspace.PushClient, func() error, error) {
	broker, err := brokerclient.New(brokerclient.Config{
		URL: cfg.Orchestrator, Identity: cfg.Identity, Peers: cfg.Peers,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("workspace push: broker client: %w", err)
	}
	connection, err := broker.DialBrokeredServe(ctx, brokerclient.DialServeOptions{
		Workspace: serveKey, NodeFingerprint: node,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("workspace push: dial node %s: %w", node, err)
	}
	client := workspacev1connect.NewWorkspaceServiceClient(connection.HTTPClient, connection.BaseURL)
	return client, connection.Close, nil
}
