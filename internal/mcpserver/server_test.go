package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

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
	fingerprintKey   = "fingerprint"
	mcpTestVersion   = "test"
	uuidKey          = "uuid"
	testRunValue     = "run"
	doneValue        = "done"
	mcpTestProfile   = "codex"
	mcpTestPrompt    = "fix it"
	mcpTestWorkspace = "workspace"
	mcpMissing       = "missing"
	profileNameKey   = "profile_name"
	projectPathKey   = "project_path"
	promptKey        = "prompt"
	requestIDKey     = "request_id"
	workspaceUUIDKey = "workspace_uuid"
	nameKey          = "name"
	afterKey         = "after"
	limitKey         = "limit"
	specKey          = "spec"
	mcpTestImage     = "alpine:3"
	mcpTestProject   = "project"
	mcpTestMount     = "/workspace"
	mcpTestRequestID = "123e4567-e89b-42d3-a456-426614174000"
)

type fakeReader struct {
	nodes            []Node
	workspaces       []Workspace
	workspace        Workspace
	listNodesErr     error
	listWorkErr      error
	getWorkErr       error
	requestedUUID    string
	listWorkCalled   int
	operatorNode     Node
	operatorErr      error
	agentRun         AgentRun
	agentEvents      AgentEvents
	agentErr         error
	startedProfile   agentprofile.Entry
	startedPrompt    string
	startedRequestID string
	appliedWorkspace WorkspaceApply
	applyResult      WorkspaceApplyResult
	validatedUUID    string
	launchedUUID     string
	stoppedUUID      string
	pushedUUID       string
	pushedPath       string
	pushResult       daftclient.WorkspacePushResult
	storageStatus    daftclient.WorkspaceStorageStatus
	storageErr       error
	rejectedUUID     string
	rejectResult     []daftclient.ManifestTransition
	unfrozenUUID     string
	deletedUUID      string
	observerSummary  ObserverSummary
	observerErr      error
	observerTail     ObserverTail
	tailUUID         string
	tailBytes        int
	tailLines        int
}

type fakeObserverTailStream struct {
	chunks [][]byte
	index  int
	err    error
}

func (f *fakeObserverTailStream) Receive() bool {
	if f.index >= len(f.chunks) {
		return false
	}
	f.index++
	return true
}

func (f *fakeObserverTailStream) Msg() *workspacev1.ObserverChunk {
	return &workspacev1.ObserverChunk{Data: f.chunks[f.index-1]}
}

func (f *fakeObserverTailStream) Err() error {
	return f.err
}

func TestCanonicalUUID(t *testing.T) {
	for value, want := range map[string]bool{
		mcpTestRequestID:                       true,
		"short":                                false,
		"123e4567xe89b-42d3-a456-426614174000": false,
		"123e4567-e89b-42d3-a456-42661417400g": false,
	} {
		if got := canonicalUUID(value); got != want {
			t.Errorf("canonicalUUID(%q) = %t, want %t", value, got, want)
		}
	}
}

func (f *fakeReader) StartAgentRun(_ context.Context, requestID, workspace string, profile agentprofile.Entry, prompt string) (AgentRun, error) {
	f.startedRequestID, f.requestedUUID, f.startedProfile, f.startedPrompt = requestID, workspace, profile, prompt
	return f.agentRun, f.agentErr
}

func (f *fakeReader) GetAgentRun(context.Context, string) (AgentRun, error) {
	return f.agentRun, f.agentErr
}

func (f *fakeReader) AgentRunEvents(context.Context, string, uint64, uint32) (AgentEvents, error) {
	return f.agentEvents, f.agentErr
}

func (f *fakeReader) CancelAgentRun(context.Context, string) (AgentRun, error) {
	return f.agentRun, f.agentErr
}

func (f *fakeReader) ObserveSummary(context.Context, string) (ObserverSummary, error) {
	return f.observerSummary, f.observerErr
}

func (f *fakeReader) ObserverTail(_ context.Context, uuid string, maxBytes, maxLines int) (ObserverTail, error) {
	f.tailUUID, f.tailBytes, f.tailLines = uuid, maxBytes, maxLines
	return f.observerTail, f.observerErr
}

func TestAgentProfileListIsSortedAndOmitsCredentialProfileName(t *testing.T) {
	profiles := []agentprofile.Entry{
		{Profile: agentprofile.Profile{Name: "z", Provider: agentprofile.ProviderCodex, Command: []string{mcpTestProfile}, Credentials: agentprofile.CredentialsSection{Enable: true, Profile: "must-not-leak"}}, Digest: "sha256:z"},
		{Profile: agentprofile.Profile{Name: "a", Provider: agentprofile.ProviderCodex, Command: []string{mcpTestProfile}}, Digest: "sha256:a"},
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := NewWithProfiles(&fakeReader{}, profiles, mcpTestVersion)
	if err != nil {
		t.Fatal(err)
	}
	serverSession, err := server.Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: mcpTestVersion, Version: mcpTestVersion}, nil)
	session, err := client.Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	result := callTool(t, session, agentProfileListTool, map[string]any{})
	output := requireMap(t, result.StructuredContent)
	listed := requireSlice(t, output["profiles"])
	if requireMap(t, listed[0])["name"] != "a" || requireMap(t, listed[1])["name"] != "z" {
		t.Fatalf("profiles not sorted: %#v", listed)
	}
	if _, leaked := requireMap(t, listed[1])["profile"]; leaked {
		t.Fatalf("credential profile leaked: %#v", listed[1])
	}
	first := requireMap(t, callTool(t, session, agentProfileListTool, map[string]any{limitKey: 1}).StructuredContent)
	if first["truncated"] != true || first["next_cursor"] != "a" || len(requireSlice(t, first["profiles"])) != 1 {
		t.Fatalf("first profile page = %#v", first)
	}
	second := requireMap(t, callTool(t, session, agentProfileListTool, map[string]any{afterKey: "a", limitKey: 1}).StructuredContent)
	if second["truncated"] != false || len(requireSlice(t, second["profiles"])) != 1 || requireMap(t, requireSlice(t, second["profiles"])[0])["name"] != "z" {
		t.Fatalf("second profile page = %#v", second)
	}
}

func TestAgentStartResolvesTrustedProfile(t *testing.T) {
	entry := agentprofile.Entry{Profile: agentprofile.Profile{Name: mcpTestProfile, Provider: agentprofile.ProviderCodex, Command: []string{mcpTestProfile, "exec"}}, Digest: "sha256:profile"}
	reader := &fakeReader{agentRun: AgentRun{UUID: testRunValue, State: "AGENT_RUN_STATE_STARTING"}}
	session := connectClientProfiles(t, reader, []agentprofile.Entry{entry})
	result := requireMap(t, callTool(t, session, agentStartTool, map[string]any{requestIDKey: mcpTestRequestID, workspaceUUIDKey: mcpTestWorkspace, profileNameKey: mcpTestProfile, promptKey: mcpTestPrompt}).StructuredContent)
	if requireMap(t, result[testRunValue])[uuidKey] != testRunValue {
		t.Fatalf("start result = %#v", result)
	}
	if reader.requestedUUID != mcpTestWorkspace || reader.startedProfile.Digest != entry.Digest || reader.startedPrompt != mcpTestPrompt {
		t.Fatalf("resolved start = workspace=%q profile=%#v prompt=%q", reader.requestedUUID, reader.startedProfile, reader.startedPrompt)
	}
	for name, arguments := range map[string]map[string]any{
		"unknown profile": {requestIDKey: mcpTestRequestID, workspaceUUIDKey: mcpTestWorkspace, profileNameKey: mcpMissing, promptKey: mcpTestPrompt},
		"oversize prompt": {requestIDKey: mcpTestRequestID, workspaceUUIDKey: mcpTestWorkspace, profileNameKey: mcpTestProfile, promptKey: string(make([]byte, agentruncatalog.MaxPromptBytes+1))},
		"invalid request": {requestIDKey: "not-a-uuid", workspaceUUIDKey: mcpTestWorkspace, profileNameKey: mcpTestProfile, promptKey: mcpTestPrompt},
	} {
		t.Run(name, func(t *testing.T) {
			response, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: agentStartTool, Arguments: arguments})
			if err != nil || !response.IsError {
				t.Fatalf("CallTool = %#v, %v", response, err)
			}
		})
	}
	if response, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: agentStartTool, Arguments: map[string]any{profileNameKey: mcpTestProfile, promptKey: mcpTestPrompt}}); err == nil || response != nil {
		t.Fatalf("missing workspace = %#v, %v", response, err)
	}
}

func (f *fakeReader) GetNode(context.Context, string) (Node, error) {
	return f.operatorNode, f.operatorErr
}

func (f *fakeReader) AuthorizeNode(_ context.Context, fingerprint, label string) (Node, error) {
	f.operatorNode.Fingerprint = fingerprint
	f.operatorNode.Label = label
	f.operatorNode.Authorized = true
	return f.operatorNode, f.operatorErr
}

func (f *fakeReader) RevokeNode(_ context.Context, fingerprint string) (Node, error) {
	f.operatorNode.Fingerprint = fingerprint
	f.operatorNode.Authorized = false
	f.operatorNode.Connected = false
	return f.operatorNode, f.operatorErr
}

func (f *fakeReader) ListNodes(context.Context) ([]Node, error) {
	return append([]Node(nil), f.nodes...), f.listNodesErr
}

func (f *fakeReader) ListWorkspaces(context.Context) ([]Workspace, error) {
	f.listWorkCalled++
	return append([]Workspace(nil), f.workspaces...), f.listWorkErr
}

func (f *fakeReader) GetWorkspace(_ context.Context, uuid string) (Workspace, error) {
	f.requestedUUID = uuid
	return f.workspace, f.getWorkErr
}

func (f *fakeReader) ApplyWorkspace(_ context.Context, input WorkspaceApply) (WorkspaceApplyResult, error) {
	f.appliedWorkspace = input
	return f.applyResult, f.getWorkErr
}

func (f *fakeReader) ValidateWorkspace(_ context.Context, uuid string) (Workspace, error) {
	f.validatedUUID = uuid
	return f.workspace, f.getWorkErr
}

func (f *fakeReader) LaunchWorkspace(_ context.Context, uuid string) (Workspace, error) {
	f.launchedUUID = uuid
	return f.workspace, f.getWorkErr
}

func (f *fakeReader) StopWorkspace(_ context.Context, uuid string) (Workspace, error) {
	f.stoppedUUID = uuid
	return f.workspace, f.getWorkErr
}

func (f *fakeReader) WorkspaceStorageStatus(_ context.Context, uuid string) (daftclient.WorkspaceStorageStatus, error) {
	f.requestedUUID = uuid
	return f.storageStatus, f.storageErr
}

func (f *fakeReader) RejectWorkspaceChanges(_ context.Context, uuid string) ([]daftclient.ManifestTransition, error) {
	f.rejectedUUID = uuid
	return append([]daftclient.ManifestTransition(nil), f.rejectResult...), f.storageErr
}

func (f *fakeReader) UnfreezeWorkspace(_ context.Context, uuid string) (daftclient.WorkspaceStorageStatus, error) {
	f.unfrozenUUID = uuid
	return f.storageStatus, f.storageErr
}

func (f *fakeReader) DeleteWorkspace(_ context.Context, uuid string) error {
	f.deletedUUID = uuid
	return f.storageErr
}

func (f *fakeReader) PushWorkspace(_ context.Context, uuid, projectPath string) (daftclient.WorkspacePushResult, error) {
	f.pushedUUID, f.pushedPath = uuid, projectPath
	return f.pushResult, f.getWorkErr
}

func connectClient(t *testing.T, reader Reader) *mcp.ClientSession {
	return connectClientProfiles(t, reader, nil)
}

func connectClientProfiles(t *testing.T, reader Reader, profiles []agentprofile.Entry) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := NewWithProfiles(reader, profiles, mcpTestVersion)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	serverSession, err := server.Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: mcpTestVersion, Version: mcpTestVersion}, nil)
	clientSession, err := client.Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, arguments any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return result
}

func TestToolsAreExactlyTheBoundedReadSurface(t *testing.T) {
	session := connectClient(t, &fakeReader{})
	result, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	wantNames := []string{agentCancelTool, agentEventsTool, agentProfileListTool, agentResultTool, agentStartTool, agentStatusTool, nodeAuthorizeTool, nodeListTool, nodeStatusTool, nodeRevokeTool, observerSummaryTool, observerTailTool, workspaceApplyTool, workspaceRejectTool, workspaceDeleteTool, workspaceGetTool, workspaceLaunchTool, workspaceListTool, workspacePushTool, workspaceStatusTool, workspaceStopTool, workspaceStorageTool, workspaceUnfreezeTool, workspaceValidateTool}
	if len(result.Tools) != len(wantNames) {
		t.Fatalf("got %d tools, want %d", len(result.Tools), len(wantNames))
	}
	for i, tool := range result.Tools {
		if tool.Name != wantNames[i] {
			t.Errorf("tool %d name = %q, want %q", i, tool.Name, wantNames[i])
		}
		if tool.Annotations == nil || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Errorf("tool %q annotations = %#v, want closed-world", tool.Name, tool.Annotations)
		}
		if tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("tool %q is missing an inferred schema", tool.Name)
		}
		switch tool.Name {
		case nodeAuthorizeTool, workspaceApplyTool, workspaceLaunchTool, workspacePushTool, workspaceUnfreezeTool:
			if tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint || !tool.Annotations.IdempotentHint {
				t.Errorf("authorize annotations = %#v", tool.Annotations)
			}
		case nodeRevokeTool, agentCancelTool, workspaceStopTool, workspaceRejectTool, workspaceDeleteTool:
			if tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint || !tool.Annotations.IdempotentHint {
				t.Errorf("revoke annotations = %#v", tool.Annotations)
			}
		case agentStartTool:
			if tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint || !tool.Annotations.IdempotentHint {
				t.Errorf("agent start annotations = %#v", tool.Annotations)
			}
		default:
			if !tool.Annotations.ReadOnlyHint {
				t.Errorf("tool %q must be read-only", tool.Name)
			}
		}
	}
}

func TestNodeListSortsAndPaginatesOutput(t *testing.T) {
	reader := &fakeReader{nodes: make([]Node, maxResults+1)}
	for i := range reader.nodes {
		reader.nodes[i] = Node{Fingerprint: fmt.Sprintf("%03d", maxResults-i)}
	}
	session := connectClient(t, reader)
	result := callTool(t, session, nodeListTool, map[string]any{})
	output := requireMap(t, result.StructuredContent)
	nodes := requireSlice(t, output["nodes"])
	if len(nodes) != defaultResults || output["truncated"] != true || output["next_cursor"] != "049" {
		t.Fatalf("first page len=%d output=%#v", len(nodes), output)
	}
	if got := requireMap(t, nodes[0])["fingerprint"]; got != "000" {
		t.Errorf("first fingerprint = %v, want 000", got)
	}
	second := requireMap(t, callTool(t, session, nodeListTool, map[string]any{afterKey: "049", limitKey: maxResults}).StructuredContent)
	secondNodes := requireSlice(t, second["nodes"])
	if len(secondNodes) != 151 || second["truncated"] != false {
		t.Fatalf("second page len=%d output=%#v", len(secondNodes), second)
	}
	if got := requireMap(t, secondNodes[len(secondNodes)-1])["fingerprint"]; got != "200" {
		t.Errorf("last fingerprint = %v, want 200", got)
	}
}

func TestListPaginationRejectsUnboundedInputs(t *testing.T) {
	session := connectClient(t, &fakeReader{})
	for name, args := range map[string]map[string]any{
		"oversize_limit":  {limitKey: maxResults + 1},
		"oversize_cursor": {afterKey: string(make([]byte, maxCursorBytes+1))},
	} {
		t.Run(name, func(t *testing.T) {
			for _, tool := range []string{nodeListTool, agentProfileListTool} {
				result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
				if err != nil || !result.IsError {
					t.Fatalf("%s result = %#v, err = %v; want tool error", tool, result, err)
				}
			}
		})
	}
}

func TestWorkspaceReads(t *testing.T) {
	const first = "first"
	reader := &fakeReader{
		workspaces: []Workspace{{UUID: "b", Name: "second"}, {UUID: "a", Name: first}},
		workspace:  Workspace{UUID: "a", Name: first, Generation: 2},
	}
	session := connectClient(t, reader)
	listResult := callTool(t, session, workspaceListTool, map[string]any{})
	workspaces := requireSlice(t, requireMap(t, listResult.StructuredContent)["workspaces"])
	if got := requireMap(t, workspaces[0])[uuidKey]; got != "a" {
		t.Errorf("first workspace UUID = %v, want a", got)
	}
	const uuid = "123e4567-e89b-12d3-a456-426614174000"
	getResult := callTool(t, session, workspaceGetTool, map[string]any{uuidKey: uuid})
	if reader.requestedUUID != uuid {
		t.Errorf("requested UUID = %q, want %q", reader.requestedUUID, uuid)
	}
	workspace := requireMap(t, requireMap(t, getResult.StructuredContent)[mcpTestWorkspace])
	if workspace["name"] != first {
		t.Errorf("workspace name = %v, want %s", workspace["name"], first)
	}
}

func TestObserverSummaryIsStructuredAndOwnershipScoped(t *testing.T) {
	summary := progress.Summary{
		TakenAtUnixMs: 1234,
		Opensnoop:     &progress.Opensnoop{Total: 7, TopPaths: []progress.Counted{{Key: "/workspace/main.go", Count: 3}}},
	}
	session := connectClient(t, &fakeReader{observerSummary: ObserverSummary{Available: true, Summary: &summary}})
	output := requireMap(t, callTool(t, session, observerSummaryTool, map[string]any{uuidKey: mcpTestWorkspace}).StructuredContent)
	if output["available"] != true {
		t.Fatalf("observer summary = %#v", output)
	}
	structured := requireMap(t, output["summary"])
	if structured["taken_at_unix_ms"] != float64(1234) {
		t.Fatalf("structured summary = %#v", structured)
	}
	opensnoop := requireMap(t, structured["opensnoop"])
	if opensnoop["total"] != float64(7) {
		t.Fatalf("opensnoop summary = %#v", opensnoop)
	}

	empty := requireMap(t, callTool(t, connectClient(t, &fakeReader{}), observerSummaryTool, map[string]any{uuidKey: mcpTestWorkspace}).StructuredContent)
	if empty["available"] != false {
		t.Fatalf("empty summary = %#v", empty)
	}
	if result, err := connectClient(t, &fakeReader{}).CallTool(t.Context(), &mcp.CallToolParams{Name: observerSummaryTool, Arguments: map[string]any{}}); err == nil || result != nil {
		t.Fatalf("missing UUID result = %#v, err = %v; want schema error", result, err)
	}
	result, err := connectClient(t, &fakeReader{observerErr: errors.New("offline")}).CallTool(t.Context(), &mcp.CallToolParams{
		Name: observerSummaryTool, Arguments: map[string]any{uuidKey: mcpTestWorkspace},
	})
	if err != nil || !result.IsError {
		t.Fatalf("read error result = %#v, err = %v; want tool error", result, err)
	}
}

func TestWorkspaceLifecycleToolsApplyValidateAndProjectStatus(t *testing.T) {
	const workspaceID = "123e4567-e89b-12d3-a456-426614174000"
	workspace := Workspace{UUID: workspaceID, Name: mcpTestProject, Generation: 1, Ready: true, Missing: []string{}, Spec: WorkspaceSpec{ImageRef: mcpTestImage, Isolation: isolationStrict, Mounts: []WorkspaceMount{{Target: mcpTestMount, Source: workspaceID}}, Ports: []WorkspacePort{{ContainerPort: 8080, Protocol: protocolTCP}}}}
	projectPath := t.TempDir()
	reader := &fakeReader{
		workspace: workspace, applyResult: WorkspaceApplyResult{Workspace: workspace, Created: true, Changed: true},
		pushResult: daftclient.WorkspacePushResult{ProjectDir: projectPath, Mounts: []daftclient.WorkspacePushMountResult{{Remote: mcpTestMount, Entries: 2}}},
	}
	session := connectClient(t, reader)
	result := callTool(t, session, workspaceApplyTool, map[string]any{
		nameKey: mcpTestProject, "expected_generation": 0,
		specKey: map[string]any{"image_ref": mcpTestImage, "isolation": isolationStrict, "mounts": []any{map[string]any{"target": mcpTestMount}}, "ports": []any{map[string]any{"container_port": 8080, "protocol": protocolTCP}}},
	})
	applyOutput := requireMap(t, result.StructuredContent)
	if applyOutput["created"] != true || applyOutput["changed"] != true || reader.appliedWorkspace.Name != mcpTestProject || reader.appliedWorkspace.Spec.Ports[0].Protocol != protocolTCP {
		t.Fatalf("apply output=%#v input=%#v", applyOutput, reader.appliedWorkspace)
	}
	validated := callTool(t, session, workspaceValidateTool, map[string]any{uuidKey: workspaceID})
	if reader.validatedUUID != workspaceID || requireMap(t, requireMap(t, validated.StructuredContent)[mcpTestWorkspace])["ready"] != true {
		t.Fatalf("validated=%#v uuid=%q", validated.StructuredContent, reader.validatedUUID)
	}
	status := callTool(t, session, workspaceStatusTool, map[string]any{uuidKey: workspaceID})
	projected := requireMap(t, requireMap(t, status.StructuredContent)[mcpTestWorkspace])
	if requireMap(t, projected[specKey])["image_ref"] != mcpTestImage {
		t.Fatalf("status = %#v", projected)
	}
	launched := callTool(t, session, workspaceLaunchTool, map[string]any{uuidKey: workspaceID})
	stopped := callTool(t, session, workspaceStopTool, map[string]any{uuidKey: workspaceID})
	if reader.launchedUUID != workspaceID || reader.stoppedUUID != workspaceID ||
		requireMap(t, requireMap(t, launched.StructuredContent)[mcpTestWorkspace])[uuidKey] != workspaceID ||
		requireMap(t, requireMap(t, stopped.StructuredContent)[mcpTestWorkspace])[uuidKey] != workspaceID {
		t.Fatalf("launch/stop results: launched=%#v stopped=%#v reader=%#v", launched, stopped, reader)
	}
	pushed := requireMap(t, callTool(t, session, workspacePushTool, map[string]any{
		uuidKey: workspaceID, projectPathKey: projectPath,
	}).StructuredContent)
	if reader.pushedUUID != workspaceID || reader.pushedPath != projectPath ||
		pushed["project_path"] != projectPath || requireMap(t, requireSlice(t, pushed["mounts"])[0])["entries"] != float64(2) {
		t.Fatalf("push output=%#v reader=%#v", pushed, reader)
	}
}

func TestWorkspaceStorageLifecycleToolsAreBoundedAndExact(t *testing.T) {
	const workspaceID = "123e4567-e89b-12d3-a456-426614174000"
	reader := &fakeReader{
		storageStatus: daftclient.WorkspaceStorageStatus{
			WorkspaceUUID: workspaceID, DiskUsedBytes: 7, DiskFreeBytes: 13, DiskBytes: 20,
			DiskFrozen: true, DiskRunning: true, CASUsageBytes: 80, ProtectedBytes: 30,
			WorkspaceProtectedBytes: 11, HardBytes: 100, SoftBytes: 85, TargetBytes: 75,
			Manifests: []daftclient.ManifestTransition{{Workspace: workspaceID + "\x00/work", Digest: "current", Generation: 4, Present: true}},
		},
		rejectResult: []daftclient.ManifestTransition{{
			Workspace: workspaceID + "\x00/work", Digest: "restored", Generation: 3, Present: true,
		}},
	}
	session := connectClient(t, reader)
	status := requireMap(t, callTool(t, session, workspaceStorageTool, map[string]any{uuidKey: workspaceID}).StructuredContent)
	if status["disk_used_bytes"] != float64(7) || status["workspace_protected_bytes"] != float64(11) || status["protected_bytes"] != float64(30) || status["hard_bytes"] != float64(100) || len(requireSlice(t, status["manifests"])) != 1 {
		t.Fatalf("storage status = %#v", status)
	}
	rejected := requireMap(t, callTool(t, session, workspaceRejectTool, map[string]any{uuidKey: workspaceID}).StructuredContent)
	manifest := requireMap(t, requireSlice(t, rejected["manifests"])[0])
	if reader.rejectedUUID != workspaceID || manifest["manifest_digest"] != "restored" || manifest["generation"] != float64(3) {
		t.Fatalf("reject = %#v, uuid %q", rejected, reader.rejectedUUID)
	}
	reader.storageStatus.DiskFrozen = false
	unfrozen := requireMap(t, callTool(t, session, workspaceUnfreezeTool, map[string]any{uuidKey: workspaceID}).StructuredContent)
	if reader.unfrozenUUID != workspaceID || unfrozen["disk_frozen"] != false {
		t.Fatalf("unfreeze = %#v, uuid %q", unfrozen, reader.unfrozenUUID)
	}
	deleted := requireMap(t, callTool(t, session, workspaceDeleteTool, map[string]any{uuidKey: workspaceID}).StructuredContent)
	if reader.deletedUUID != workspaceID || deleted["deleted"] != true {
		t.Fatalf("delete = %#v, uuid %q", deleted, reader.deletedUUID)
	}
	for _, tool := range []string{workspaceStorageTool, workspaceRejectTool, workspaceUnfreezeTool, workspaceDeleteTool} {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: map[string]any{}})
		if err == nil || result != nil {
			t.Fatalf("%s empty UUID result=%#v err=%v", tool, result, err)
		}
		result, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: map[string]any{uuidKey: ""}})
		if err != nil || !result.IsError {
			t.Fatalf("%s blank UUID result=%#v err=%v", tool, result, err)
		}
	}
}

func TestWorkspaceApplyRejectsUnboundedAndAmbiguousInputs(t *testing.T) {
	session := connectClient(t, &fakeReader{})
	tests := []map[string]any{
		{},
		{"uuid": "id", nameKey: nameKey, "expected_generation": 1, specKey: map[string]any{}},
		{"uuid": "id", specKey: map[string]any{}},
		{nameKey: nameKey, specKey: map[string]any{"isolation": "unknown"}},
		{nameKey: nameKey, specKey: map[string]any{"ports": []any{map[string]any{"container_port": 80, "protocol": "sctp"}}}},
		{nameKey: nameKey, specKey: map[string]any{"image_ref": string(make([]byte, 1<<20))}},
	}
	for i, arguments := range tests {
		response, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: workspaceApplyTool, Arguments: arguments})
		if err == nil && (response == nil || !response.IsError) {
			t.Errorf("case %d response=%#v error=%v", i, response, err)
		}
	}
}

func TestWorkspacePushRequiresExplicitAbsoluteProjectPath(t *testing.T) {
	session := connectClient(t, &fakeReader{})
	for _, arguments := range []map[string]any{
		{},
		{uuidKey: mcpTestWorkspace, projectPathKey: "relative"},
		{uuidKey: mcpTestWorkspace, projectPathKey: "/" + string(make([]byte, 4096))},
	} {
		response, err := session.CallTool(t.Context(), &mcp.CallToolParams{
			Name: workspacePushTool, Arguments: arguments,
		})
		if err == nil && (response == nil || !response.IsError) {
			t.Fatalf("workspace_push(%#v) = %#v, %v", arguments, response, err)
		}
	}
	failed := connectClient(t, &fakeReader{getWorkErr: errors.New("push failed")})
	response, err := failed.CallTool(t.Context(), &mcp.CallToolParams{
		Name: workspacePushTool, Arguments: map[string]any{
			uuidKey: mcpTestWorkspace, projectPathKey: t.TempDir(),
		},
	})
	if err != nil || !response.IsError {
		t.Fatalf("workspace_push reader error = %#v, %v", response, err)
	}
}

func TestNodeMutationToolsValidateAndReturnStatus(t *testing.T) {
	id, _ := identity.Generate(identity.RoleSandboxHost)
	fingerprint := id.Fingerprint().String()
	reader := &fakeReader{operatorNode: Node{Connected: true}}
	session := connectClient(t, reader)
	authorized := callTool(t, session, nodeAuthorizeTool, map[string]any{fingerprintKey: fingerprint, "label": "builder"})
	node := requireMap(t, requireMap(t, authorized.StructuredContent)["node"])
	if node["authorized"] != true || node["label"] != "builder" {
		t.Fatalf("authorize output = %#v", node)
	}
	status := callTool(t, session, nodeStatusTool, map[string]any{fingerprintKey: fingerprint})
	if requireMap(t, requireMap(t, status.StructuredContent)["node"])["connected"] != true {
		t.Fatalf("status output = %#v", status.StructuredContent)
	}
	revoked := callTool(t, session, nodeRevokeTool, map[string]any{fingerprintKey: fingerprint})
	if requireMap(t, requireMap(t, revoked.StructuredContent)["node"])["authorized"] != false {
		t.Fatalf("revoke output = %#v", revoked.StructuredContent)
	}
	invalid, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: nodeStatusTool, Arguments: map[string]any{fingerprintKey: "bad"}})
	if err != nil || !invalid.IsError {
		t.Fatalf("invalid fingerprint = %#v, %v; want tool error", invalid, err)
	}
}

func TestObserverTailAppliesDefaultsAndBounds(t *testing.T) {
	reader := &fakeReader{observerTail: ObserverTail{Data: "sample", Bytes: 6, Lines: 1}}
	session := connectClient(t, reader)
	output := requireMap(t, callTool(t, session, observerTailTool, map[string]any{uuidKey: mcpTestWorkspace}).StructuredContent)
	if output["data"] != "sample" || reader.tailUUID != mcpTestWorkspace || reader.tailBytes != defaultTailBytes || reader.tailLines != defaultTailLines {
		t.Fatalf("observer tail = %#v; request=%q/%d/%d", output, reader.tailUUID, reader.tailBytes, reader.tailLines)
	}
	for name, args := range map[string]map[string]any{
		"bytes": {uuidKey: mcpTestWorkspace, "max_bytes": maxTailBytes + 1},
		"lines": {uuidKey: mcpTestWorkspace, "max_lines": maxTailLines + 1},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: observerTailTool, Arguments: args})
			if err != nil || !result.IsError {
				t.Fatalf("result = %#v, err = %v; want tool error", result, err)
			}
		})
	}
	result, err := connectClient(t, &fakeReader{observerErr: errors.New("offline")}).CallTool(t.Context(), &mcp.CallToolParams{
		Name: observerTailTool, Arguments: map[string]any{uuidKey: mcpTestWorkspace},
	})
	if err != nil || !result.IsError {
		t.Fatalf("read error result = %#v, err = %v; want tool error", result, err)
	}
}

func TestObserverTailRetention(t *testing.T) {
	var retained []byte
	truncated := false
	for _, chunk := range [][]byte{[]byte("one\n"), []byte("two\n"), []byte("three\n")} {
		retained, truncated = retainLastBytes(retained, chunk, 8, truncated)
	}
	tail := finishObserverTail(retained, 10, truncated)
	if tail.Data != "three" || tail.Bytes != 5 || tail.Lines != 1 || !tail.Truncated {
		t.Fatalf("byte-capped tail = %#v", tail)
	}

	retained, truncated = retainLastBytes(nil, []byte("one\ntwo\nthree\n"), 100, false)
	tail = finishObserverTail(retained, 2, truncated)
	if tail.Data != "two\nthree" || tail.Bytes != 9 || tail.Lines != 2 || !tail.Truncated {
		t.Fatalf("line-capped tail = %#v", tail)
	}

	retained, truncated = retainLastBytes(nil, []byte("long-fragment"), 4, false)
	tail = finishObserverTail(retained, 2, truncated)
	if tail.Data != "" || !tail.Truncated {
		t.Fatalf("partial-line tail = %#v", tail)
	}

	stream := &fakeObserverTailStream{chunks: [][]byte{[]byte("one\n"), []byte("two\n"), []byte("three\n")}}
	tail, err := drainObserverTail(stream, 100, 2)
	if err != nil || tail.Data != "two\nthree" {
		t.Fatalf("drainObserverTail = %#v, %v", tail, err)
	}
	wantErr := errors.New("stream failed")
	if _, err := drainObserverTail(&fakeObserverTailStream{err: wantErr}, 100, 2); !errors.Is(err, wantErr) {
		t.Fatalf("drainObserverTail error = %v, want %v", err, wantErr)
	}
}

func TestAgentRunToolsBoundContextAndSeparateResult(t *testing.T) {
	reader := &fakeReader{
		agentRun:    AgentRun{UUID: testRunValue, State: "AGENT_RUN_STATE_SUCCEEDED", Terminal: true, ExitCodeSet: true, Result: "final answer", LastEventCursor: 2},
		agentEvents: AgentEvents{Events: []AgentEvent{{Cursor: 2, State: "AGENT_RUN_STATE_SUCCEEDED", Message: doneValue}}, NextCursor: 2, LastEventCursor: 2, Terminal: true},
	}
	session := connectClient(t, reader)
	status := requireMap(t, callTool(t, session, agentStatusTool, map[string]any{uuidKey: testRunValue}).StructuredContent)
	run := requireMap(t, status[testRunValue])
	if _, leaked := run["result"]; leaked {
		t.Fatalf("status leaked result: %#v", run)
	}
	events := requireMap(t, callTool(t, session, agentEventsTool, map[string]any{uuidKey: testRunValue, "after_cursor": 1, "limit": 1}).StructuredContent)
	if events["next_cursor"] != float64(2) || len(requireSlice(t, events["events"])) != 1 {
		t.Fatalf("events = %#v", events)
	}
	result := requireMap(t, callTool(t, session, agentResultTool, map[string]any{uuidKey: testRunValue}).StructuredContent)
	if result["result"] != "final answer" {
		t.Fatalf("result = %#v", result)
	}
	canceled := requireMap(t, callTool(t, session, agentCancelTool, map[string]any{uuidKey: testRunValue}).StructuredContent)
	if requireMap(t, canceled[testRunValue])[uuidKey] != testRunValue {
		t.Fatalf("cancel = %#v", canceled)
	}

	reader.agentRun.Terminal = false
	nonterminal, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: agentResultTool, Arguments: map[string]any{uuidKey: testRunValue}})
	if err != nil || !nonterminal.IsError {
		t.Fatalf("nonterminal result = %#v, %v", nonterminal, err)
	}
	oversize, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: agentEventsTool, Arguments: map[string]any{uuidKey: testRunValue, "limit": maxResults + 1}})
	if err != nil || !oversize.IsError {
		t.Fatalf("oversize events = %#v, %v", oversize, err)
	}
}

func requireMap(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value has type %T, want map[string]any", value)
	}
	return result
}

func requireSlice(t *testing.T, value any) []any {
	t.Helper()
	result, ok := value.([]any)
	if !ok {
		t.Fatalf("value has type %T, want []any", value)
	}
	return result
}

func TestToolErrorsAreReturnedToTheMCPCaller(t *testing.T) {
	tests := []struct {
		name        string
		reader      *fakeReader
		args        any
		protocolErr bool
	}{
		{name: nodeListTool, reader: &fakeReader{listNodesErr: errors.New("nodes unavailable")}, args: map[string]any{}},
		{name: workspaceListTool, reader: &fakeReader{listWorkErr: errors.New("catalog unavailable")}, args: map[string]any{}},
		{name: workspaceGetTool, reader: &fakeReader{getWorkErr: errors.New("not found")}, args: map[string]any{uuidKey: mcpMissing}},
		{name: nodeStatusTool, reader: &fakeReader{operatorErr: errors.New("status unavailable")}, args: validFingerprintArgs(t)},
		{name: nodeAuthorizeTool, reader: &fakeReader{operatorErr: errors.New("authorize unavailable")}, args: validFingerprintArgs(t)},
		{name: nodeRevokeTool, reader: &fakeReader{operatorErr: errors.New("revoke unavailable")}, args: validFingerprintArgs(t)},
		{name: agentStatusTool, reader: &fakeReader{agentErr: errors.New("run unavailable")}, args: map[string]any{uuidKey: testRunValue}},
		{name: agentEventsTool, reader: &fakeReader{agentErr: errors.New("events unavailable")}, args: map[string]any{uuidKey: testRunValue}},
		{name: agentCancelTool, reader: &fakeReader{agentErr: errors.New("cancel unavailable")}, args: map[string]any{uuidKey: testRunValue}},
		{name: agentResultTool, reader: &fakeReader{agentErr: errors.New("result unavailable")}, args: map[string]any{uuidKey: testRunValue}},
		{name: workspaceGetTool, reader: &fakeReader{}, args: map[string]any{}, protocolErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := connectClient(t, test.reader)
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: test.name, Arguments: test.args})
			if test.protocolErr {
				if err == nil {
					t.Fatal("missing required argument did not return an MCP protocol error")
				}
				return
			}
			if err != nil {
				t.Fatalf("call tool: %v", err)
			}
			if !result.IsError {
				t.Fatalf("result = %#v, want tool error", result)
			}
		})
	}
}

func validFingerprintArgs(t *testing.T) map[string]any {
	t.Helper()
	id, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return map[string]any{fingerprintKey: id.Fingerprint().String()}
}

func TestMapWorkspacesHandlesCompleteAndMissingMessages(t *testing.T) {
	got := mapWorkspaces([]*workspacev1.Workspace{
		nil,
		{
			Metadata: &workspacev1.WorkspaceMetadata{Uuid: "id", Name: "name", Generation: 4},
			Status: &workspacev1.WorkspaceStatus{
				ObservedGeneration: 3,
				CurrentSandbox:     &sandboxv1.SandboxRef{Uid: "sandbox"},
			},
		},
	})
	if len(got) != 2 || got[0].UUID != "" || got[0].Spec.ImageRef != "" {
		t.Fatalf("mapped missing workspace = %#v", got)
	}
	if got[1].UUID != "id" || got[1].Name != "name" || got[1].Generation != 4 || got[1].ObservedGeneration != 3 || got[1].CurrentSandboxUUID != "sandbox" {
		t.Errorf("mapped workspace = %#v", got[1])
	}
}

func TestCheckedWorkspaceRejectsUnboundedExistingDeclarations(t *testing.T) {
	input := &workspacev1.Workspace{Metadata: &workspacev1.WorkspaceMetadata{Name: mcpTestProject}, Spec: &workspacev1.WorkspaceSpec{Env: make([]*sandboxv1.EnvVar, maxWorkspaceItems+1)}}
	if _, err := checkedWorkspace(input); err == nil {
		t.Fatal("oversized existing Workspace was projected into MCP output")
	}
	unboundedSpec := input
	input = &workspacev1.Workspace{
		Metadata: &workspacev1.WorkspaceMetadata{Name: string(make([]byte, 256))},
		Status:   &workspacev1.WorkspaceStatus{Missing: make([]string, maxWorkspaceItems+1)},
	}
	if _, err := checkedWorkspace(input); err == nil {
		t.Fatal("oversized Workspace metadata was projected into MCP output")
	}
	// List is deliberately summary-only and therefore remains bounded even if a
	// Workspace was created by an older or non-MCP client with a large spec.
	if got := mapWorkspaces([]*workspacev1.Workspace{unboundedSpec}); len(got) != 1 || got[0].Name != mcpTestProject || len(got[0].Spec.Env) != 0 {
		t.Fatalf("summary mapping = %#v", got)
	}
}

func TestMapWorkspaceBaseDerivesReadyCondition(t *testing.T) {
	input := &workspacev1.Workspace{Status: &workspacev1.WorkspaceStatus{Conditions: []*sandboxv1.Condition{
		{Type: "Other", Status: sandboxv1.ConditionStatus_CONDITION_STATUS_TRUE},
		{Type: conditionReady, Status: sandboxv1.ConditionStatus_CONDITION_STATUS_FALSE},
		{Type: conditionReady, Status: sandboxv1.ConditionStatus_CONDITION_STATUS_TRUE},
	}}}
	if got := mapWorkspaceBase(input); !got.Ready {
		t.Fatalf("Ready = false for conditions %#v", input.GetStatus().GetConditions())
	}
}

func TestWorkspaceSpecProjectionRoundTripsAllFields(t *testing.T) {
	input := WorkspaceSpec{
		ImageRef:              mcpTestImage,
		DevcontainerPath:      ".devcontainer/devcontainer.json",
		Mounts:                []WorkspaceMount{{Target: mcpTestMount, Source: "source", ReadOnly: true}},
		Env:                   []WorkspaceEnv{{Name: "MODE", Value: "test"}},
		Ports:                 []WorkspacePort{{ContainerPort: 53, Protocol: protocolUDP}, {ContainerPort: 443, Protocol: protocolTCP}},
		TargetNodeFingerprint: "sha256:node",
		GPUCount:              2,
		GPUMinVRAMBytes:       8 << 30,
		Isolation:             isolationShared,
		MemoryBytes:           16 << 30,
		CPUCores:              8,
		DiskBytes:             24 << 30,
		NestedVirtualization:  true,
		PrivilegedFuseMounts:  true,
		PrivilegedBuild:       true,
	}
	wire := workspaceSpecToProto(input)
	got := workspaceSpecFromProto(wire)
	if !reflect.DeepEqual(got, input) {
		t.Fatalf("round trip = %#v, want %#v", got, input)
	}
	input.Isolation = isolationStrict
	if got := workspaceSpecFromProto(workspaceSpecToProto(input)); got.Isolation != isolationStrict {
		t.Fatalf("strict isolation round trip = %#v", got)
	}
	if protocolName(sandboxv1.Protocol_PROTOCOL_UNSPECIFIED) != "" ||
		protocolFromName("invalid") != sandboxv1.Protocol_PROTOCOL_UNSPECIFIED {
		t.Fatal("unspecified protocol did not remain unspecified")
	}
}

func TestWorkspaceBoundsRejectEveryUnboundedDimension(t *testing.T) {
	tooManyMounts := make([]WorkspaceMount, maxWorkspaceItems+1)
	tooManyPorts := make([]WorkspacePort, maxWorkspaceItems+1)
	tooManyEnv := make([]WorkspaceEnv, maxWorkspaceItems+1)
	tests := []WorkspaceSpec{
		{Mounts: tooManyMounts},
		{Ports: tooManyPorts},
		{Env: tooManyEnv},
		{DevcontainerPath: string(make([]byte, 4097))},
		{TargetNodeFingerprint: string(make([]byte, 257))},
		{Env: []WorkspaceEnv{{Name: "VALUE", Value: string(make([]byte, maxWorkspaceBytes+1))}}},
	}
	for i, input := range tests {
		if err := validateWorkspaceSpecBounds(input); err == nil {
			t.Errorf("case %d accepted an unbounded declaration", i)
		}
	}
	if err := validateWorkspaceApply(workspaceApplyInput{Name: string(make([]byte, 256))}); err == nil {
		t.Fatal("oversized workspace name accepted")
	}
}

type fakeNodeLister struct {
	response *nodectrlv1.ListNodesResponse
	err      error
}

func (f fakeNodeLister) ListNodes(context.Context, *connect.Request[nodectrlv1.ListNodesRequest]) (*connect.Response[nodectrlv1.ListNodesResponse], error) {
	return connect.NewResponse(f.response), f.err
}

type fakeWorkspaceReader struct {
	listResponse  *workspacev1.ListWorkspacesResponse
	workspace     *workspacev1.Workspace
	applyResponse *workspacev1.ApplyWorkspaceResponse
	applyRequest  *workspacev1.ApplyWorkspaceRequest
	getUUID       string
	validateUUID  string
	launchUUID    string
	stopUUID      string
	observeUUID   string
	summary       []byte
	err           error
	mineOnly      bool
}

type fakeOperatorClient struct {
	status *operatorv1.NodeStatus
	err    error
}

type fakeAgentRunClient struct {
	run          *agentrunv1.AgentRun
	events       *agentrunv1.AgentRunEventsResponse
	err          error
	startErr     error
	getErr       error
	captureStart func(*agentrunv1.StartAgentRunRequest)
}

type fakeSecretClient struct {
	request *secretdeliveryv1.DeliverSecretRequest
	err     error
}

func (f *fakeSecretClient) Deliver(_ context.Context, request *connect.Request[secretdeliveryv1.DeliverSecretRequest]) (*connect.Response[secretdeliveryv1.DeliverSecretResponse], error) {
	f.request = request.Msg
	return connect.NewResponse(&secretdeliveryv1.DeliverSecretResponse{Installed: true}), f.err
}

func testCredentialCatalog(t *testing.T) *agentcredential.Catalog {
	t.Helper()
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth.json")
	catalog := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(auth, []byte(`{"token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog, []byte(`{"version":1,"credentials":[{"name":"host-only","provider":"codex","source":{"file":"auth.json"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := agentcredential.Load(catalog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Destroy)
	return c
}

func (f fakeAgentRunClient) Get(context.Context, *connect.Request[agentrunv1.GetAgentRunRequest]) (*connect.Response[agentrunv1.AgentRun], error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return connect.NewResponse(f.run), f.err
}

func (f fakeAgentRunClient) Start(_ context.Context, request *connect.Request[agentrunv1.StartAgentRunRequest]) (*connect.Response[agentrunv1.AgentRun], error) {
	if f.captureStart != nil {
		f.captureStart(request.Msg)
	}
	if f.startErr != nil {
		return nil, f.startErr
	}
	return connect.NewResponse(f.run), f.err
}

func (f fakeAgentRunClient) Events(context.Context, *connect.Request[agentrunv1.AgentRunEventsRequest]) (*connect.Response[agentrunv1.AgentRunEventsResponse], error) {
	return connect.NewResponse(f.events), f.err
}

func (f fakeAgentRunClient) Cancel(context.Context, *connect.Request[agentrunv1.CancelAgentRunRequest]) (*connect.Response[agentrunv1.AgentRun], error) {
	return connect.NewResponse(f.run), f.err
}

func (f fakeOperatorClient) AuthorizeNode(context.Context, *connect.Request[operatorv1.AuthorizeNodeRequest]) (*connect.Response[operatorv1.NodeStatus], error) {
	return connect.NewResponse(f.status), f.err
}

func (f fakeOperatorClient) RevokeNode(context.Context, *connect.Request[operatorv1.RevokeNodeRequest]) (*connect.Response[operatorv1.NodeStatus], error) {
	return connect.NewResponse(f.status), f.err
}

func (f fakeOperatorClient) GetNode(context.Context, *connect.Request[operatorv1.GetNodeRequest]) (*connect.Response[operatorv1.NodeStatus], error) {
	return connect.NewResponse(f.status), f.err
}

func (f *fakeWorkspaceReader) List(_ context.Context, request *connect.Request[workspacev1.ListWorkspacesRequest]) (*connect.Response[workspacev1.ListWorkspacesResponse], error) {
	f.mineOnly = request.Msg.GetMineOnly()
	return connect.NewResponse(f.listResponse), f.err
}

func (f *fakeWorkspaceReader) Get(_ context.Context, request *connect.Request[workspacev1.GetWorkspaceRequest]) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	f.getUUID = request.Msg.GetHandle().GetUuid()
	return connect.NewResponse(&workspacev1.WorkspaceResponse{Workspace: f.workspace}), f.err
}

func (f *fakeWorkspaceReader) Apply(_ context.Context, request *connect.Request[workspacev1.ApplyWorkspaceRequest]) (*connect.Response[workspacev1.ApplyWorkspaceResponse], error) {
	f.applyRequest = request.Msg
	return connect.NewResponse(f.applyResponse), f.err
}

func (f *fakeWorkspaceReader) Validate(_ context.Context, request *connect.Request[workspacev1.ValidateRequest]) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	f.validateUUID = request.Msg.GetHandle().GetUuid()
	return connect.NewResponse(&workspacev1.WorkspaceResponse{Workspace: f.workspace}), f.err
}

func (f *fakeWorkspaceReader) Launch(_ context.Context, request *connect.Request[workspacev1.LaunchRequest]) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	f.launchUUID = request.Msg.GetHandle().GetUuid()
	return connect.NewResponse(&workspacev1.WorkspaceResponse{Workspace: f.workspace}), f.err
}

func (f *fakeWorkspaceReader) Stop(_ context.Context, request *connect.Request[workspacev1.StopRequest]) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	f.stopUUID = request.Msg.GetHandle().GetUuid()
	return connect.NewResponse(&workspacev1.WorkspaceResponse{Workspace: f.workspace}), f.err
}

func (f *fakeWorkspaceReader) ObserveSummary(_ context.Context, request *connect.Request[workspacev1.ObserveSummaryRequest]) (*connect.Response[workspacev1.ObserveSummaryResponse], error) {
	f.observeUUID = request.Msg.GetWorkspace().GetUuid()
	return connect.NewResponse(&workspacev1.ObserveSummaryResponse{Summary: f.summary}), f.err
}

func (f *fakeWorkspaceReader) ObserverTail(context.Context, *connect.Request[workspacev1.ObserverTailRequest]) (*connect.ServerStreamForClient[workspacev1.ObserverChunk], error) {
	return nil, f.err
}

func TestConnectReaderMapsRPCsAndForcesOwnership(t *testing.T) {
	protoWorkspace := &workspacev1.Workspace{Metadata: &workspacev1.WorkspaceMetadata{Uuid: mcpTestWorkspace}}
	summaryData, err := progress.Marshal(progress.Summary{TakenAtUnixMs: 42})
	if err != nil {
		t.Fatal(err)
	}
	workspaces := &fakeWorkspaceReader{
		listResponse:  &workspacev1.ListWorkspacesResponse{Workspaces: []*workspacev1.Workspace{protoWorkspace}},
		workspace:     protoWorkspace,
		applyResponse: &workspacev1.ApplyWorkspaceResponse{Workspace: protoWorkspace, Created: true, Changed: true},
		summary:       summaryData,
	}
	var startRequest *agentrunv1.StartAgentRunRequest
	secretClient := &fakeSecretClient{}
	reader := ConnectReader{
		nodes: fakeNodeLister{response: &nodectrlv1.ListNodesResponse{Nodes: []*nodectrlv1.Node{{
			Fingerprint: "fingerprint",
			Hostname:    "node",
			Version:     "1.2.3",
			Resources:   &nodectrlv1.HostResources{Cpus: 8, MemoryBytes: 16},
		}}}},
		workspace: workspaces,
		operator:  fakeOperatorClient{status: &operatorv1.NodeStatus{Fingerprint: "fingerprint", Authorized: true}},
		agentRuns: fakeAgentRunClient{run: &agentrunv1.AgentRun{Uuid: testRunValue, State: agentrunv1.AgentRunState_AGENT_RUN_STATE_SUCCEEDED, Result: doneValue}, events: &agentrunv1.AgentRunEventsResponse{Events: []*agentrunv1.AgentRunEvent{{Cursor: 1}}, NextCursor: 1}, captureStart: func(request *agentrunv1.StartAgentRunRequest) { startRequest = request }},
		secrets:   secretClient, credentials: testCredentialCatalog(t),
	}
	nodes, err := reader.ListNodes(t.Context())
	if err != nil || len(nodes) != 1 || nodes[0].CPUs != 8 || nodes[0].MemoryBytes != 16 {
		t.Fatalf("ListNodes() = %#v, %v", nodes, err)
	}
	testConnectReaderWorkspaceRPCs(t, reader, workspaces)
	status, err := reader.GetNode(t.Context(), "fingerprint")
	if err != nil || !status.Authorized {
		t.Fatalf("GetNode() = %#v, %v", status, err)
	}
	if _, err := reader.AuthorizeNode(t.Context(), "fingerprint", "node"); err != nil {
		t.Fatalf("AuthorizeNode: %v", err)
	}
	if _, err := reader.RevokeNode(t.Context(), "fingerprint"); err != nil {
		t.Fatalf("RevokeNode: %v", err)
	}
	if run, err := reader.GetAgentRun(t.Context(), testRunValue); err != nil || !run.Terminal || run.Result != doneValue {
		t.Fatalf("GetAgentRun = %#v, %v", run, err)
	}
	if events, err := reader.AgentRunEvents(t.Context(), testRunValue, 0, 1); err != nil || events.NextCursor != 1 || len(events.Events) != 1 {
		t.Fatalf("AgentRunEvents = %#v, %v", events, err)
	}
	if _, err := reader.CancelAgentRun(t.Context(), testRunValue); err != nil {
		t.Fatalf("CancelAgentRun = %v", err)
	}
	if summary, err := reader.ObserveSummary(t.Context(), mcpTestWorkspace); err != nil || !summary.Available || summary.Summary.TakenAtUnixMs != 42 || workspaces.observeUUID != mcpTestWorkspace {
		t.Fatalf("ObserveSummary = %#v, %v; request=%q", summary, err, workspaces.observeUUID)
	}
	profile := agentprofile.Entry{Profile: agentprofile.Profile{
		Name: mcpTestProfile, Provider: agentprofile.ProviderCodex, Command: []string{mcpTestProfile, "exec"},
		Image:       "example.com/agent@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Credentials: agentprofile.CredentialsSection{Enable: true, Profile: "host-only"},
		Resources:   agentprofile.ResourcesSection{Enable: true, CPUCores: 4, MemoryBytes: 8 << 30},
		Egress:      agentprofile.EgressSection{Enable: true},
	}, Digest: "sha256:profile"}
	if _, err := reader.StartAgentRun(t.Context(), mcpTestRequestID, mcpTestWorkspace, profile, mcpTestPrompt); err != nil {
		t.Fatalf("StartAgentRun = %v", err)
	}
	if startRequest.GetRequestId() != mcpTestRequestID || startRequest.GetWorkspaceUuid() != mcpTestWorkspace || startRequest.GetProfileDigest() != profile.Digest || len(startRequest.GetCommand()) != 2 || startRequest.GetPrompt() != mcpTestPrompt {
		t.Fatalf("Start request = %#v", startRequest)
	}
	if startRequest.GetImageRef() != profile.Profile.Image || !startRequest.GetResources().GetEnable() || startRequest.GetResources().GetCpuCores() != 4 || startRequest.GetResources().GetMemoryBytes() != 8<<30 || !startRequest.GetEgress().GetEnable() {
		t.Fatalf("Start policy = %#v", startRequest)
	}
	if !startRequest.GetCredentialRequired() || startRequest.GetCredentialProvider() != "codex" {
		t.Fatalf("Start credential marker = %#v", startRequest)
	}
	if secretClient.request != nil {
		t.Fatalf("credential redelivered for completed run: %+v", secretClient.request)
	}
}

func TestConnectReaderObserveSummaryEdgeCases(t *testing.T) {
	workspaces := &fakeWorkspaceReader{}
	reader := ConnectReader{workspace: workspaces}
	summary, err := reader.ObserveSummary(t.Context(), mcpTestWorkspace)
	if err != nil || summary.Available || summary.Summary != nil {
		t.Fatalf("empty ObserveSummary = %#v, %v", summary, err)
	}

	workspaces.summary = []byte("{not-json")
	if _, err := reader.ObserveSummary(t.Context(), mcpTestWorkspace); err == nil {
		t.Fatal("malformed ObserveSummary succeeded")
	}

	wantErr := errors.New("offline")
	workspaces.err = wantErr
	if _, err := reader.ObserveSummary(t.Context(), mcpTestWorkspace); !errors.Is(err, wantErr) {
		t.Fatalf("ObserveSummary error = %v, want %v", err, wantErr)
	}
	if _, err := reader.ObserverTail(t.Context(), mcpTestWorkspace, 100, 2); !errors.Is(err, wantErr) {
		t.Fatalf("ObserverTail error = %v, want %v", err, wantErr)
	}
	workspaces.err = nil
	if _, err := reader.ObserverTail(t.Context(), mcpTestWorkspace, 100, 2); err == nil {
		t.Fatal("nil ObserverTail stream succeeded")
	}
}

func testConnectReaderWorkspaceRPCs(t *testing.T, reader ConnectReader, workspaces *fakeWorkspaceReader) {
	t.Helper()
	listed, err := reader.ListWorkspaces(t.Context())
	if err != nil || len(listed) != 1 || !workspaces.mineOnly {
		t.Fatalf("ListWorkspaces() = %#v, %v; mine_only=%v", listed, err, workspaces.mineOnly)
	}
	got, err := reader.GetWorkspace(t.Context(), mcpTestWorkspace)
	if err != nil || got.UUID != mcpTestWorkspace || workspaces.getUUID != mcpTestWorkspace {
		t.Fatalf("GetWorkspace() = %#v, %v; request=%q", got, err, workspaces.getUUID)
	}
	if validated, err := reader.ValidateWorkspace(t.Context(), mcpTestWorkspace); err != nil || validated.UUID != mcpTestWorkspace || workspaces.validateUUID != mcpTestWorkspace {
		t.Fatalf("ValidateWorkspace = %#v, %v", validated, err)
	}
	if launched, err := reader.LaunchWorkspace(t.Context(), mcpTestWorkspace); err != nil || launched.UUID != mcpTestWorkspace || workspaces.launchUUID != mcpTestWorkspace {
		t.Fatalf("LaunchWorkspace = %#v, %v", launched, err)
	}
	if stopped, err := reader.StopWorkspace(t.Context(), mcpTestWorkspace); err != nil || stopped.UUID != mcpTestWorkspace || workspaces.stopUUID != mcpTestWorkspace {
		t.Fatalf("StopWorkspace = %#v, %v", stopped, err)
	}
	applied, err := reader.ApplyWorkspace(t.Context(), WorkspaceApply{Name: mcpTestProject, Spec: WorkspaceSpec{Isolation: isolationStrict, Ports: []WorkspacePort{{ContainerPort: 80, Protocol: protocolTCP}}}})
	if err != nil || !applied.Created || !applied.Changed || workspaces.applyRequest.GetHandle().GetName() != mcpTestProject || workspaces.applyRequest.GetSpec().GetIsolation() != sandboxv1.Isolation_ISOLATION_STRICT || workspaces.applyRequest.GetSpec().GetPorts()[0].GetProtocol() != sandboxv1.Protocol_PROTOCOL_TCP {
		t.Fatalf("ApplyWorkspace = %#v request=%#v error=%v", applied, workspaces.applyRequest, err)
	}
}

func TestConnectReaderPropagatesRPCErrors(t *testing.T) {
	wantErr := errors.New("rpc failed")
	reader := ConnectReader{
		nodes:     fakeNodeLister{response: &nodectrlv1.ListNodesResponse{}, err: wantErr},
		workspace: &fakeWorkspaceReader{listResponse: &workspacev1.ListWorkspacesResponse{}, err: wantErr},
		operator:  fakeOperatorClient{status: &operatorv1.NodeStatus{}, err: wantErr},
		agentRuns: fakeAgentRunClient{run: &agentrunv1.AgentRun{}, events: &agentrunv1.AgentRunEventsResponse{}, err: wantErr},
	}
	if _, err := reader.ListNodes(t.Context()); !errors.Is(err, wantErr) {
		t.Errorf("ListNodes error = %v", err)
	}
	if _, err := reader.ListWorkspaces(t.Context()); !errors.Is(err, wantErr) {
		t.Errorf("ListWorkspaces error = %v", err)
	}
	if _, err := reader.GetWorkspace(t.Context(), mcpTestWorkspace); !errors.Is(err, wantErr) {
		t.Errorf("GetWorkspace error = %v", err)
	}
	if _, err := reader.ValidateWorkspace(t.Context(), mcpTestWorkspace); !errors.Is(err, wantErr) {
		t.Errorf("ValidateWorkspace error = %v", err)
	}
	if _, err := reader.LaunchWorkspace(t.Context(), mcpTestWorkspace); !errors.Is(err, wantErr) {
		t.Errorf("LaunchWorkspace error = %v", err)
	}
	if _, err := reader.StopWorkspace(t.Context(), mcpTestWorkspace); !errors.Is(err, wantErr) {
		t.Errorf("StopWorkspace error = %v", err)
	}
	if _, err := reader.ApplyWorkspace(t.Context(), WorkspaceApply{Name: mcpTestProject}); !errors.Is(err, wantErr) {
		t.Errorf("ApplyWorkspace error = %v", err)
	}
	if _, err := reader.GetNode(t.Context(), "fingerprint"); !errors.Is(err, wantErr) {
		t.Errorf("GetNode error = %v", err)
	}
	if _, err := reader.AuthorizeNode(t.Context(), "fingerprint", "node"); !errors.Is(err, wantErr) {
		t.Errorf("AuthorizeNode error = %v", err)
	}
	if _, err := reader.RevokeNode(t.Context(), "fingerprint"); !errors.Is(err, wantErr) {
		t.Errorf("RevokeNode error = %v", err)
	}
	if _, err := reader.GetAgentRun(t.Context(), testRunValue); !errors.Is(err, wantErr) {
		t.Errorf("GetAgentRun error = %v", err)
	}
	if _, err := reader.StartAgentRun(t.Context(), mcpTestRequestID, mcpTestWorkspace, agentprofile.Entry{}, "prompt"); !errors.Is(err, wantErr) {
		t.Errorf("StartAgentRun error = %v", err)
	}
	if _, err := reader.AgentRunEvents(t.Context(), testRunValue, 0, 1); !errors.Is(err, wantErr) {
		t.Errorf("AgentRunEvents error = %v", err)
	}
	if _, err := reader.CancelAgentRun(t.Context(), testRunValue); !errors.Is(err, wantErr) {
		t.Errorf("CancelAgentRun error = %v", err)
	}
}

func TestConnectReaderCredentialDeliveryFailures(t *testing.T) {
	profile := agentprofile.Entry{Profile: agentprofile.Profile{Provider: agentprofile.ProviderCodex, Credentials: agentprofile.CredentialsSection{Enable: true, Profile: "host-only"}}}
	run := &agentrunv1.AgentRun{Uuid: testRunValue, State: agentrunv1.AgentRunState_AGENT_RUN_STATE_AWAITING_CREDENTIAL}
	if _, err := (ConnectReader{agentRuns: fakeAgentRunClient{run: run}}).StartAgentRun(t.Context(), mcpTestRequestID, "w", profile, ""); err == nil {
		t.Fatal("missing credential catalog succeeded")
	}
	missing := profile
	missing.Profile.Credentials.Profile = mcpMissing
	if _, err := (ConnectReader{agentRuns: fakeAgentRunClient{run: run}, credentials: testCredentialCatalog(t), secrets: &fakeSecretClient{}}).StartAgentRun(t.Context(), mcpTestRequestID, "w", missing, ""); err == nil {
		t.Fatal("unknown credential succeeded")
	}
	want := errors.New("deliver")
	if _, err := (ConnectReader{agentRuns: fakeAgentRunClient{run: run}, credentials: testCredentialCatalog(t), secrets: &fakeSecretClient{err: want}}).StartAgentRun(t.Context(), mcpTestRequestID, "w", profile, ""); !errors.Is(err, want) {
		t.Fatalf("delivery error=%v", err)
	}
	want = errors.New("refresh")
	if _, err := (ConnectReader{agentRuns: fakeAgentRunClient{run: run, getErr: want}, credentials: testCredentialCatalog(t), secrets: &fakeSecretClient{}}).StartAgentRun(t.Context(), mcpTestRequestID, "w", profile, ""); !errors.Is(err, want) {
		t.Fatalf("refresh error=%v", err)
	}
}

func TestConstructorsRejectMissingDependencies(t *testing.T) {
	if _, err := New(nil, "test"); err == nil {
		t.Error("New accepted a nil reader")
	}
	if _, err := New(&fakeReader{}, ""); err == nil {
		t.Error("New accepted an empty version")
	}
	if _, err := NewConnectReader(nil); err == nil {
		t.Error("NewConnectReader accepted a nil client")
	}
	if _, err := NewConnectReader(&daftclient.Client{}); err == nil {
		t.Error("NewConnectReader accepted a client without services")
	}
}

func TestConnectReaderPushesExplicitEmptyProject(t *testing.T) {
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".daft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".daft", "config.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := daftclient.Bootstrap(daftclient.Config{
		Orchestrator: "https://example.test", ConfigDir: t.TempDir(), ProjectDir: project,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	reader, err := NewConnectReader(client)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reader.PushWorkspace(t.Context(), mcpTestWorkspace, project)
	if err != nil || result.ProjectDir != project {
		t.Fatalf("PushWorkspace = %#v, %v", result, err)
	}
}
