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
	"github.com/richiejp/darkaifactory/internal/agentruncatalog"
	"github.com/richiejp/darkaifactory/internal/daftclient"
	agentrunv1 "github.com/richiejp/darkaifactory/internal/genpb/agentrun/v1"
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
	agentStartTool       = "agent_start"
	agentStatusTool      = "agent_status"
	agentEventsTool      = "agent_events"
	agentCancelTool      = "agent_cancel"
	agentResultTool      = "agent_result"
)

type Reader interface {
	ListNodes(context.Context) ([]Node, error)
	ListWorkspaces(context.Context) ([]Workspace, error)
	GetWorkspace(context.Context, string) (Workspace, error)
	GetNode(context.Context, string) (Node, error)
	AuthorizeNode(context.Context, string, string) (Node, error)
	RevokeNode(context.Context, string) (Node, error)
	GetAgentRun(context.Context, string) (AgentRun, error)
	AgentRunEvents(context.Context, string, uint64, uint32) (AgentEvents, error)
	CancelAgentRun(context.Context, string) (AgentRun, error)
	StartAgentRun(context.Context, string, agentprofile.Entry, string) (AgentRun, error)
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

type AgentRun struct {
	UUID            string `json:"uuid"`
	WorkspaceUUID   string `json:"workspace_uuid"`
	ProfileName     string `json:"profile_name"`
	ProfileDigest   string `json:"profile_digest"`
	State           string `json:"state"`
	Terminal        bool   `json:"terminal"`
	SandboxUID      string `json:"sandbox_uid,omitempty"`
	NodeFingerprint string `json:"node_fingerprint,omitempty"`
	NodeJobID       string `json:"node_job_id,omitempty"`
	ExitCode        int32  `json:"exit_code,omitempty"`
	ExitCodeSet     bool   `json:"exit_code_set,omitempty"`
	Reason          string `json:"reason,omitempty"`
	Result          string `json:"-"`
	LastEventCursor uint64 `json:"last_event_cursor"`
}

type AgentEvent struct {
	Cursor  uint64 `json:"cursor"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

type AgentEvents struct {
	Events          []AgentEvent `json:"events"`
	NextCursor      uint64       `json:"next_cursor"`
	LastEventCursor uint64       `json:"last_event_cursor"`
	Terminal        bool         `json:"terminal"`
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

type agentRunInput struct {
	UUID string `json:"uuid" jsonschema:"canonical AgentRun UUID"`
}

type agentStartInput struct {
	WorkspaceUUID string `json:"workspace_uuid" jsonschema:"canonical workspace UUID"`
	ProfileName   string `json:"profile_name" jsonschema:"immutable trusted-host AgentProfile name"`
	Prompt        string `json:"prompt" jsonschema:"coding task, at most 262144 bytes"`
}

type agentEventsInput struct {
	UUID        string `json:"uuid" jsonschema:"canonical AgentRun UUID"`
	AfterCursor uint64 `json:"after_cursor,omitempty" jsonschema:"return events strictly after this cursor"`
	Limit       uint32 `json:"limit,omitempty" jsonschema:"maximum events to return, 1 to 200; default 50"`
}

type (
	agentRunOutput struct {
		Run AgentRun `json:"run"`
	}
	agentEventsOutput struct{ AgentEvents }
	agentResultOutput struct {
		UUID        string `json:"uuid"`
		State       string `json:"state"`
		ExitCode    int32  `json:"exit_code,omitempty"`
		ExitCodeSet bool   `json:"exit_code_set"`
		Reason      string `json:"reason,omitempty"`
		Result      string `json:"result,omitempty"`
	}
)

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
	addAgentRunTools(server, annotations, reader, profiles)
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

func addAgentRunTools(server *mcp.Server, readAnnotations *mcp.ToolAnnotations, reader Reader, profiles []agentprofile.Entry) {
	byName := make(map[string]agentprofile.Entry, len(profiles))
	for i := range profiles {
		profile := profiles[i]
		byName[profile.Profile.Name] = profile
	}
	startAnnotations := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: false, OpenWorldHint: boolPtr(false)}
	mcp.AddTool(server, &mcp.Tool{Name: agentStartTool, Description: "Start a durable coding-agent run from an immutable trusted-host profile.", Annotations: startAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agentStartInput) (*mcp.CallToolResult, agentRunOutput, error) {
			if input.WorkspaceUUID == "" || input.ProfileName == "" {
				return nil, agentRunOutput{}, fmt.Errorf("workspace_uuid and profile_name are required")
			}
			if len(input.Prompt) > agentruncatalog.MaxPromptBytes {
				return nil, agentRunOutput{}, fmt.Errorf("prompt exceeds %d bytes", agentruncatalog.MaxPromptBytes)
			}
			profile, ok := byName[input.ProfileName]
			if !ok {
				return nil, agentRunOutput{}, fmt.Errorf("unknown AgentProfile %q", input.ProfileName)
			}
			run, err := reader.StartAgentRun(ctx, input.WorkspaceUUID, profile, input.Prompt)
			return nil, agentRunOutput{Run: run}, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: agentStatusTool, Description: "Get durable status for one owned AgentRun.", Annotations: readAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agentRunInput) (*mcp.CallToolResult, agentRunOutput, error) {
			if input.UUID == "" {
				return nil, agentRunOutput{}, fmt.Errorf("uuid is required")
			}
			run, err := reader.GetAgentRun(ctx, input.UUID)
			return nil, agentRunOutput{Run: run}, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: agentEventsTool, Description: "Read a bounded resumable page of durable AgentRun events.", Annotations: readAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agentEventsInput) (*mcp.CallToolResult, agentEventsOutput, error) {
			if input.UUID == "" {
				return nil, agentEventsOutput{}, fmt.Errorf("uuid is required")
			}
			if input.Limit > uint32(maxResults) {
				return nil, agentEventsOutput{}, fmt.Errorf("limit exceeds %d", maxResults)
			}
			events, err := reader.AgentRunEvents(ctx, input.UUID, input.AfterCursor, input.Limit)
			return nil, agentEventsOutput{AgentEvents: events}, err
		})
	cancelAnnotations := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: true, OpenWorldHint: boolPtr(false)}
	mcp.AddTool(server, &mcp.Tool{Name: agentCancelTool, Description: "Request idempotent cancellation of one owned AgentRun.", Annotations: cancelAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agentRunInput) (*mcp.CallToolResult, agentRunOutput, error) {
			if input.UUID == "" {
				return nil, agentRunOutput{}, fmt.Errorf("uuid is required")
			}
			run, err := reader.CancelAgentRun(ctx, input.UUID)
			return nil, agentRunOutput{Run: run}, err
		})
	mcp.AddTool(server, &mcp.Tool{Name: agentResultTool, Description: "Read the bounded terminal result of one owned AgentRun.", Annotations: readAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agentRunInput) (*mcp.CallToolResult, agentResultOutput, error) {
			if input.UUID == "" {
				return nil, agentResultOutput{}, fmt.Errorf("uuid is required")
			}
			run, err := reader.GetAgentRun(ctx, input.UUID)
			if err != nil {
				return nil, agentResultOutput{}, err
			}
			if !run.Terminal {
				return nil, agentResultOutput{}, fmt.Errorf("AgentRun %s is not terminal", input.UUID)
			}
			return nil, agentResultOutput{UUID: run.UUID, State: run.State, ExitCode: run.ExitCode, ExitCodeSet: run.ExitCodeSet, Reason: run.Reason, Result: run.Result}, nil
		})
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

type agentRunClient interface {
	Start(context.Context, *connect.Request[agentrunv1.StartAgentRunRequest]) (*connect.Response[agentrunv1.AgentRun], error)
	Get(context.Context, *connect.Request[agentrunv1.GetAgentRunRequest]) (*connect.Response[agentrunv1.AgentRun], error)
	Events(context.Context, *connect.Request[agentrunv1.AgentRunEventsRequest]) (*connect.Response[agentrunv1.AgentRunEventsResponse], error)
	Cancel(context.Context, *connect.Request[agentrunv1.CancelAgentRunRequest]) (*connect.Response[agentrunv1.AgentRun], error)
}

type ConnectReader struct {
	nodes     nodeLister
	workspace workspaceReader
	operator  operatorClient
	agentRuns agentRunClient
}

func (r ConnectReader) StartAgentRun(ctx context.Context, workspaceUUID string, profile agentprofile.Entry, prompt string) (AgentRun, error) {
	response, err := r.agentRuns.Start(ctx, connect.NewRequest(&agentrunv1.StartAgentRunRequest{
		WorkspaceUuid: workspaceUUID, ProfileName: profile.Profile.Name, ProfileDigest: profile.Digest,
		Command: slices.Clone(profile.Profile.Command), Prompt: prompt,
	}))
	if err != nil {
		return AgentRun{}, err
	}
	return mapAgentRun(response.Msg), nil
}

func NewConnectReader(client *daftclient.Client) (*ConnectReader, error) {
	if client == nil || client.Nodes == nil || client.Workspace == nil || client.Operator == nil || client.AgentRuns == nil {
		return nil, fmt.Errorf("mcpserver: daft client is required")
	}
	return &ConnectReader{nodes: client.Nodes, workspace: client.Workspace, operator: client.Operator, agentRuns: client.AgentRuns}, nil
}

func (r ConnectReader) GetAgentRun(ctx context.Context, uuid string) (AgentRun, error) {
	response, err := r.agentRuns.Get(ctx, connect.NewRequest(&agentrunv1.GetAgentRunRequest{Uuid: uuid}))
	if err != nil {
		return AgentRun{}, err
	}
	return mapAgentRun(response.Msg), nil
}

func (r ConnectReader) AgentRunEvents(ctx context.Context, uuid string, after uint64, limit uint32) (AgentEvents, error) {
	response, err := r.agentRuns.Events(ctx, connect.NewRequest(&agentrunv1.AgentRunEventsRequest{Uuid: uuid, AfterCursor: after, Limit: limit}))
	if err != nil {
		return AgentEvents{}, err
	}
	out := AgentEvents{NextCursor: response.Msg.GetNextCursor(), LastEventCursor: response.Msg.GetLastEventCursor(), Terminal: response.Msg.GetTerminal(), Events: make([]AgentEvent, len(response.Msg.GetEvents()))}
	for i, event := range response.Msg.GetEvents() {
		out.Events[i] = AgentEvent{Cursor: event.GetCursor(), State: event.GetState().String(), Message: event.GetMessage()}
	}
	return out, nil
}

func (r ConnectReader) CancelAgentRun(ctx context.Context, uuid string) (AgentRun, error) {
	response, err := r.agentRuns.Cancel(ctx, connect.NewRequest(&agentrunv1.CancelAgentRunRequest{Uuid: uuid}))
	if err != nil {
		return AgentRun{}, err
	}
	return mapAgentRun(response.Msg), nil
}

func mapAgentRun(run *agentrunv1.AgentRun) AgentRun {
	state := run.GetState()
	terminal := state == agentrunv1.AgentRunState_AGENT_RUN_STATE_SUCCEEDED || state == agentrunv1.AgentRunState_AGENT_RUN_STATE_FAILED || state == agentrunv1.AgentRunState_AGENT_RUN_STATE_CANCELED || state == agentrunv1.AgentRunState_AGENT_RUN_STATE_LOST
	return AgentRun{UUID: run.GetUuid(), WorkspaceUUID: run.GetWorkspaceUuid(), ProfileName: run.GetProfileName(), ProfileDigest: run.GetProfileDigest(), State: state.String(), Terminal: terminal, SandboxUID: run.GetSandboxUid(), NodeFingerprint: run.GetNodeFingerprint(), NodeJobID: run.GetNodeJobId(), ExitCode: run.GetExitCode(), ExitCodeSet: run.GetExitCodeSet(), Reason: run.GetReason(), Result: run.GetResult(), LastEventCursor: run.GetLastEventCursor()}
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
