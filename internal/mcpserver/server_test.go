package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richiejp/darkaifactory/internal/agentprofile"
	"github.com/richiejp/darkaifactory/internal/daftclient"
	agentrunv1 "github.com/richiejp/darkaifactory/internal/genpb/agentrun/v1"
	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	operatorv1 "github.com/richiejp/darkaifactory/internal/genpb/operator/v1"
	sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
)

const (
	fingerprintKey = "fingerprint"
	mcpTestVersion = "test"
	uuidKey        = "uuid"
	testRunValue   = "run"
	doneValue      = "done"
)

type fakeReader struct {
	nodes          []Node
	workspaces     []Workspace
	workspace      Workspace
	listNodesErr   error
	listWorkErr    error
	getWorkErr     error
	requestedUUID  string
	listWorkCalled int
	operatorNode   Node
	operatorErr    error
	agentRun       AgentRun
	agentEvents    AgentEvents
	agentErr       error
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

func TestAgentProfileListIsSortedAndOmitsCredentialProfileName(t *testing.T) {
	profiles := []agentprofile.Entry{
		{Profile: agentprofile.Profile{Name: "z", Provider: agentprofile.ProviderCodex, Command: []string{"codex"}, Credentials: agentprofile.CredentialsSection{Enable: true, Profile: "must-not-leak"}}, Digest: "sha256:z"},
		{Profile: agentprofile.Profile{Name: "a", Provider: agentprofile.ProviderCodex, Command: []string{"codex"}}, Digest: "sha256:a"},
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
	listed := requireSlice(t, requireMap(t, result.StructuredContent)["profiles"])
	if requireMap(t, listed[0])["name"] != "a" || requireMap(t, listed[1])["name"] != "z" {
		t.Fatalf("profiles not sorted: %#v", listed)
	}
	if _, leaked := requireMap(t, listed[1])["profile"]; leaked {
		t.Fatalf("credential profile leaked: %#v", listed[1])
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

func connectClient(t *testing.T, reader Reader) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server, err := New(reader, mcpTestVersion)
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
	wantNames := []string{agentCancelTool, agentEventsTool, agentProfileListTool, agentResultTool, agentStatusTool, nodeAuthorizeTool, nodeListTool, nodeStatusTool, nodeRevokeTool, workspaceGetTool, workspaceListTool}
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
		case nodeAuthorizeTool:
			if tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint || !tool.Annotations.IdempotentHint {
				t.Errorf("authorize annotations = %#v", tool.Annotations)
			}
		case nodeRevokeTool, agentCancelTool:
			if tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || !*tool.Annotations.DestructiveHint || !tool.Annotations.IdempotentHint {
				t.Errorf("revoke annotations = %#v", tool.Annotations)
			}
		default:
			if !tool.Annotations.ReadOnlyHint {
				t.Errorf("tool %q must be read-only", tool.Name)
			}
		}
	}
}

func TestNodeListSortsAndCapsOutput(t *testing.T) {
	reader := &fakeReader{nodes: make([]Node, maxResults+1)}
	for i := range reader.nodes {
		reader.nodes[i] = Node{Fingerprint: fmt.Sprintf("%03d", maxResults-i)}
	}
	result := callTool(t, connectClient(t, reader), nodeListTool, map[string]any{})
	output := requireMap(t, result.StructuredContent)
	nodes := requireSlice(t, output["nodes"])
	if len(nodes) != maxResults || output["truncated"] != true {
		t.Fatalf("got len=%d truncated=%v", len(nodes), output["truncated"])
	}
	if got := requireMap(t, nodes[0])["fingerprint"]; got != "000" {
		t.Errorf("first fingerprint = %v, want 000", got)
	}
	if got := requireMap(t, nodes[len(nodes)-1])["fingerprint"]; got != "199" {
		t.Errorf("last fingerprint = %v, want 199", got)
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
	workspace := requireMap(t, requireMap(t, getResult.StructuredContent)["workspace"])
	if workspace["name"] != first {
		t.Errorf("workspace name = %v, want %s", workspace["name"], first)
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
		{name: workspaceGetTool, reader: &fakeReader{getWorkErr: errors.New("not found")}, args: map[string]any{uuidKey: "missing"}},
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
	if len(got) != 2 || got[0] != (Workspace{}) {
		t.Fatalf("mapped missing workspace = %#v", got)
	}
	want := Workspace{UUID: "id", Name: "name", Generation: 4, ObservedGeneration: 3, CurrentSandboxUUID: "sandbox"}
	if got[1] != want {
		t.Errorf("mapped workspace = %#v, want %#v", got[1], want)
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
	listResponse *workspacev1.ListWorkspacesResponse
	err          error
	mineOnly     bool
}

type fakeOperatorClient struct {
	status *operatorv1.NodeStatus
	err    error
}

type fakeAgentRunClient struct {
	run    *agentrunv1.AgentRun
	events *agentrunv1.AgentRunEventsResponse
	err    error
}

func (f fakeAgentRunClient) Get(context.Context, *connect.Request[agentrunv1.GetAgentRunRequest]) (*connect.Response[agentrunv1.AgentRun], error) {
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

func TestConnectReaderMapsRPCsAndForcesOwnership(t *testing.T) {
	protoWorkspace := &workspacev1.Workspace{Metadata: &workspacev1.WorkspaceMetadata{Uuid: "workspace"}}
	workspaces := &fakeWorkspaceReader{
		listResponse: &workspacev1.ListWorkspacesResponse{Workspaces: []*workspacev1.Workspace{protoWorkspace}},
	}
	reader := ConnectReader{
		nodes: fakeNodeLister{response: &nodectrlv1.ListNodesResponse{Nodes: []*nodectrlv1.Node{{
			Fingerprint: "fingerprint",
			Hostname:    "node",
			Version:     "1.2.3",
			Resources:   &nodectrlv1.HostResources{Cpus: 8, MemoryBytes: 16},
		}}}},
		workspace: workspaces,
		operator:  fakeOperatorClient{status: &operatorv1.NodeStatus{Fingerprint: "fingerprint", Authorized: true}},
		agentRuns: fakeAgentRunClient{run: &agentrunv1.AgentRun{Uuid: testRunValue, State: agentrunv1.AgentRunState_AGENT_RUN_STATE_SUCCEEDED, Result: doneValue}, events: &agentrunv1.AgentRunEventsResponse{Events: []*agentrunv1.AgentRunEvent{{Cursor: 1}}, NextCursor: 1}},
	}
	nodes, err := reader.ListNodes(t.Context())
	if err != nil || len(nodes) != 1 || nodes[0].CPUs != 8 || nodes[0].MemoryBytes != 16 {
		t.Fatalf("ListNodes() = %#v, %v", nodes, err)
	}
	listed, err := reader.ListWorkspaces(t.Context())
	if err != nil || len(listed) != 1 || !workspaces.mineOnly {
		t.Fatalf("ListWorkspaces() = %#v, %v; mine_only=%v", listed, err, workspaces.mineOnly)
	}
	got, err := reader.GetWorkspace(t.Context(), "workspace")
	if err != nil || got.UUID != "workspace" {
		t.Fatalf("GetWorkspace() = %#v, %v", got, err)
	}
	if _, err := reader.GetWorkspace(t.Context(), "someone-elses-workspace"); err == nil {
		t.Fatal("GetWorkspace returned a workspace outside the owned list")
	}
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
	if _, err := reader.GetWorkspace(t.Context(), "workspace"); !errors.Is(err, wantErr) {
		t.Errorf("GetWorkspace error = %v", err)
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
	if _, err := reader.AgentRunEvents(t.Context(), testRunValue, 0, 1); !errors.Is(err, wantErr) {
		t.Errorf("AgentRunEvents error = %v", err)
	}
	if _, err := reader.CancelAgentRun(t.Context(), testRunValue); !errors.Is(err, wantErr) {
		t.Errorf("CancelAgentRun error = %v", err)
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
