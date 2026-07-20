package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richiejp/darkaifactory/internal/daftclient"
	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
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
	server, err := New(reader, "test")
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	serverSession, err := server.Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
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
	wantNames := []string{nodeListTool, workspaceGetTool, workspaceListTool}
	if len(result.Tools) != len(wantNames) {
		t.Fatalf("got %d tools, want %d", len(result.Tools), len(wantNames))
	}
	for i, tool := range result.Tools {
		if tool.Name != wantNames[i] {
			t.Errorf("tool %d name = %q, want %q", i, tool.Name, wantNames[i])
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Errorf("tool %q annotations = %#v, want read-only closed-world", tool.Name, tool.Annotations)
		}
		if tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("tool %q is missing an inferred schema", tool.Name)
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
	if got := requireMap(t, workspaces[0])["uuid"]; got != "a" {
		t.Errorf("first workspace UUID = %v, want a", got)
	}
	const uuid = "123e4567-e89b-12d3-a456-426614174000"
	getResult := callTool(t, session, workspaceGetTool, map[string]any{"uuid": uuid})
	if reader.requestedUUID != uuid {
		t.Errorf("requested UUID = %q, want %q", reader.requestedUUID, uuid)
	}
	workspace := requireMap(t, requireMap(t, getResult.StructuredContent)["workspace"])
	if workspace["name"] != first {
		t.Errorf("workspace name = %v, want %s", workspace["name"], first)
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
		{name: workspaceGetTool, reader: &fakeReader{getWorkErr: errors.New("not found")}, args: map[string]any{"uuid": "missing"}},
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
}

func TestConnectReaderPropagatesRPCErrors(t *testing.T) {
	wantErr := errors.New("rpc failed")
	reader := ConnectReader{
		nodes:     fakeNodeLister{response: &nodectrlv1.ListNodesResponse{}, err: wantErr},
		workspace: &fakeWorkspaceReader{listResponse: &workspacev1.ListWorkspacesResponse{}, err: wantErr},
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
