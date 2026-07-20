// Package mcpserver exposes bounded, ownership-safe daft read tools.
package mcpserver

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richiejp/darkaifactory/internal/agentprofile"
	"github.com/richiejp/darkaifactory/internal/daftclient"
	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	operatorv1 "github.com/richiejp/darkaifactory/internal/genpb/operator/v1"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
)

const maxResults = 200

const (
	nodeListTool         = "node_list"
	nodeStatusTool       = "node_registration_status"
	nodeAuthorizeTool    = "node_authorize"
	nodeRevokeTool       = "node_revoke"
	workspaceGetTool     = "workspace_get"
	workspaceListTool    = "workspace_list"
	agentProfileListTool = "agent_profile_list"
)

type Reader interface {
	ListNodes(context.Context) ([]Node, error)
	ListWorkspaces(context.Context) ([]Workspace, error)
	GetWorkspace(context.Context, string) (Workspace, error)
	GetNode(context.Context, string) (Node, error)
	AuthorizeNode(context.Context, string, string) (Node, error)
	RevokeNode(context.Context, string) (Node, error)
}

type Node struct {
	Fingerprint string `json:"fingerprint"`
	Hostname    string `json:"hostname,omitempty"`
	Version     string `json:"version,omitempty"`
	CPUs        uint32 `json:"cpus,omitempty"`
	MemoryBytes uint64 `json:"memory_bytes,omitempty"`
	Label       string `json:"label,omitempty"`
	Authorized  bool   `json:"authorized"`
	Connected   bool   `json:"connected"`
}

type Workspace struct {
	UUID               string `json:"uuid"`
	Name               string `json:"name"`
	Generation         uint64 `json:"generation"`
	ObservedGeneration uint64 `json:"observed_generation"`
	CurrentSandboxUUID string `json:"current_sandbox_uuid,omitempty"`
}

type listNodesOutput struct {
	Nodes     []Node `json:"nodes"`
	Truncated bool   `json:"truncated"`
}

type listWorkspacesOutput struct {
	Workspaces []Workspace `json:"workspaces"`
	Truncated  bool        `json:"truncated"`
}

type agentProfileOutput struct {
	Name               string                        `json:"name"`
	Digest             string                        `json:"digest"`
	Provider           agentprofile.Provider         `json:"provider"`
	Image              string                        `json:"image"`
	Command            []string                      `json:"command"`
	CredentialsEnabled bool                          `json:"credentials_enabled"`
	Resources          agentprofile.ResourcesSection `json:"resources"`
	Egress             agentprofile.EgressSection    `json:"egress"`
}

type listAgentProfilesOutput struct {
	Profiles []agentProfileOutput `json:"profiles"`
}

type getWorkspaceInput struct {
	UUID string `json:"uuid" jsonschema:"canonical workspace UUID"`
}

type getWorkspaceOutput struct {
	Workspace Workspace `json:"workspace"`
}

type nodeFingerprintInput struct {
	Fingerprint string `json:"fingerprint" jsonschema:"canonical sha256 certificate fingerprint"`
}

type authorizeNodeInput struct {
	Fingerprint string `json:"fingerprint" jsonschema:"canonical sha256 certificate fingerprint"`
	Label       string `json:"label,omitempty" jsonschema:"human-readable node label, at most 128 bytes"`
}

type nodeOutput struct {
	Node Node `json:"node"`
}

func New(reader Reader, version string) (*mcp.Server, error) {
	return NewWithProfiles(reader, nil, version)
}

func NewWithProfiles(reader Reader, profiles []agentprofile.Entry, version string) (*mcp.Server, error) {
	if reader == nil {
		return nil, fmt.Errorf("mcpserver: reader is required")
	}
	if version == "" {
		return nil, fmt.Errorf("mcpserver: version is required")
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "daft", Version: version}, nil)
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)}
	addAgentProfileListTool(server, annotations, profiles)
	mcp.AddTool(server, &mcp.Tool{Name: nodeListTool, Description: "List live daft Nodes visible to the orchestrator.", Annotations: annotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listNodesOutput, error) {
			nodes, truncated, err := readBounded(ctx, reader.ListNodes, func(node Node) string { return node.Fingerprint })
			if err != nil {
				return nil, listNodesOutput{}, fmt.Errorf("list nodes: %w", err)
			}
			return nil, listNodesOutput{Nodes: nodes, Truncated: truncated}, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: nodeStatusTool, Description: "Get durable authorization and live registration status for one Node.", Annotations: annotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input nodeFingerprintInput) (*mcp.CallToolResult, nodeOutput, error) {
			if err := validateFingerprint(input.Fingerprint); err != nil {
				return nil, nodeOutput{}, err
			}
			node, err := reader.GetNode(ctx, input.Fingerprint)
			if err != nil {
				return nil, nodeOutput{}, fmt.Errorf("get node status: %w", err)
			}
			return nil, nodeOutput{Node: node}, nil
		})
	authorizeAnnotations := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(false)}
	mcp.AddTool(server, &mcp.Tool{Name: nodeAuthorizeTool, Description: "Authorize one expected sandbox Node certificate fingerprint.", Annotations: authorizeAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input authorizeNodeInput) (*mcp.CallToolResult, nodeOutput, error) {
			if err := validateFingerprint(input.Fingerprint); err != nil {
				return nil, nodeOutput{}, err
			}
			node, err := reader.AuthorizeNode(ctx, input.Fingerprint, input.Label)
			if err != nil {
				return nil, nodeOutput{}, fmt.Errorf("authorize node: %w", err)
			}
			return nil, nodeOutput{Node: node}, nil
		})
	revokeAnnotations := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: true, OpenWorldHint: boolPtr(false)}
	mcp.AddTool(server, &mcp.Tool{Name: nodeRevokeTool, Description: "Revoke a sandbox Node and terminate its live registration.", Annotations: revokeAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input nodeFingerprintInput) (*mcp.CallToolResult, nodeOutput, error) {
			if err := validateFingerprint(input.Fingerprint); err != nil {
				return nil, nodeOutput{}, err
			}
			node, err := reader.RevokeNode(ctx, input.Fingerprint)
			if err != nil {
				return nil, nodeOutput{}, fmt.Errorf("revoke node: %w", err)
			}
			return nil, nodeOutput{Node: node}, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: workspaceListTool, Description: "List Workspaces owned by the authenticated client.", Annotations: annotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listWorkspacesOutput, error) {
			workspaces, truncated, err := readBounded(ctx, reader.ListWorkspaces, func(workspace Workspace) string { return workspace.UUID })
			if err != nil {
				return nil, listWorkspacesOutput{}, fmt.Errorf("list workspaces: %w", err)
			}
			return nil, listWorkspacesOutput{Workspaces: workspaces, Truncated: truncated}, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: workspaceGetTool, Description: "Get one owned Workspace by canonical UUID.", Annotations: annotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input getWorkspaceInput) (*mcp.CallToolResult, getWorkspaceOutput, error) {
			if input.UUID == "" {
				return nil, getWorkspaceOutput{}, fmt.Errorf("uuid is required")
			}
			workspace, err := reader.GetWorkspace(ctx, input.UUID)
			if err != nil {
				return nil, getWorkspaceOutput{}, fmt.Errorf("get workspace: %w", err)
			}
			return nil, getWorkspaceOutput{Workspace: workspace}, nil
		})
	return server, nil
}

func addAgentProfileListTool(server *mcp.Server, annotations *mcp.ToolAnnotations, profiles []agentprofile.Entry) {
	profileOutputs := make([]agentProfileOutput, len(profiles))
	for i := range profiles {
		entry := profiles[i]
		profileOutputs[i] = agentProfileOutput{Name: entry.Profile.Name, Digest: entry.Digest, Provider: entry.Profile.Provider, Image: entry.Profile.Image, Command: slices.Clone(entry.Profile.Command), CredentialsEnabled: entry.Profile.Credentials.Enable, Resources: entry.Profile.Resources, Egress: entry.Profile.Egress}
	}
	slices.SortFunc(profileOutputs, func(a, b agentProfileOutput) int { return cmp.Compare(a.Name, b.Name) })
	mcp.AddTool(server, &mcp.Tool{Name: agentProfileListTool, Description: "List immutable coding-agent profiles configured on this trusted host.", Annotations: annotations},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, listAgentProfilesOutput, error) {
			out := slices.Clone(profileOutputs)
			for i := range out {
				out[i].Command = slices.Clone(out[i].Command)
			}
			return nil, listAgentProfilesOutput{Profiles: out}, nil
		})
}

func boundedSorted[T any](values []T, key func(T) string) ([]T, bool) {
	slices.SortFunc(values, func(a, b T) int { return cmp.Compare(key(a), key(b)) })
	truncated := len(values) > maxResults
	if truncated {
		values = values[:maxResults]
	}
	return values, truncated
}

func readBounded[T any](ctx context.Context, read func(context.Context) ([]T, error), key func(T) string) (values []T, truncated bool, err error) {
	values, err = read(ctx)
	if err != nil {
		return nil, false, err
	}
	values, truncated = boundedSorted(values, key)
	return values, truncated, nil
}

type nodeLister interface {
	ListNodes(context.Context, *connect.Request[nodectrlv1.ListNodesRequest]) (*connect.Response[nodectrlv1.ListNodesResponse], error)
}

type workspaceReader interface {
	List(context.Context, *connect.Request[workspacev1.ListWorkspacesRequest]) (*connect.Response[workspacev1.ListWorkspacesResponse], error)
}

type operatorClient interface {
	AuthorizeNode(context.Context, *connect.Request[operatorv1.AuthorizeNodeRequest]) (*connect.Response[operatorv1.NodeStatus], error)
	RevokeNode(context.Context, *connect.Request[operatorv1.RevokeNodeRequest]) (*connect.Response[operatorv1.NodeStatus], error)
	GetNode(context.Context, *connect.Request[operatorv1.GetNodeRequest]) (*connect.Response[operatorv1.NodeStatus], error)
}

type ConnectReader struct {
	nodes     nodeLister
	workspace workspaceReader
	operator  operatorClient
}

func NewConnectReader(client *daftclient.Client) (*ConnectReader, error) {
	if client == nil || client.Nodes == nil || client.Workspace == nil || client.Operator == nil {
		return nil, fmt.Errorf("mcpserver: daft client is required")
	}
	return &ConnectReader{nodes: client.Nodes, workspace: client.Workspace, operator: client.Operator}, nil
}

func (r ConnectReader) GetNode(ctx context.Context, fingerprint string) (Node, error) {
	response, err := r.operator.GetNode(ctx, connect.NewRequest(&operatorv1.GetNodeRequest{Fingerprint: fingerprint}))
	if err != nil {
		return Node{}, err
	}
	return mapOperatorNode(response.Msg), nil
}

func (r ConnectReader) AuthorizeNode(ctx context.Context, fingerprint, label string) (Node, error) {
	response, err := r.operator.AuthorizeNode(ctx, connect.NewRequest(&operatorv1.AuthorizeNodeRequest{Fingerprint: fingerprint, Label: label}))
	if err != nil {
		return Node{}, err
	}
	return mapOperatorNode(response.Msg), nil
}

func (r ConnectReader) RevokeNode(ctx context.Context, fingerprint string) (Node, error) {
	response, err := r.operator.RevokeNode(ctx, connect.NewRequest(&operatorv1.RevokeNodeRequest{Fingerprint: fingerprint}))
	if err != nil {
		return Node{}, err
	}
	return mapOperatorNode(response.Msg), nil
}

func mapOperatorNode(node *operatorv1.NodeStatus) Node {
	return Node{Fingerprint: node.GetFingerprint(), Label: node.GetLabel(), Authorized: node.GetAuthorized(), Connected: node.GetConnected(), Hostname: node.GetHostname(), Version: node.GetVersion(), CPUs: node.GetResources().GetCpus(), MemoryBytes: node.GetResources().GetMemoryBytes()}
}

func validateFingerprint(value string) error {
	if _, err := identity.ParseFingerprint(value); err != nil {
		return fmt.Errorf("fingerprint: %w", err)
	}
	return nil
}

func (r ConnectReader) ListNodes(ctx context.Context) ([]Node, error) {
	response, err := r.nodes.ListNodes(ctx, connect.NewRequest(&nodectrlv1.ListNodesRequest{}))
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(response.Msg.GetNodes()))
	for _, node := range response.Msg.GetNodes() {
		out = append(out, Node{Fingerprint: node.GetFingerprint(), Hostname: node.GetHostname(), Version: node.GetVersion(), CPUs: node.GetResources().GetCpus(), MemoryBytes: node.GetResources().GetMemoryBytes()})
	}
	return out, nil
}

func (r ConnectReader) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	response, err := r.workspace.List(ctx, connect.NewRequest(&workspacev1.ListWorkspacesRequest{MineOnly: true}))
	if err != nil {
		return nil, err
	}
	return mapWorkspaces(response.Msg.GetWorkspaces()), nil
}

func (r ConnectReader) GetWorkspace(ctx context.Context, uuid string) (Workspace, error) {
	workspaces, err := r.ListWorkspaces(ctx)
	if err != nil {
		return Workspace{}, err
	}
	for _, workspace := range workspaces {
		if workspace.UUID == uuid {
			return workspace, nil
		}
	}
	return Workspace{}, fmt.Errorf("owned workspace %q not found", uuid)
}

func mapWorkspaces(input []*workspacev1.Workspace) []Workspace {
	out := make([]Workspace, 0, len(input))
	for _, workspace := range input {
		out = append(out, mapWorkspace(workspace))
	}
	return out
}

func mapWorkspace(input *workspacev1.Workspace) Workspace {
	return Workspace{UUID: input.GetMetadata().GetUuid(), Name: input.GetMetadata().GetName(), Generation: input.GetMetadata().GetGeneration(), ObservedGeneration: input.GetStatus().GetObservedGeneration(), CurrentSandboxUUID: input.GetStatus().GetCurrentSandbox().GetUid()}
}

func boolPtr(value bool) *bool { return &value }
