// Package mcpserver exposes bounded, ownership-safe daft read tools.
package mcpserver

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richiejp/darkaifactory/internal/daftclient"
	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
)

const maxResults = 200

const (
	nodeListTool      = "node_list"
	workspaceGetTool  = "workspace_get"
	workspaceListTool = "workspace_list"
)

type Reader interface {
	ListNodes(context.Context) ([]Node, error)
	ListWorkspaces(context.Context) ([]Workspace, error)
	GetWorkspace(context.Context, string) (Workspace, error)
}

type Node struct {
	Fingerprint string `json:"fingerprint"`
	Hostname    string `json:"hostname,omitempty"`
	Version     string `json:"version,omitempty"`
	CPUs        uint32 `json:"cpus,omitempty"`
	MemoryBytes uint64 `json:"memory_bytes,omitempty"`
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

type getWorkspaceInput struct {
	UUID string `json:"uuid" jsonschema:"canonical workspace UUID"`
}

type getWorkspaceOutput struct {
	Workspace Workspace `json:"workspace"`
}

func New(reader Reader, version string) (*mcp.Server, error) {
	if reader == nil {
		return nil, fmt.Errorf("mcpserver: reader is required")
	}
	if version == "" {
		return nil, fmt.Errorf("mcpserver: version is required")
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "daft", Version: version}, nil)
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: boolPtr(false)}
	mcp.AddTool(server, &mcp.Tool{Name: nodeListTool, Description: "List live daft Nodes visible to the orchestrator.", Annotations: annotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listNodesOutput, error) {
			nodes, truncated, err := readBounded(ctx, reader.ListNodes, func(node Node) string { return node.Fingerprint })
			if err != nil {
				return nil, listNodesOutput{}, fmt.Errorf("list nodes: %w", err)
			}
			return nil, listNodesOutput{Nodes: nodes, Truncated: truncated}, nil
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

type ConnectReader struct {
	nodes     nodeLister
	workspace workspaceReader
}

func NewConnectReader(client *daftclient.Client) (*ConnectReader, error) {
	if client == nil || client.Nodes == nil || client.Workspace == nil {
		return nil, fmt.Errorf("mcpserver: daft client is required")
	}
	return &ConnectReader{nodes: client.Nodes, workspace: client.Workspace}, nil
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
