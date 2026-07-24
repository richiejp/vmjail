//go:build linux && integration

package testdaemons_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richiejp/darkaifactory/internal/testdaemons"
)

func TestClusterAgentRunLifecycleOverMCP(t *testing.T) {
	imagePath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")
	taggedRef := pushDockerTarballToRegistry(t, imagePath, "test/mcp-agent:v1")
	digest, err := crane.Digest(taggedRef)
	if err != nil {
		t.Fatalf("resolve test image digest: %v", err)
	}
	imageRef := strings.TrimSuffix(taggedRef, ":v1") + "@" + digest

	cluster := testdaemons.NewCluster(t, testdaemons.WithOperatorClient())
	workspaceName := "mcp-agent-run"
	projectDir := t.TempDir()
	mountSource := filepath.Join(projectDir, "src")
	if err := os.MkdirAll(filepath.Join(projectDir, ".daft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mountSource, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mountSource, "input.txt"), []byte("MCP-PUSH-OK\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	projectConfig := []byte(`{"mounts":[{"local":"src","remote":"/workspace"}]}`)
	if err := os.WriteFile(filepath.Join(projectDir, ".daft", "config.json"), projectConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	runDaft(
		t, cluster,
		"workspace", "create", "--name", workspaceName,
		"--orchestrator", cluster.OrchestratorURL,
	)
	runDaft(
		t, cluster,
		"workspace", "mount", "add", "/workspace",
		"--name", workspaceName, "--orchestrator", cluster.OrchestratorURL,
	)
	runDaft(
		t, cluster,
		"workspace", "target", cluster.SandboxdFP.String(), "--name", workspaceName,
		"--orchestrator", cluster.OrchestratorURL,
	)

	profilePath := writeMCPAgentProfile(t, imageRef)
	var stderr bytes.Buffer
	processCtx, cancelProcess := context.WithCancel(context.Background())
	t.Cleanup(cancelProcess)
	command := exec.CommandContext(
		processCtx, cluster.MCPBin, //nolint:gosec // test-owned binary and paths.
		"--orchestrator", cluster.OrchestratorURL,
		"--config-dir", cluster.ClientConfigDir,
		"--project-dir", projectDir,
		"--agent-profiles", profilePath,
	)
	command.Stderr = &stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "daft-integration", Version: "test"}, nil)
	session, err := client.Connect(processCtx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatalf("connect to daft-mcp: %v\n%s", err, stderr.String())
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close MCP session: %v\n%s", err, stderr.String())
		}
	})

	workspaceList := callMCPTool(t, session, "workspace_list", map[string]any{})
	workspaceUUID := findWorkspaceUUID(t, workspaceList, workspaceName)
	pushed := callMCPTool(t, session, "workspace_push", map[string]any{
		"uuid": workspaceUUID, "project_path": projectDir,
	})
	mounts, ok := pushed["mounts"].([]any)
	if !ok || len(mounts) != 1 {
		t.Fatalf("workspace_push result = %#v", pushed)
	}
	firstMount, ok := mounts[0].(map[string]any)
	if !ok || numberField(t, firstMount, "entries") == 0 {
		t.Fatalf("workspace_push mount result = %#v", mounts[0])
	}
	started := callMCPTool(t, session, "agent_start", map[string]any{
		"request_id":     "123e4567-e89b-42d3-a456-426614174000",
		"workspace_uuid": workspaceUUID,
		"profile_name":   "integration",
		"prompt":         "cat /workspace/input.txt && printf 'MCP-AGENT-OK\\n'",
	})
	runUUID := stringField(t, mapField(t, started, "run"), "uuid")
	if runUUID == "" {
		t.Fatal("agent_start returned an empty run UUID")
	}

	deadline := time.Now().Add(90 * time.Second)
	after := uint64(0)
	var terminal bool
	for !terminal && time.Now().Before(deadline) {
		events := callMCPTool(t, session, "agent_events", map[string]any{
			"uuid": runUUID, "after_cursor": after, "limit": 50,
		})
		after = uint64(numberField(t, events, "next_cursor"))
		terminal, _ = events["terminal"].(bool)
		if !terminal {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !terminal {
		status := callMCPTool(t, session, "agent_status", map[string]any{"uuid": runUUID})
		t.Fatalf("AgentRun did not become terminal before timeout: %#v\n%s", status, stderr.String())
	}

	status := mapField(t, callMCPTool(t, session, "agent_status", map[string]any{"uuid": runUUID}), "run")
	if got := stringField(t, status, "state"); got != "AGENT_RUN_STATE_SUCCEEDED" {
		t.Fatalf("terminal state = %q, want succeeded; status=%#v", got, status)
	}
	if _, leaked := status["result"]; leaked {
		t.Fatalf("agent_status leaked the result body: %#v", status)
	}
	result := callMCPTool(t, session, "agent_result", map[string]any{"uuid": runUUID})
	if got := stringField(t, result, "result"); !strings.Contains(got, "MCP-AGENT-OK") {
		t.Fatalf("agent_result = %q, want job sentinel; result=%#v", got, result)
	}
	if got := stringField(t, result, "result"); !strings.Contains(got, "MCP-PUSH-OK") {
		t.Fatalf("agent_result = %q, want pushed-file sentinel; result=%#v", got, result)
	}

	// Terminal reconciliation must also tear down the exact Sandbox and clear
	// the Workspace binding. Poll through MCP so this assertion covers the
	// public resume/status boundary rather than reaching into daemon state.
	for time.Now().Before(deadline) {
		workspace := mapField(t, callMCPTool(t, session, "workspace_get", map[string]any{"uuid": workspaceUUID}), "workspace")
		if _, present := workspace["current_sandbox_uuid"]; !present {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("terminal AgentRun did not clear Workspace Sandbox binding\n%s", stderr.String())
}

func runDaft(t *testing.T, cluster *testdaemons.Cluster, args ...string) {
	t.Helper()
	fullArgs := append([]string{"--config-dir", cluster.ClientConfigDir}, args...)
	cmd := exec.CommandContext(t.Context(), cluster.DaftBin, fullArgs...) //nolint:gosec // test-owned binary and arguments.
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("daft %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func writeMCPAgentProfile(t *testing.T, imageRef string) string {
	t.Helper()
	profile := map[string]any{
		"version": 1,
		"profiles": []any{map[string]any{
			"name": "integration", "provider": "codex", "image": imageRef,
			"command":     []string{"/bin/dash", "-c"},
			"credentials": map[string]any{"enable": false},
			"resources":   map[string]any{"enable": false},
			"egress":      map[string]any{"enable": false},
		}},
	}
	data, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agent-profiles.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func callMCPTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("MCP %s: %v", name, err)
	}
	if result.IsError {
		content, _ := json.Marshal(result.Content)
		t.Fatalf("MCP %s returned tool error: %s", name, content)
	}
	out, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("MCP %s structured content has type %T", name, result.StructuredContent)
	}
	return out
}

func findWorkspaceUUID(t *testing.T, output map[string]any, name string) string {
	t.Helper()
	workspaces, ok := output["workspaces"].([]any)
	if !ok {
		t.Fatalf("workspace_list output = %#v", output)
	}
	for _, raw := range workspaces {
		workspace, ok := raw.(map[string]any)
		if ok && workspace["name"] == name {
			return stringField(t, workspace, "uuid")
		}
	}
	t.Fatalf("workspace %q missing from %#v", name, output)
	return ""
}

func mapField(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := object[key].(map[string]any)
	if !ok {
		t.Fatalf("%s in %#v has type %T", key, object, object[key])
	}
	return value
}

func stringField(t *testing.T, object map[string]any, key string) string {
	t.Helper()
	value, ok := object[key].(string)
	if !ok {
		t.Fatalf("%s in %#v has type %T", key, object, object[key])
	}
	return value
}

func numberField(t *testing.T, object map[string]any, key string) float64 {
	t.Helper()
	value, ok := object[key].(float64)
	if !ok {
		t.Fatalf("%s in %#v has type %T", key, object, object[key])
	}
	return value
}
