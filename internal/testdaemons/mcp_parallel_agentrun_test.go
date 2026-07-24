//go:build linux && integration

package testdaemons_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richiejp/darkaifactory/internal/testdaemons"
)

func TestClusterParallelAgentRunsOnDifferentNodes(t *testing.T) {
	imagePath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")
	taggedRef := pushDockerTarballToRegistry(t, imagePath, "test/mcp-parallel-agent:v1")
	digest, err := crane.Digest(taggedRef)
	if err != nil {
		t.Fatalf("resolve test image digest: %v", err)
	}
	imageRef := strings.TrimSuffix(taggedRef, ":v1") + "@" + digest

	cluster := testdaemons.NewCluster(
		t,
		testdaemons.WithOperatorClient(),
		testdaemons.WithSandboxdCount(2),
	)
	profilePath := writeMCPAgentProfile(t, imageRef)

	type project struct {
		name      string
		dir       string
		workspace string
		sentinel  string
	}
	projects := make([]project, 2)
	for i := range projects {
		projects[i] = project{
			name:     "parallel-" + string(rune('a'+i)),
			dir:      t.TempDir(),
			sentinel: "PARALLEL-" + string(rune('A'+i)) + "-OK",
		}
		src := filepath.Join(projects[i].dir, "src")
		if err := os.MkdirAll(filepath.Join(projects[i].dir, ".daft"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(src, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "input.txt"), []byte(projects[i].sentinel+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		config := []byte(`{"mounts":[{"local":"src","remote":"/workspace"}]}`)
		if err := os.WriteFile(filepath.Join(projects[i].dir, ".daft", "config.json"), config, 0o600); err != nil {
			t.Fatal(err)
		}
		runDaft(t, cluster, "workspace", "create", "--name", projects[i].name,
			"--orchestrator", cluster.OrchestratorURL)
		runDaft(t, cluster, "workspace", "mount", "add", "/workspace",
			"--name", projects[i].name, "--orchestrator", cluster.OrchestratorURL)
		runDaft(t, cluster, "workspace", "target", cluster.SandboxdFPs[i].String(),
			"--name", projects[i].name, "--orchestrator", cluster.OrchestratorURL)
	}

	sessionA, _, stderrA := startMCP(t, cluster, projects[0].dir, profilePath)
	sessionB, _, stderrB := startMCP(t, cluster, projects[1].dir, profilePath)
	for i, session := range []*mcp.ClientSession{sessionA, sessionB} {
		listed := callMCPTool(t, session, "workspace_list", map[string]any{})
		projects[i].workspace = findWorkspaceUUID(t, listed, projects[i].name)
		callMCPTool(t, session, "workspace_push", map[string]any{
			"uuid": projects[i].workspace, "project_path": projects[i].dir,
		})
	}

	runA := mapField(t, callMCPTool(t, sessionA, "agent_start", map[string]any{
		"request_id":     "423e4567-e89b-42d3-a456-426614174000",
		"workspace_uuid": projects[0].workspace,
		"profile_name":   "integration",
		"prompt":         "sleep 15; cat /workspace/input.txt",
	}), "run")
	runB := mapField(t, callMCPTool(t, sessionB, "agent_start", map[string]any{
		"request_id":     "523e4567-e89b-42d3-a456-426614174000",
		"workspace_uuid": projects[1].workspace,
		"profile_name":   "integration",
		"prompt":         "sleep 5; cat /workspace/input.txt",
	}), "run")
	runAUUID := stringField(t, runA, "uuid")
	runBUUID := stringField(t, runB, "uuid")
	if got := stringField(t, runA, "node_fingerprint"); got != cluster.SandboxdFPs[0].String() {
		t.Fatalf("first AgentRun node = %q, want %q", got, cluster.SandboxdFPs[0])
	}
	if got := stringField(t, runB, "node_fingerprint"); got != cluster.SandboxdFPs[1].String() {
		t.Fatalf("second AgentRun node = %q, want %q", got, cluster.SandboxdFPs[1])
	}

	deadline := time.Now().Add(90 * time.Second)
	waitAgentState(t, sessionA, runAUUID, "AGENT_RUN_STATE_RUNNING", deadline, stderrA)
	waitAgentState(t, sessionB, runBUUID, "AGENT_RUN_STATE_RUNNING", deadline, stderrB)
	waitAgentState(t, sessionA, runAUUID, "AGENT_RUN_STATE_SUCCEEDED", deadline, stderrA)
	waitAgentState(t, sessionB, runBUUID, "AGENT_RUN_STATE_SUCCEEDED", deadline, stderrB)

	for i, item := range []struct {
		session *mcp.ClientSession
		uuid    string
	}{
		{sessionA, runAUUID},
		{sessionB, runBUUID},
	} {
		result := callMCPTool(t, item.session, "agent_result", map[string]any{"uuid": item.uuid})
		if got := stringField(t, result, "result"); !strings.Contains(got, projects[i].sentinel) {
			t.Fatalf("project %d result = %q, want %q", i, got, projects[i].sentinel)
		}
	}
}
