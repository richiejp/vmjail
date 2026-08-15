// Package mcpserver exposes bounded, ownership-safe daft read tools.
package mcpserver

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/richiejp/darkaifactory/internal/agentcredential"
	"github.com/richiejp/darkaifactory/internal/agentprofile"
	"github.com/richiejp/darkaifactory/internal/agentruncatalog"
	"github.com/richiejp/darkaifactory/internal/daftclient"
	agentrunv1 "github.com/richiejp/darkaifactory/internal/genpb/agentrun/v1"
	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	operatorv1 "github.com/richiejp/darkaifactory/internal/genpb/operator/v1"
	sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"
	secretdeliveryv1 "github.com/richiejp/darkaifactory/internal/genpb/secretdelivery/v1"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/observability/progress"
)

const (
	defaultResults = 50
	maxResults     = 200
	maxCursorBytes = 512
)

const (
	maxWorkspaceItems = 256
	maxWorkspaceBytes = 1 << 20
	defaultTailBytes  = 64 << 10
	maxTailBytes      = 256 << 10
	defaultTailLines  = 50
	maxTailLines      = 200
)

const (
	isolationShared = "shared"
	isolationStrict = "strict"
	protocolTCP     = "tcp"
	protocolUDP     = "udp"
	conditionReady  = "Ready"
)

const (
	nodeListTool            = "node_list"
	nodeStatusTool          = "node_registration_status"
	nodeAuthorizeTool       = "node_authorize"
	nodeRevokeTool          = "node_revoke"
	workspaceGetTool        = "workspace_get"
	workspaceListTool       = "workspace_list"
	workspaceApplyTool      = "workspace_apply"
	workspacePushTool       = "workspace_push"
	workspaceValidateTool   = "workspace_validate"
	workspaceStatusTool     = "workspace_status"
	workspaceLaunchTool     = "workspace_launch"
	workspaceStopTool       = "workspace_stop"
	workspaceStorageTool    = "workspace_storage_status"
	workspaceRejectTool     = "workspace_changes_reject"
	workspaceUnfreezeTool   = "workspace_unfreeze"
	workspaceDeleteTool     = "workspace_delete"
	agentProfileListTool    = "agent_profile_list"
	agentStartTool          = "agent_start"
	agentStatusTool         = "agent_status"
	agentEventsTool         = "agent_events"
	agentCancelTool         = "agent_cancel"
	agentResultTool         = "agent_result"
	observerSummaryTool     = "sandbox_observer_summary"
	observerTailTool        = "sandbox_observer_tail"
	workspaceDiagnosticTool = "workspace_diagnostics"
)

type Reader interface {
	ListNodes(context.Context) ([]Node, error)
	ListWorkspaces(context.Context) ([]Workspace, error)
	GetWorkspace(context.Context, string) (Workspace, error)
	ApplyWorkspace(context.Context, WorkspaceApply) (WorkspaceApplyResult, error)
	PushWorkspace(context.Context, string, string) (daftclient.WorkspacePushResult, error)
	ValidateWorkspace(context.Context, string) (Workspace, error)
	LaunchWorkspace(context.Context, string) (Workspace, error)
	StopWorkspace(context.Context, string) (Workspace, error)
	WorkspaceStorageStatus(context.Context, string) (daftclient.WorkspaceStorageStatus, error)
	RejectWorkspaceChanges(context.Context, string) ([]daftclient.ManifestTransition, error)
	UnfreezeWorkspace(context.Context, string) (daftclient.WorkspaceStorageStatus, error)
	DeleteWorkspace(context.Context, string) error
	GetNode(context.Context, string) (Node, error)
	AuthorizeNode(context.Context, string, string) (Node, error)
	RevokeNode(context.Context, string) (Node, error)
	GetAgentRun(context.Context, string) (AgentRun, error)
	AgentRunEvents(context.Context, string, uint64, uint32) (AgentEvents, error)
	CancelAgentRun(context.Context, string) (AgentRun, error)
	StartAgentRun(context.Context, string, string, agentprofile.Entry, string) (AgentRun, error)
	ObserveSummary(context.Context, string) (ObserverSummary, error)
	ObserverTail(context.Context, string, int, int) (ObserverTail, error)
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
	UUID               string                  `json:"uuid"`
	Name               string                  `json:"name"`
	Generation         uint64                  `json:"generation"`
	ObservedGeneration uint64                  `json:"observed_generation"`
	CurrentSandboxUUID string                  `json:"current_sandbox_uuid,omitempty"`
	Ready              bool                    `json:"ready"`
	Missing            []string                `json:"missing"`
	Spec               WorkspaceSpec           `json:"spec"`
	Storage            *workspaceStorageOutput `json:"storage,omitempty"`
}

type WorkspaceSummary struct {
	UUID               string `json:"uuid"`
	Name               string `json:"name"`
	Generation         uint64 `json:"generation"`
	ObservedGeneration uint64 `json:"observed_generation"`
	CurrentSandboxUUID string `json:"current_sandbox_uuid,omitempty"`
	Ready              bool   `json:"ready"`
}

type WorkspaceSpec struct {
	ImageRef              string           `json:"image_ref,omitempty"`
	DevcontainerPath      string           `json:"devcontainer_path,omitempty"`
	Mounts                []WorkspaceMount `json:"mounts,omitempty"`
	Env                   []WorkspaceEnv   `json:"env,omitempty"`
	Ports                 []WorkspacePort  `json:"ports,omitempty"`
	TargetNodeFingerprint string           `json:"target_node_fingerprint,omitempty"`
	GPUCount              uint32           `json:"gpu_count,omitempty"`
	GPUMinVRAMBytes       uint64           `json:"gpu_min_vram_bytes,omitempty"`
	Isolation             string           `json:"isolation,omitempty"`
	MemoryBytes           uint64           `json:"memory_bytes,omitempty"`
	CPUCores              uint32           `json:"cpu_cores,omitempty"`
	DiskBytes             uint64           `json:"disk_bytes,omitempty"`
	NestedVirtualization  bool             `json:"nested_virtualization,omitempty"`
	PrivilegedFuseMounts  bool             `json:"privileged_fuse_mounts,omitempty"`
	PrivilegedBuild       bool             `json:"privileged_build,omitempty"`
}

type WorkspaceMount struct {
	Target   string `json:"target"`
	Source   string `json:"source,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

type WorkspaceEnv struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type WorkspacePort struct {
	ContainerPort uint32 `json:"container_port"`
	Protocol      string `json:"protocol"`
}

type WorkspaceApply struct {
	UUID               string
	Name               string
	ExpectedGeneration uint64
	Spec               WorkspaceSpec
}

type WorkspaceApplyResult struct {
	Workspace Workspace
	Created   bool
	Changed   bool
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

type ObserverSummary struct {
	Available bool              `json:"available"`
	Summary   *progress.Summary `json:"summary,omitempty"`
}

type ObserverTail struct {
	Data      string `json:"data"`
	Bytes     int    `json:"bytes"`
	Lines     int    `json:"lines"`
	Truncated bool   `json:"truncated"`
}

type WorkspaceDiagnostics struct {
	Summary    ObserverSummary `json:"guest_activity"`
	HostTail   ObserverTail    `json:"host_vm_resources"`
	Coverage   []string        `json:"coverage"`
	BlindSpots []string        `json:"blind_spots"`
}

type listNodesOutput struct {
	Nodes      []Node `json:"nodes"`
	Truncated  bool   `json:"truncated"`
	NextCursor string `json:"next_cursor,omitempty"`
}

type listWorkspacesOutput struct {
	Workspaces []WorkspaceSummary `json:"workspaces"`
	Truncated  bool               `json:"truncated"`
	NextCursor string             `json:"next_cursor,omitempty"`
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
	Profiles   []agentProfileOutput `json:"profiles"`
	Truncated  bool                 `json:"truncated"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

type listInput struct {
	After string `json:"after,omitempty" jsonschema:"return entries with stable key lexically after this cursor"`
	Limit uint32 `json:"limit,omitempty" jsonschema:"maximum entries to return, 1 to 200; default 50"`
}

type getWorkspaceInput struct {
	UUID string `json:"uuid" jsonschema:"canonical workspace UUID"`
}

type getWorkspaceOutput struct {
	Workspace Workspace `json:"workspace"`
}

type workspaceApplyInput struct {
	UUID               string        `json:"uuid,omitempty" jsonschema:"existing canonical Workspace UUID; mutually exclusive with name"`
	Name               string        `json:"name,omitempty" jsonschema:"owner-scoped Workspace name; use with expected_generation 0 to create"`
	ExpectedGeneration uint64        `json:"expected_generation" jsonschema:"0 creates by name; positive values conditionally update"`
	Spec               WorkspaceSpec `json:"spec"`
}

type workspaceApplyOutput struct {
	Workspace Workspace `json:"workspace"`
	Created   bool      `json:"created"`
	Changed   bool      `json:"changed"`
}

type workspacePushInput struct {
	UUID        string `json:"uuid" jsonschema:"canonical workspace UUID"`
	ProjectPath string `json:"project_path" jsonschema:"absolute trusted-host project directory containing .daft/config.json"`
}

type workspacePushOutput struct {
	ProjectPath string                     `json:"project_path"`
	Mounts      []workspacesyncMountOutput `json:"mounts"`
}

type workspaceStorageOutput struct {
	WorkspaceUUID           string                     `json:"workspace_uuid"`
	DiskUsedBytes           uint64                     `json:"disk_used_bytes"`
	DiskFreeBytes           uint64                     `json:"disk_free_bytes"`
	DiskBytes               uint64                     `json:"disk_bytes"`
	DiskFrozen              bool                       `json:"disk_frozen"`
	DiskRunning             bool                       `json:"disk_running"`
	WorkspaceProtectedBytes uint64                     `json:"workspace_protected_bytes"`
	Manifests               []manifestTransitionOutput `json:"manifests"`
	CASUsageBytes           uint64                     `json:"cas_usage_bytes"`
	ProtectedBytes          uint64                     `json:"protected_bytes"`
	RecoverableBytes        uint64                     `json:"recoverable_bytes"`
	QueuedBytes             uint64                     `json:"queued_bytes"`
	ChunkCount              uint64                     `json:"chunk_count"`
	QueuedCount             uint64                     `json:"queued_count"`
	HardBytes               uint64                     `json:"hard_bytes"`
	SoftBytes               uint64                     `json:"soft_bytes"`
	TargetBytes             uint64                     `json:"target_bytes"`
	ExpirySeconds           int64                      `json:"expiry_seconds"`
}

type manifestTransitionOutput struct {
	Workspace  string `json:"workspace"`
	Digest     string `json:"manifest_digest,omitempty"`
	Generation uint64 `json:"generation,omitempty"`
	Present    bool   `json:"current_manifest_present"`
}

type workspaceRejectOutput struct {
	Manifests []manifestTransitionOutput `json:"manifests"`
}

type workspaceDeleteOutput struct {
	UUID    string `json:"uuid"`
	Deleted bool   `json:"deleted"`
}

type workspacesyncMountOutput struct {
	Remote  string `json:"remote"`
	Entries int    `json:"entries"`
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
	RequestID     string `json:"request_id" jsonschema:"caller-generated canonical UUID; exact retries converge to one AgentRun"`
	WorkspaceUUID string `json:"workspace_uuid" jsonschema:"canonical workspace UUID"`
	ProfileName   string `json:"profile_name" jsonschema:"immutable trusted-host AgentProfile name"`
	Prompt        string `json:"prompt" jsonschema:"coding task, at most 262144 bytes"`
}

type agentEventsInput struct {
	UUID        string `json:"uuid" jsonschema:"canonical AgentRun UUID"`
	AfterCursor uint64 `json:"after_cursor,omitempty" jsonschema:"return events strictly after this cursor"`
	Limit       uint32 `json:"limit,omitempty" jsonschema:"maximum events to return, 1 to 200; default 50"`
}

type observerTailInput struct {
	UUID     string `json:"uuid" jsonschema:"canonical Workspace UUID"`
	MaxBytes uint32 `json:"max_bytes,omitempty" jsonschema:"maximum retained output bytes, 1 to 262144; default 65536"`
	MaxLines uint32 `json:"max_lines,omitempty" jsonschema:"maximum retained lines, 1 to 200; default 50"`
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
	addObserverSummaryTool(server, annotations, reader)
	addObserverTailTool(server, annotations, reader)
	addWorkspaceDiagnosticTool(server, annotations, reader)
	mcp.AddTool(server, &mcp.Tool{Name: nodeListTool, Description: "List a bounded, cursor-resumable page of live daft Nodes visible to the orchestrator.", Annotations: annotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input listInput) (*mcp.CallToolResult, listNodesOutput, error) {
			nodes, next, truncated, err := readPage(ctx, reader.ListNodes, input, func(node Node) string { return node.Fingerprint })
			if err != nil {
				return nil, listNodesOutput{}, fmt.Errorf("list nodes: %w", err)
			}
			return nil, listNodesOutput{Nodes: nodes, Truncated: truncated, NextCursor: next}, nil
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
	addWorkspaceTools(server, annotations, reader)
	return server, nil
}

func addWorkspaceDiagnosticTool(server *mcp.Server, annotations *mcp.ToolAnnotations, reader Reader) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        workspaceDiagnosticTool,
		Description: "Get one bounded diagnostic bundle: guest workload activity plus host VM/resource samples, with explicit coverage and blind spots.",
		Annotations: annotations,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input observerTailInput) (*mcp.CallToolResult, WorkspaceDiagnostics, error) {
		maxBytes, maxLines, err := observerTailLimits(input)
		if err != nil {
			return nil, WorkspaceDiagnostics{}, err
		}
		summary, err := reader.ObserveSummary(ctx, input.UUID)
		if err != nil {
			return nil, WorkspaceDiagnostics{}, fmt.Errorf("diagnose guest workload: %w", err)
		}
		tail, err := reader.ObserverTail(ctx, input.UUID, maxBytes, maxLines)
		if err != nil {
			return nil, WorkspaceDiagnostics{}, fmt.Errorf("diagnose host VM: %w", err)
		}
		return nil, WorkspaceDiagnostics{
			Summary: summary, HostTail: tail,
			Coverage:   []string{"guest exec/file-open/sleep/file-and-socket-I/O activity", "host VM CPU/memory/VMM-I/O/cgroup/aggregate-network samples"},
			BlindSpots: []string{"syscall stacks and kernel wait channels", "per-flow network detail and TCP retransmits"},
		}, nil
	})
}

func addObserverSummaryTool(server *mcp.Server, annotations *mcp.ToolAnnotations, reader Reader) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        observerSummaryTool,
		Description: "Read the current bounded build-progress summary for one owned Workspace's running Sandbox.",
		Annotations: annotations,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input getWorkspaceInput) (*mcp.CallToolResult, ObserverSummary, error) {
		if input.UUID == "" {
			return nil, ObserverSummary{}, fmt.Errorf("uuid is required")
		}
		summary, err := reader.ObserveSummary(ctx, input.UUID)
		if err != nil {
			return nil, ObserverSummary{}, fmt.Errorf("observe Sandbox summary: %w", err)
		}
		return nil, summary, nil
	})
}

func addObserverTailTool(server *mcp.Server, annotations *mcp.ToolAnnotations, reader Reader) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        observerTailTool,
		Description: "Read a byte/line-capped tail snapshot of one owned Workspace's running Sandbox observer log.",
		Annotations: annotations,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input observerTailInput) (*mcp.CallToolResult, ObserverTail, error) {
		maxBytes, maxLines, err := observerTailLimits(input)
		if err != nil {
			return nil, ObserverTail{}, err
		}
		tail, err := reader.ObserverTail(ctx, input.UUID, maxBytes, maxLines)
		if err != nil {
			return nil, ObserverTail{}, fmt.Errorf("tail Sandbox observer: %w", err)
		}
		return nil, tail, nil
	})
}

func observerTailLimits(input observerTailInput) (maxBytes, maxLines int, err error) {
	if input.UUID == "" {
		return 0, 0, fmt.Errorf("uuid is required")
	}
	if input.MaxBytes > maxTailBytes {
		return 0, 0, fmt.Errorf("max_bytes exceeds %d", maxTailBytes)
	}
	if input.MaxLines > maxTailLines {
		return 0, 0, fmt.Errorf("max_lines exceeds %d", maxTailLines)
	}
	maxBytes, maxLines = int(input.MaxBytes), int(input.MaxLines)
	if maxBytes == 0 {
		maxBytes = defaultTailBytes
	}
	if maxLines == 0 {
		maxLines = defaultTailLines
	}
	return maxBytes, maxLines, nil
}

func addWorkspaceTools(server *mcp.Server, readAnnotations *mcp.ToolAnnotations, reader Reader) {
	mcp.AddTool(server, &mcp.Tool{Name: workspaceListTool, Description: "List a bounded, cursor-resumable page of Workspaces owned by the authenticated client.", Annotations: readAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input listInput) (*mcp.CallToolResult, listWorkspacesOutput, error) {
			workspaces, next, truncated, err := readPage(ctx, reader.ListWorkspaces, input, func(workspace Workspace) string { return workspace.UUID })
			if err != nil {
				return nil, listWorkspacesOutput{}, fmt.Errorf("list workspaces: %w", err)
			}
			summaries := make([]WorkspaceSummary, len(workspaces))
			for i := range workspaces {
				summaries[i] = workspaceSummary(workspaces[i])
			}
			return nil, listWorkspacesOutput{Workspaces: summaries, Truncated: truncated, NextCursor: next}, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: workspaceGetTool, Description: "Get one owned Workspace by canonical UUID.", Annotations: readAnnotations},
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
	mcp.AddTool(server, &mcp.Tool{Name: workspaceStatusTool, Description: "Get the complete bounded status and declaration of one owned Workspace.", Annotations: readAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input getWorkspaceInput) (*mcp.CallToolResult, getWorkspaceOutput, error) {
			if input.UUID == "" {
				return nil, getWorkspaceOutput{}, fmt.Errorf("uuid is required")
			}
			workspace, err := reader.GetWorkspace(ctx, input.UUID)
			if err != nil {
				return nil, getWorkspaceOutput{}, err
			}
			storage, err := reader.WorkspaceStorageStatus(ctx, input.UUID)
			if err != nil {
				return nil, getWorkspaceOutput{}, err
			}
			projected := workspaceStorageOutputFromClient(storage)
			workspace.Storage = &projected
			return nil, getWorkspaceOutput{Workspace: workspace}, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: workspaceValidateTool, Description: "Read the server-derived readiness of one owned Workspace without mutation.", Annotations: readAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input getWorkspaceInput) (*mcp.CallToolResult, getWorkspaceOutput, error) {
			if input.UUID == "" {
				return nil, getWorkspaceOutput{}, fmt.Errorf("uuid is required")
			}
			workspace, err := reader.ValidateWorkspace(ctx, input.UUID)
			return nil, getWorkspaceOutput{Workspace: workspace}, err
		})
	applyAnnotations := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(false)}
	mcp.AddTool(server, &mcp.Tool{Name: workspaceApplyTool, Description: "Atomically create or converge one owned Workspace declaration with optimistic concurrency.", Annotations: applyAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input workspaceApplyInput) (*mcp.CallToolResult, workspaceApplyOutput, error) {
			if err := validateWorkspaceApply(input); err != nil {
				return nil, workspaceApplyOutput{}, err
			}
			result, err := reader.ApplyWorkspace(ctx, WorkspaceApply(input))
			return nil, workspaceApplyOutput(result), err
		})
	addWorkspacePushTool(server, reader)
	launchAnnotations := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(false)}
	mcp.AddTool(server, &mcp.Tool{Name: workspaceLaunchTool, Description: "Idempotently launch one ready owned Workspace.", Annotations: launchAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input getWorkspaceInput) (*mcp.CallToolResult, getWorkspaceOutput, error) {
			if input.UUID == "" {
				return nil, getWorkspaceOutput{}, fmt.Errorf("uuid is required")
			}
			workspace, err := reader.LaunchWorkspace(ctx, input.UUID)
			return nil, getWorkspaceOutput{Workspace: workspace}, err
		})
	stopAnnotations := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: true, OpenWorldHint: boolPtr(false)}
	mcp.AddTool(server, &mcp.Tool{Name: workspaceStopTool, Description: "Idempotently stop the current Sandbox only after writable mounts are durably snapshotted and its retained disk is flushed; preservation failure refuses teardown.", Annotations: stopAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input getWorkspaceInput) (*mcp.CallToolResult, getWorkspaceOutput, error) {
			if input.UUID == "" {
				return nil, getWorkspaceOutput{}, fmt.Errorf("uuid is required")
			}
			workspace, err := reader.StopWorkspace(ctx, input.UUID)
			return nil, getWorkspaceOutput{Workspace: workspace}, err
		})
	addWorkspaceStorageTools(server, readAnnotations, reader)
}

func addWorkspaceStorageTools(server *mcp.Server, readAnnotations *mcp.ToolAnnotations, reader Reader) {
	mcp.AddTool(server, &mcp.Tool{Name: workspaceStorageTool, Description: "Get bounded retained-disk and target-Node CAS accounting for one Workspace.", Annotations: readAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input getWorkspaceInput) (*mcp.CallToolResult, workspaceStorageOutput, error) {
			if input.UUID == "" {
				return nil, workspaceStorageOutput{}, fmt.Errorf("uuid is required")
			}
			status, err := reader.WorkspaceStorageStatus(ctx, input.UUID)
			return nil, workspaceStorageOutputFromClient(status), err
		})
	rejectAnnotations := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: true, OpenWorldHint: boolPtr(false)}
	mcp.AddTool(server, &mcp.Tool{Name: workspaceRejectTool, Description: "Reject exact unadopted Sandbox manifests and discard the stopped retained disk.", Annotations: rejectAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input getWorkspaceInput) (*mcp.CallToolResult, workspaceRejectOutput, error) {
			if input.UUID == "" {
				return nil, workspaceRejectOutput{}, fmt.Errorf("uuid is required")
			}
			transitions, err := reader.RejectWorkspaceChanges(ctx, input.UUID)
			out := make([]manifestTransitionOutput, len(transitions))
			for index := range transitions {
				out[index] = manifestTransitionOutput{
					Workspace: transitions[index].Workspace, Digest: transitions[index].Digest,
					Generation: transitions[index].Generation, Present: transitions[index].Present,
				}
			}
			return nil, workspaceRejectOutput{Manifests: out}, err
		})
	unfreezeAnnotations := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(false)}
	mcp.AddTool(server, &mcp.Tool{Name: workspaceUnfreezeTool, Description: "Explicitly unfreeze a remediated Workspace disk after exact-manifest validation.", Annotations: unfreezeAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input getWorkspaceInput) (*mcp.CallToolResult, workspaceStorageOutput, error) {
			if input.UUID == "" {
				return nil, workspaceStorageOutput{}, fmt.Errorf("uuid is required")
			}
			status, err := reader.UnfreezeWorkspace(ctx, input.UUID)
			return nil, workspaceStorageOutputFromClient(status), err
		})
	deleteAnnotations := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(true), IdempotentHint: true, OpenWorldHint: boolPtr(false)}
	mcp.AddTool(server, &mcp.Tool{Name: workspaceDeleteTool, Description: "Permanently delete a stopped Workspace's Node data and exact catalog generation.", Annotations: deleteAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input getWorkspaceInput) (*mcp.CallToolResult, workspaceDeleteOutput, error) {
			if input.UUID == "" {
				return nil, workspaceDeleteOutput{}, fmt.Errorf("uuid is required")
			}
			err := reader.DeleteWorkspace(ctx, input.UUID)
			return nil, workspaceDeleteOutput{UUID: input.UUID, Deleted: err == nil}, err
		})
}

func workspaceStorageOutputFromClient(input daftclient.WorkspaceStorageStatus) workspaceStorageOutput {
	output := workspaceStorageOutput{
		WorkspaceUUID: input.WorkspaceUUID, DiskUsedBytes: input.DiskUsedBytes,
		DiskFreeBytes: input.DiskFreeBytes, DiskBytes: input.DiskBytes,
		DiskFrozen: input.DiskFrozen, DiskRunning: input.DiskRunning,
		WorkspaceProtectedBytes: input.WorkspaceProtectedBytes,
		CASUsageBytes:           input.CASUsageBytes, ProtectedBytes: input.ProtectedBytes,
		RecoverableBytes: input.RecoverableBytes, QueuedBytes: input.QueuedBytes,
		ChunkCount: input.ChunkCount, QueuedCount: input.QueuedCount,
		HardBytes: input.HardBytes, SoftBytes: input.SoftBytes,
		TargetBytes: input.TargetBytes, ExpirySeconds: input.ExpirySeconds,
	}
	for index := range input.Manifests {
		output.Manifests = append(output.Manifests, manifestTransitionOutput{
			Workspace: input.Manifests[index].Workspace, Digest: input.Manifests[index].Digest,
			Generation: input.Manifests[index].Generation, Present: input.Manifests[index].Present,
		})
	}
	return output
}

func addWorkspacePushTool(server *mcp.Server, reader Reader) {
	annotations := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(false)}
	mcp.AddTool(server, &mcp.Tool{Name: workspacePushTool, Description: "Upload project-root-confined mounts from an explicit trusted-host path to one Workspace's assigned Node.", Annotations: annotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input workspacePushInput) (*mcp.CallToolResult, workspacePushOutput, error) {
			if input.UUID == "" || input.ProjectPath == "" {
				return nil, workspacePushOutput{}, fmt.Errorf("uuid and project_path are required")
			}
			if !filepath.IsAbs(input.ProjectPath) || len(input.ProjectPath) > 4096 {
				return nil, workspacePushOutput{}, fmt.Errorf("project_path must be an absolute path of at most 4096 bytes")
			}
			result, err := reader.PushWorkspace(ctx, input.UUID, input.ProjectPath)
			if err != nil {
				return nil, workspacePushOutput{}, fmt.Errorf("push workspace: %w", err)
			}
			mounts := make([]workspacesyncMountOutput, len(result.Mounts))
			for i := range result.Mounts {
				mounts[i] = workspacesyncMountOutput{Remote: result.Mounts[i].Remote, Entries: result.Mounts[i].Entries}
			}
			return nil, workspacePushOutput{ProjectPath: result.ProjectDir, Mounts: mounts}, nil
		})
}

func workspaceSummary(input Workspace) WorkspaceSummary {
	return WorkspaceSummary{UUID: input.UUID, Name: input.Name, Generation: input.Generation, ObservedGeneration: input.ObservedGeneration, CurrentSandboxUUID: input.CurrentSandboxUUID, Ready: input.Ready}
}

func validateWorkspaceApply(input workspaceApplyInput) error {
	if (input.UUID == "") == (input.Name == "") {
		return fmt.Errorf("exactly one of uuid or name is required")
	}
	if input.UUID != "" && input.ExpectedGeneration == 0 {
		return fmt.Errorf("uuid updates require expected_generation")
	}
	if len(input.Name) > 255 {
		return fmt.Errorf("workspace declaration exceeds bounded input limits")
	}
	return validateWorkspaceSpecBounds(input.Spec)
}

func canonicalUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validateWorkspaceSpecBounds(spec WorkspaceSpec) error {
	if len(spec.Mounts) > maxWorkspaceItems || len(spec.Env) > maxWorkspaceItems || len(spec.Ports) > maxWorkspaceItems {
		return fmt.Errorf("workspace declaration exceeds bounded input limits")
	}
	if len(spec.ImageRef) > 4096 || len(spec.DevcontainerPath) > 4096 || len(spec.TargetNodeFingerprint) > 256 {
		return fmt.Errorf("workspace declaration contains an oversized field")
	}
	if spec.Isolation != "" && spec.Isolation != isolationShared && spec.Isolation != isolationStrict {
		return fmt.Errorf("isolation must be shared or strict")
	}
	totalBytes := len(spec.ImageRef) + len(spec.DevcontainerPath) + len(spec.TargetNodeFingerprint)
	for _, mount := range spec.Mounts {
		totalBytes += len(mount.Target) + len(mount.Source)
	}
	for _, env := range spec.Env {
		totalBytes += len(env.Name) + len(env.Value)
	}
	for _, port := range spec.Ports {
		if port.Protocol != protocolTCP && port.Protocol != protocolUDP {
			return fmt.Errorf("port protocol must be tcp or udp")
		}
	}
	if totalBytes > maxWorkspaceBytes {
		return fmt.Errorf("workspace declaration exceeds 1 MiB")
	}
	return nil
}

func addAgentRunTools(server *mcp.Server, readAnnotations *mcp.ToolAnnotations, reader Reader, profiles []agentprofile.Entry) {
	byName := make(map[string]agentprofile.Entry, len(profiles))
	for i := range profiles {
		profile := profiles[i]
		byName[profile.Profile.Name] = profile
	}
	startAnnotations := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true, OpenWorldHint: boolPtr(false)}
	mcp.AddTool(server, &mcp.Tool{Name: agentStartTool, Description: "Idempotently start a durable coding-agent run from an immutable trusted-host profile.", Annotations: startAnnotations},
		func(ctx context.Context, _ *mcp.CallToolRequest, input agentStartInput) (*mcp.CallToolResult, agentRunOutput, error) {
			if !canonicalUUID(input.RequestID) || input.WorkspaceUUID == "" || input.ProfileName == "" {
				return nil, agentRunOutput{}, fmt.Errorf("canonical request_id, workspace_uuid, and profile_name are required")
			}
			if len(input.Prompt) > agentruncatalog.MaxPromptBytes {
				return nil, agentRunOutput{}, fmt.Errorf("prompt exceeds %d bytes", agentruncatalog.MaxPromptBytes)
			}
			profile, ok := byName[input.ProfileName]
			if !ok {
				return nil, agentRunOutput{}, fmt.Errorf("unknown AgentProfile %q", input.ProfileName)
			}
			run, err := reader.StartAgentRun(ctx, input.RequestID, input.WorkspaceUUID, profile, input.Prompt)
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
	mcp.AddTool(server, &mcp.Tool{Name: agentProfileListTool, Description: "List a bounded, cursor-resumable page of immutable coding-agent profiles configured on this trusted host.", Annotations: annotations},
		func(_ context.Context, _ *mcp.CallToolRequest, input listInput) (*mcp.CallToolResult, listAgentProfilesOutput, error) {
			out, next, truncated, err := pageSorted(profileOutputs, input, func(profile agentProfileOutput) string { return profile.Name })
			if err != nil {
				return nil, listAgentProfilesOutput{}, fmt.Errorf("list agent profiles: %w", err)
			}
			for i := range out {
				out[i].Command = slices.Clone(out[i].Command)
			}
			return nil, listAgentProfilesOutput{Profiles: out, Truncated: truncated, NextCursor: next}, nil
		})
}

func pageSorted[T any](values []T, input listInput, key func(T) string) (page []T, next string, truncated bool, err error) {
	limit, err := listLimit(input)
	if err != nil {
		return nil, "", false, err
	}
	values = slices.Clone(values)
	slices.SortFunc(values, func(a, b T) int { return cmp.Compare(key(a), key(b)) })
	start := 0
	for start < len(values) && key(values[start]) <= input.After {
		start++
	}
	values = values[start:]
	truncated = len(values) > limit
	if truncated {
		values = values[:limit]
	}
	if truncated && len(values) > 0 {
		next = key(values[len(values)-1])
	}
	return values, next, truncated, nil
}

func listLimit(input listInput) (int, error) {
	if len(input.After) > maxCursorBytes {
		return 0, fmt.Errorf("after cursor exceeds %d bytes", maxCursorBytes)
	}
	if input.Limit > maxResults {
		return 0, fmt.Errorf("limit exceeds %d", maxResults)
	}
	limit := int(input.Limit)
	if limit == 0 {
		limit = defaultResults
	}
	return limit, nil
}

func readPage[T any](ctx context.Context, read func(context.Context) ([]T, error), input listInput, key func(T) string) (values []T, next string, truncated bool, err error) {
	if _, err := listLimit(input); err != nil {
		return nil, "", false, err
	}
	values, err = read(ctx)
	if err != nil {
		return nil, "", false, err
	}
	return pageSorted(values, input, key)
}

type nodeLister interface {
	ListNodes(context.Context, *connect.Request[nodectrlv1.ListNodesRequest]) (*connect.Response[nodectrlv1.ListNodesResponse], error)
}

//nolint:dupl // Generated Workspace and AgentRun client seams are necessarily parallel but carry distinct protocol types.
type workspaceReader interface {
	List(context.Context, *connect.Request[workspacev1.ListWorkspacesRequest]) (*connect.Response[workspacev1.ListWorkspacesResponse], error)
	Get(context.Context, *connect.Request[workspacev1.GetWorkspaceRequest]) (*connect.Response[workspacev1.WorkspaceResponse], error)
	Apply(context.Context, *connect.Request[workspacev1.ApplyWorkspaceRequest]) (*connect.Response[workspacev1.ApplyWorkspaceResponse], error)
	Validate(context.Context, *connect.Request[workspacev1.ValidateRequest]) (*connect.Response[workspacev1.WorkspaceResponse], error)
	Launch(context.Context, *connect.Request[workspacev1.LaunchRequest]) (*connect.Response[workspacev1.WorkspaceResponse], error)
	Stop(context.Context, *connect.Request[workspacev1.StopRequest]) (*connect.Response[workspacev1.WorkspaceResponse], error)
	ObserveSummary(context.Context, *connect.Request[workspacev1.ObserveSummaryRequest]) (*connect.Response[workspacev1.ObserveSummaryResponse], error)
	ObserverTail(context.Context, *connect.Request[workspacev1.ObserverTailRequest]) (*connect.ServerStreamForClient[workspacev1.ObserverChunk], error)
}

type operatorClient interface {
	AuthorizeNode(context.Context, *connect.Request[operatorv1.AuthorizeNodeRequest]) (*connect.Response[operatorv1.NodeStatus], error)
	RevokeNode(context.Context, *connect.Request[operatorv1.RevokeNodeRequest]) (*connect.Response[operatorv1.NodeStatus], error)
	GetNode(context.Context, *connect.Request[operatorv1.GetNodeRequest]) (*connect.Response[operatorv1.NodeStatus], error)
}

//nolint:dupl // Generated Workspace and AgentRun client seams are necessarily parallel but carry distinct protocol types.
type agentRunClient interface {
	Start(context.Context, *connect.Request[agentrunv1.StartAgentRunRequest]) (*connect.Response[agentrunv1.AgentRun], error)
	Get(context.Context, *connect.Request[agentrunv1.GetAgentRunRequest]) (*connect.Response[agentrunv1.AgentRun], error)
	Events(context.Context, *connect.Request[agentrunv1.AgentRunEventsRequest]) (*connect.Response[agentrunv1.AgentRunEventsResponse], error)
	Cancel(context.Context, *connect.Request[agentrunv1.CancelAgentRunRequest]) (*connect.Response[agentrunv1.AgentRun], error)
}

type secretDeliveryClient interface {
	Deliver(context.Context, *connect.Request[secretdeliveryv1.DeliverSecretRequest]) (*connect.Response[secretdeliveryv1.DeliverSecretResponse], error)
}

type ConnectReader struct {
	client      *daftclient.Client
	nodes       nodeLister
	workspace   workspaceReader
	operator    operatorClient
	agentRuns   agentRunClient
	secrets     secretDeliveryClient
	credentials *agentcredential.Catalog
}

func (r ConnectReader) StartAgentRun(ctx context.Context, requestID, workspaceUUID string, profile agentprofile.Entry, prompt string) (AgentRun, error) {
	credentialProvider := ""
	if profile.Profile.Credentials.Enable {
		credentialProvider = string(profile.Profile.Provider)
	}
	response, err := r.agentRuns.Start(ctx, connect.NewRequest(&agentrunv1.StartAgentRunRequest{
		RequestId: requestID, WorkspaceUuid: workspaceUUID, ProfileName: profile.Profile.Name, ProfileDigest: profile.Digest,
		Command: slices.Clone(profile.Profile.Command), Prompt: prompt, ImageRef: profile.Profile.Image,
		Resources:          &agentrunv1.AgentRunResources{Enable: profile.Profile.Resources.Enable, CpuCores: profile.Profile.Resources.CPUCores, MemoryBytes: profile.Profile.Resources.MemoryBytes},
		Egress:             &agentrunv1.AgentRunEgress{Enable: profile.Profile.Egress.Enable},
		CredentialRequired: profile.Profile.Credentials.Enable,
		CredentialProvider: credentialProvider,
	}))
	if err != nil {
		return AgentRun{}, err
	}
	if profile.Profile.Credentials.Enable && response.Msg.GetState() == agentrunv1.AgentRunState_AGENT_RUN_STATE_AWAITING_CREDENTIAL {
		if r.credentials == nil || r.secrets == nil {
			return AgentRun{}, fmt.Errorf("mcpserver: credential delivery is not configured")
		}
		credential, ok := r.credentials.Get(profile.Profile.Credentials.Profile)
		if !ok || string(credential.Provider) != credentialProvider {
			return AgentRun{}, fmt.Errorf("mcpserver: credential profile %q does not match agent provider", profile.Profile.Credentials.Profile)
		}
		deliveryID, idErr := mintDeliveryID()
		if idErr != nil {
			return AgentRun{}, idErr
		}
		deliveryErr := credential.Secret.WithBytes(func(payload []byte) error {
			_, callErr := r.secrets.Deliver(ctx, connect.NewRequest(&secretdeliveryv1.DeliverSecretRequest{AgentRunUuid: response.Msg.GetUuid(), DeliveryId: deliveryID, ExpiresAt: timestamppb.New(time.Now().Add(2 * time.Minute)), Kind: secretdeliveryv1.SecretKind_SECRET_KIND_CODEX_AUTH_JSON, Payload: payload}))
			return callErr
		})
		if deliveryErr != nil {
			return AgentRun{}, deliveryErr
		}
		response, err = r.agentRuns.Get(ctx, connect.NewRequest(&agentrunv1.GetAgentRunRequest{Uuid: response.Msg.GetUuid()}))
		if err != nil {
			return AgentRun{}, err
		}
	}
	return mapAgentRun(response.Msg), nil
}

func NewConnectReader(client *daftclient.Client) (*ConnectReader, error) {
	return NewConnectReaderWithCredentials(client, nil)
}

func NewConnectReaderWithCredentials(client *daftclient.Client, credentials *agentcredential.Catalog) (*ConnectReader, error) {
	if client == nil || client.Nodes == nil || client.Workspace == nil || client.Operator == nil || client.AgentRuns == nil {
		return nil, fmt.Errorf("mcpserver: daft client is required")
	}
	return &ConnectReader{client: client, nodes: client.Nodes, workspace: client.Workspace, operator: client.Operator, agentRuns: client.AgentRuns, secrets: client.Secrets, credentials: credentials}, nil
}

func (r ConnectReader) PushWorkspace(ctx context.Context, uuid, projectPath string) (daftclient.WorkspacePushResult, error) {
	return r.client.PushWorkspace(ctx, daftclient.WorkspacePushOptions{
		WorkspaceUUID: uuid,
		ProjectDir:    projectPath,
	})
}

func mintDeliveryID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mcpserver: mint delivery ID: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
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

func (r ConnectReader) ObserveSummary(ctx context.Context, uuid string) (ObserverSummary, error) {
	response, err := r.workspace.ObserveSummary(ctx, connect.NewRequest(&workspacev1.ObserveSummaryRequest{
		Workspace: workspaceUUIDHandle(uuid),
	}))
	if err != nil {
		return ObserverSummary{}, err
	}
	data := response.Msg.GetSummary()
	if len(data) == 0 {
		return ObserverSummary{Available: false}, nil
	}
	summary, err := progress.Parse(data)
	if err != nil {
		return ObserverSummary{}, err
	}
	return ObserverSummary{Available: true, Summary: &summary}, nil
}

func (r ConnectReader) ObserverTail(ctx context.Context, uuid string, maxBytes, maxLines int) (ObserverTail, error) {
	stream, err := r.workspace.ObserverTail(ctx, connect.NewRequest(&workspacev1.ObserverTailRequest{
		Workspace: workspaceUUIDHandle(uuid),
		Follow:    false,
	}))
	if err != nil {
		return ObserverTail{}, err
	}
	if stream == nil {
		return ObserverTail{}, fmt.Errorf("mcpserver: observer tail returned a nil stream")
	}
	defer func() { _ = stream.Close() }()
	return drainObserverTail(stream, maxBytes, maxLines)
}

type observerTailStream interface {
	Receive() bool
	Msg() *workspacev1.ObserverChunk
	Err() error
}

func drainObserverTail(stream observerTailStream, maxBytes, maxLines int) (ObserverTail, error) {
	var retained []byte
	truncated := false
	for stream.Receive() {
		retained, truncated = retainLastBytes(retained, stream.Msg().GetData(), maxBytes, truncated)
	}
	if err := stream.Err(); err != nil {
		return ObserverTail{}, err
	}
	return finishObserverTail(retained, maxLines, truncated), nil
}

func retainLastBytes(retained, chunk []byte, maxBytes int, truncated bool) ([]byte, bool) {
	if len(chunk) >= maxBytes {
		return append(retained[:0], chunk[len(chunk)-maxBytes:]...), true
	}
	overflow := len(retained) + len(chunk) - maxBytes
	if overflow > 0 {
		copy(retained, retained[overflow:])
		retained = retained[:len(retained)-overflow]
		truncated = true
	}
	return append(retained, chunk...), truncated
}

func finishObserverTail(retained []byte, maxLines int, byteTruncated bool) ObserverTail {
	text := strings.TrimRight(string(retained), "\n")
	if byteTruncated {
		if firstNewline := strings.IndexByte(text, '\n'); firstNewline >= 0 {
			text = text[firstNewline+1:]
		} else {
			text = ""
		}
	}
	if text == "" {
		return ObserverTail{Truncated: byteTruncated}
	}
	lines := strings.Split(text, "\n")
	lineTruncated := len(lines) > maxLines
	if lineTruncated {
		lines = lines[len(lines)-maxLines:]
	}
	text = strings.Join(lines, "\n")
	return ObserverTail{Data: text, Bytes: len(text), Lines: len(lines), Truncated: byteTruncated || lineTruncated}
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
	response, err := r.workspace.Get(ctx, connect.NewRequest(&workspacev1.GetWorkspaceRequest{Handle: workspaceUUIDHandle(uuid)}))
	if err != nil {
		return Workspace{}, err
	}
	return checkedWorkspace(response.Msg.GetWorkspace())
}

func (r ConnectReader) ValidateWorkspace(ctx context.Context, uuid string) (Workspace, error) {
	response, err := r.workspace.Validate(ctx, connect.NewRequest(&workspacev1.ValidateRequest{Handle: workspaceUUIDHandle(uuid)}))
	if err != nil {
		return Workspace{}, err
	}
	return checkedWorkspace(response.Msg.GetWorkspace())
}

func (r ConnectReader) LaunchWorkspace(ctx context.Context, uuid string) (Workspace, error) {
	response, err := r.workspace.Launch(ctx, connect.NewRequest(&workspacev1.LaunchRequest{Handle: workspaceUUIDHandle(uuid)}))
	if err != nil {
		return Workspace{}, err
	}
	return checkedWorkspace(response.Msg.GetWorkspace())
}

func (r ConnectReader) StopWorkspace(ctx context.Context, uuid string) (Workspace, error) {
	response, err := r.workspace.Stop(ctx, connect.NewRequest(&workspacev1.StopRequest{Handle: workspaceUUIDHandle(uuid)}))
	if err != nil {
		return Workspace{}, err
	}
	return checkedWorkspace(response.Msg.GetWorkspace())
}

func (r ConnectReader) WorkspaceStorageStatus(ctx context.Context, uuid string) (daftclient.WorkspaceStorageStatus, error) {
	return r.client.WorkspaceStorageStatus(ctx, uuid)
}

func (r ConnectReader) RejectWorkspaceChanges(ctx context.Context, uuid string) ([]daftclient.ManifestTransition, error) {
	return r.client.RejectWorkspaceChanges(ctx, uuid)
}

func (r ConnectReader) UnfreezeWorkspace(ctx context.Context, uuid string) (daftclient.WorkspaceStorageStatus, error) {
	return r.client.UnfreezeWorkspace(ctx, uuid)
}

func (r ConnectReader) DeleteWorkspace(ctx context.Context, uuid string) error {
	return r.client.DeleteWorkspace(ctx, uuid)
}

func (r ConnectReader) ApplyWorkspace(ctx context.Context, input WorkspaceApply) (WorkspaceApplyResult, error) {
	handle := &workspacev1.WorkspaceHandle{}
	if input.UUID != "" {
		handle.Handle = &workspacev1.WorkspaceHandle_Uuid{Uuid: input.UUID}
	} else {
		handle.Handle = &workspacev1.WorkspaceHandle_Name{Name: input.Name}
	}
	response, err := r.workspace.Apply(ctx, connect.NewRequest(&workspacev1.ApplyWorkspaceRequest{
		Handle: handle, ExpectedGeneration: input.ExpectedGeneration, Spec: workspaceSpecToProto(input.Spec),
	}))
	if err != nil {
		return WorkspaceApplyResult{}, err
	}
	workspace, err := checkedWorkspace(response.Msg.GetWorkspace())
	if err != nil {
		return WorkspaceApplyResult{}, err
	}
	return WorkspaceApplyResult{Workspace: workspace, Created: response.Msg.GetCreated(), Changed: response.Msg.GetChanged()}, nil
}

func workspaceUUIDHandle(uuid string) *workspacev1.WorkspaceHandle {
	return &workspacev1.WorkspaceHandle{Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: uuid}}
}

func mapWorkspaces(input []*workspacev1.Workspace) []Workspace {
	out := make([]Workspace, 0, len(input))
	for _, workspace := range input {
		out = append(out, mapWorkspaceBase(workspace))
	}
	return out
}

func mapWorkspace(input *workspacev1.Workspace) Workspace {
	out := mapWorkspaceBase(input)
	out.Missing = slices.Clone(input.GetStatus().GetMissing())
	out.Spec = workspaceSpecFromProto(input.GetSpec())
	return out
}

func mapWorkspaceBase(input *workspacev1.Workspace) Workspace {
	ready := false
	for _, condition := range input.GetStatus().GetConditions() {
		if condition.GetType() == conditionReady && condition.GetStatus() == sandboxv1.ConditionStatus_CONDITION_STATUS_TRUE {
			ready = true
		}
	}
	return Workspace{UUID: input.GetMetadata().GetUuid(), Name: input.GetMetadata().GetName(), Generation: input.GetMetadata().GetGeneration(), ObservedGeneration: input.GetStatus().GetObservedGeneration(), CurrentSandboxUUID: input.GetStatus().GetCurrentSandbox().GetUid(), Ready: ready}
}

func checkedWorkspace(input *workspacev1.Workspace) (Workspace, error) {
	out := mapWorkspace(input)
	if len(out.Name) > 255 || len(out.Missing) > maxWorkspaceItems {
		return Workspace{}, fmt.Errorf("workspace response exceeds bounded output limits")
	}
	if err := validateWorkspaceSpecBounds(out.Spec); err != nil {
		return Workspace{}, fmt.Errorf("workspace response: %w", err)
	}
	return out, nil
}

func workspaceSpecFromProto(input *workspacev1.WorkspaceSpec) WorkspaceSpec {
	out := WorkspaceSpec{ImageRef: input.GetImageRef(), DevcontainerPath: input.GetDevcontainerPath(), TargetNodeFingerprint: input.GetTargetSandboxFingerprint(), GPUCount: input.GetGpuRequest().GetCount(), GPUMinVRAMBytes: input.GetGpuRequest().GetMinVramBytes(), MemoryBytes: input.GetMemoryBytes(), CPUCores: input.GetCpuCores(), DiskBytes: input.GetDiskBytes(), NestedVirtualization: input.GetNestedVirtualization().GetEnable(), PrivilegedFuseMounts: input.GetPrivilegedFuseMounts().GetEnable(), PrivilegedBuild: input.GetPrivilegedBuild().GetEnable()}
	switch input.GetIsolation() {
	case sandboxv1.Isolation_ISOLATION_SHARED:
		out.Isolation = isolationShared
	case sandboxv1.Isolation_ISOLATION_STRICT:
		out.Isolation = isolationStrict
	}
	for _, mount := range input.GetMounts() {
		out.Mounts = append(out.Mounts, WorkspaceMount{Target: mount.GetTarget(), Source: mount.GetSource(), ReadOnly: mount.GetReadOnly()})
	}
	for _, env := range input.GetEnv() {
		out.Env = append(out.Env, WorkspaceEnv{Name: env.GetName(), Value: env.GetValue()})
	}
	for _, port := range input.GetPorts() {
		out.Ports = append(out.Ports, WorkspacePort{ContainerPort: port.GetContainerPort(), Protocol: protocolName(port.GetProtocol())})
	}
	return out
}

func workspaceSpecToProto(input WorkspaceSpec) *workspacev1.WorkspaceSpec {
	out := &workspacev1.WorkspaceSpec{ImageRef: input.ImageRef, DevcontainerPath: input.DevcontainerPath, TargetSandboxFingerprint: input.TargetNodeFingerprint, GpuRequest: &sandboxv1.GpuRequest{Count: input.GPUCount, MinVramBytes: input.GPUMinVRAMBytes}, MemoryBytes: input.MemoryBytes, CpuCores: input.CPUCores, DiskBytes: input.DiskBytes, NestedVirtualization: &sandboxv1.NestedVirtualization{Enable: input.NestedVirtualization}, PrivilegedFuseMounts: &sandboxv1.PrivilegedFuseMounts{Enable: input.PrivilegedFuseMounts}, PrivilegedBuild: &sandboxv1.PrivilegedBuild{Enable: input.PrivilegedBuild}}
	switch input.Isolation {
	case isolationShared:
		out.Isolation = sandboxv1.Isolation_ISOLATION_SHARED
	case isolationStrict:
		out.Isolation = sandboxv1.Isolation_ISOLATION_STRICT
	}
	for _, mount := range input.Mounts {
		out.Mounts = append(out.Mounts, &sandboxv1.Mount{Target: mount.Target, Source: mount.Source, ReadOnly: mount.ReadOnly})
	}
	for _, env := range input.Env {
		out.Env = append(out.Env, &sandboxv1.EnvVar{Name: env.Name, Value: env.Value})
	}
	for _, port := range input.Ports {
		out.Ports = append(out.Ports, &sandboxv1.PortMapping{ContainerPort: port.ContainerPort, Protocol: protocolFromName(port.Protocol)})
	}
	return out
}

func protocolName(protocol sandboxv1.Protocol) string {
	if protocol == sandboxv1.Protocol_PROTOCOL_TCP {
		return protocolTCP
	}
	if protocol == sandboxv1.Protocol_PROTOCOL_UDP {
		return protocolUDP
	}
	return ""
}

func protocolFromName(protocol string) sandboxv1.Protocol {
	if protocol == protocolTCP {
		return sandboxv1.Protocol_PROTOCOL_TCP
	}
	if protocol == protocolUDP {
		return sandboxv1.Protocol_PROTOCOL_UDP
	}
	return sandboxv1.Protocol_PROTOCOL_UNSPECIFIED
}

func boolPtr(value bool) *bool { return &value }
