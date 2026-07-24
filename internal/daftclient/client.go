// Package daftclient provides reusable authenticated clients for an orchestrator.
package daftclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"github.com/richiejp/darkaifactory/internal/cas"
	"github.com/richiejp/darkaifactory/internal/clientconfig"
	"github.com/richiejp/darkaifactory/internal/genpb/agentrun/v1/agentrunv1connect"
	"github.com/richiejp/darkaifactory/internal/genpb/health/v1/healthv1connect"
	"github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1/nodectrlv1connect"
	"github.com/richiejp/darkaifactory/internal/genpb/operator/v1/operatorv1connect"
	"github.com/richiejp/darkaifactory/internal/genpb/secretdelivery/v1/secretdeliveryv1connect"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/workspacesync"
)

const peersFilename = "peers.json"

type Config struct {
	Orchestrator string
	ConfigDir    string
	ProjectDir   string
	Options      []connect.ClientOption
}

// Client owns one authenticated HTTP/2 transport shared by all generated
// service clients. Close releases its idle connections.
type Client struct {
	Resolved  clientconfig.Resolved
	Workspace workspacev1connect.WorkspaceCatalogServiceClient
	Nodes     nodectrlv1connect.NodeControlServiceClient
	Operator  operatorv1connect.OperatorServiceClient
	Health    healthv1connect.HealthServiceClient
	AgentRuns agentrunv1connect.AgentRunServiceClient
	Secrets   secretdeliveryv1connect.SecretDeliveryServiceClient

	transport *http2.Transport
	identity  *identity.Identity
	peers     *identity.PeerStore
}

type WorkspacePushOptions struct {
	WorkspaceUUID string
	ProjectDir    string
	CASDir        string
}

type (
	WorkspacePushResult      = workspacesync.PushResult
	WorkspacePushMountResult = workspacesync.MountResult
)

// Open loads an existing client identity. It never creates credentials, which
// makes daemon startup free of implicit trust-identity mutations.
func Open(cfg Config) (*Client, error) {
	return open(cfg, false)
}

// Bootstrap opens the client and creates a client identity when absent. CLI
// setup paths may opt into this explicitly; long-lived servers should use Open.
func Bootstrap(cfg Config) (*Client, error) {
	return open(cfg, true)
}

func open(cfg Config, bootstrap bool) (*Client, error) {
	resolved, err := clientconfig.Resolve(clientconfig.Config{
		Orchestrator: cfg.Orchestrator,
		ConfigDir:    cfg.ConfigDir,
		ProjectDir:   cfg.ProjectDir,
	})
	if err != nil {
		return nil, err
	}
	baseURL, err := canonicalURL(resolved.OrchestratorURL)
	if err != nil {
		return nil, err
	}
	resolved.OrchestratorURL = baseURL

	var id *identity.Identity
	if bootstrap {
		id, err = identity.LoadOrGenerate(resolved.IdentityDir, identity.RoleClient)
	} else {
		id, err = identity.Load(resolved.IdentityDir)
	}
	if err != nil {
		return nil, fmt.Errorf("daft: load client identity: %w", err)
	}
	peers, err := identity.OpenPeerStore(filepath.Join(resolved.IdentityDir, peersFilename))
	if err != nil {
		return nil, fmt.Errorf("daft: open client peer store: %w", err)
	}
	tlsConfig, err := identity.ClientTLSConfig(id, peers, "")
	if err != nil {
		return nil, err
	}
	transport := &http2.Transport{TLSClientConfig: tlsConfig}
	httpClient := &http.Client{Transport: transport}
	return &Client{
		Resolved:  resolved,
		Workspace: workspacev1connect.NewWorkspaceCatalogServiceClient(httpClient, baseURL, cfg.Options...),
		Nodes:     nodectrlv1connect.NewNodeControlServiceClient(httpClient, baseURL, cfg.Options...),
		Operator:  operatorv1connect.NewOperatorServiceClient(httpClient, baseURL, cfg.Options...),
		Health:    healthv1connect.NewHealthServiceClient(httpClient, baseURL, cfg.Options...),
		AgentRuns: agentrunv1connect.NewAgentRunServiceClient(httpClient, baseURL, cfg.Options...),
		Secrets:   secretdeliveryv1connect.NewSecretDeliveryServiceClient(httpClient, baseURL, cfg.Options...),
		transport: transport,
		identity:  id,
		peers:     peers,
	}, nil
}

// PushWorkspace uploads the mounts declared by one explicit project root to
// the Workspace's assigned Node. It never consults or changes process cwd.
func (c *Client) PushWorkspace(ctx context.Context, opts WorkspacePushOptions) (WorkspacePushResult, error) {
	if c == nil || c.Workspace == nil || c.identity == nil || c.peers == nil {
		return WorkspacePushResult{}, fmt.Errorf("daft: initialized client is required")
	}
	casDir := opts.CASDir
	if casDir == "" {
		casDir = filepath.Join(c.Resolved.IdentityDir, "cas")
	}
	store, err := cas.Open(casDir)
	if err != nil {
		return WorkspacePushResult{}, fmt.Errorf("daft: open workspace CAS: %w", err)
	}
	return workspacesync.Push(ctx, workspacesync.PushConfig{
		Orchestrator:  c.Resolved.OrchestratorURL,
		ProjectDir:    opts.ProjectDir,
		WorkspaceUUID: opts.WorkspaceUUID,
		Identity:      c.identity,
		Peers:         c.peers,
		Store:         store,
		Catalog:       c.Workspace,
	})
}

func canonicalURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("daft: parse orchestrator URL: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("daft: orchestrator URL must be https://host[:port], got %q", raw)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("daft: orchestrator URL must not contain a path, got %q", raw)
	}
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

func (c *Client) Close() {
	if c != nil && c.transport != nil {
		c.transport.CloseIdleConnections()
	}
}
