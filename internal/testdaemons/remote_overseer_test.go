//go:build linux && integration

package testdaemons_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/testdaemons"
)

func TestRemoteOverseerRecipeOverSSHAndMCP(t *testing.T) {
	cluster := testdaemons.NewCluster(t, testdaemons.WithOperatorClient())
	sshServer := testdaemons.NewSSHServer(t)
	repository := repositoryRoot(t)
	artifact := requireEnvPath(t, "DAFT_SANDBOXD_BUNDLED_BIN",
		"nix build .#daft-sandboxd -o bin/daft-sandboxd-bundled")
	version := strings.TrimSpace(runLocal(t, artifact, "version"))
	digest := sha256File(t, artifact)

	inspection := runLocal(
		t,
		filepath.Join(repository, ".agents/skills/deploy-daft-node/scripts/inspect-artifact.sh"),
		artifact,
	)
	if !strings.Contains(inspection, "schema=daft-artifact-v1") ||
		!strings.Contains(inspection, "sha256="+digest) ||
		!strings.Contains(inspection, "version="+version) {
		t.Fatalf("artifact inspection = %q", inspection)
	}
	probe := sshServer.RunScript(t,
		filepath.Join(repository, ".agents/skills/deploy-daft-node/scripts/probe-node.sh"))
	if !strings.Contains(probe, "schema=daft-node-probe-v1") ||
		!strings.Contains(probe, "kvm_access=rw") ||
		!strings.Contains(probe, "cgroup_controllers=") {
		t.Fatalf("remote Node probe = %q", probe)
	}

	remoteBase := t.TempDir()
	remoteConfig := filepath.Join(remoteBase, "config")
	remoteState := filepath.Join(remoteBase, "state")
	installed := filepath.Join(remoteBase, "daft-sandboxd")
	staged := installed + ".staged"
	sshServer.RunShell(t, `set -eu
mkdir -p -- "$1" "$2"
`, remoteConfig, remoteState)
	sshServer.Upload(t, artifact, staged)
	stageScript := filepath.Join(repository, ".agents/skills/deploy-daft-node/scripts/stage-release.sh")
	stagedOutput := sshServer.RunScript(t, stageScript, staged, installed, digest, version)
	if !strings.Contains(stagedOutput, "installed="+installed) {
		t.Fatalf("stage output = %q", stagedOutput)
	}
	// An exact deployment retry must converge without replacing the installed
	// generation.
	retryStaged := installed + ".retry"
	sshServer.Upload(t, artifact, retryStaged)
	retryOutput := sshServer.RunScript(t, stageScript, retryStaged, installed, digest, version)
	if !strings.Contains(retryOutput, "status=noop") {
		t.Fatalf("stage retry output = %q", retryOutput)
	}
	verify := sshServer.RunScript(t,
		filepath.Join(repository, ".agents/skills/deploy-daft-node/scripts/verify-node.sh"),
		installed, cluster.OrchestratorURL, version)
	if !strings.Contains(verify, "local_binary=ok") {
		t.Fatalf("remote verify output = %q", verify)
	}

	show := sshServer.RunShell(t, `set -eu
"$1" auth show --config-dir "$2"
`, installed, remoteConfig)
	nodeFingerprint := parseShownFingerprint(t, show)
	sshServer.RunShell(t, `set -eu
"$1" auth approve "$2" --role orchestrator --config-dir "$3"
`, installed, cluster.OrchestratorFP.String(), remoteConfig)

	imagePath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")
	taggedRef := pushDockerTarballToRegistry(t, imagePath, "test/remote-overseer:v1")
	imageDigest, err := crane.Digest(taggedRef)
	if err != nil {
		t.Fatalf("resolve test image digest: %v", err)
	}
	imageRef := strings.TrimSuffix(taggedRef, ":v1") + "@" + imageDigest
	projectDir := remoteOverseerProject(t)
	profilePath := writeMCPAgentProfile(t, imageRef)
	session, stopMCP, stderr := startMCP(t, cluster, projectDir, profilePath)

	authorized := mapField(t, callMCPTool(t, session, "node_authorize", map[string]any{
		"fingerprint": nodeFingerprint, "label": "ssh-proof",
	}), "node")
	if stringField(t, authorized, "fingerprint") != nodeFingerprint {
		t.Fatalf("authorized Node = %#v", authorized)
	}

	remotePID := startRemoteSandboxd(t, sshServer, installed, cluster.OrchestratorURL, remoteConfig, remoteState)
	defer func() { stopRemoteSandboxd(t, sshServer, remotePID) }()
	time.Sleep(500 * time.Millisecond)
	startup := remoteSandboxdDiagnostic(t, sshServer, remotePID, remoteState)
	t.Logf("remote sandboxd startup:\n%s", startup)
	if strings.Contains(startup, "process=stopped") {
		t.Fatalf("remote sandboxd exited during startup:\n%s", startup)
	}
	deadline := time.Now().Add(90 * time.Second)
	waitNodeConnected(t, session, nodeFingerprint, true, deadline)

	// A service restart must preserve the Node identity and reconnect without
	// repeating trust establishment.
	stopRemoteSandboxd(t, sshServer, remotePID)
	waitNodeConnected(t, session, nodeFingerprint, false, deadline)
	remotePID = startRemoteSandboxd(t, sshServer, installed, cluster.OrchestratorURL, remoteConfig, remoteState)
	waitNodeConnected(t, session, nodeFingerprint, true, deadline)

	applied := callMCPTool(t, session, "workspace_apply", map[string]any{
		"name": "remote-overseer-proof", "expected_generation": 0,
		"spec": map[string]any{
			"image_ref": imageRef,
			"mounts": []any{map[string]any{
				"target": "/workspace", "source": "src", "read_only": false,
			}},
			"target_node_fingerprint": nodeFingerprint,
			"isolation":               "shared",
			"disk_bytes":              1 << 30,
		},
	})
	workspace := mapField(t, applied, "workspace")
	workspaceUUID := stringField(t, workspace, "uuid")
	if created, _ := applied["created"].(bool); !created {
		t.Fatalf("initial workspace_apply did not create: %#v", applied)
	}
	callMCPTool(t, session, "workspace_validate", map[string]any{"uuid": workspaceUUID})
	callMCPTool(t, session, "workspace_push", map[string]any{
		"uuid": workspaceUUID, "project_path": projectDir,
	})

	startArgs := map[string]any{
		"request_id":     "623e4567-e89b-42d3-a456-426614174000",
		"workspace_uuid": workspaceUUID,
		"profile_name":   "integration",
		"prompt":         "sleep 2; cat /workspace/input.txt",
	}
	run := mapField(t, callMCPTool(t, session, "agent_start", startArgs), "run")
	runUUID := stringField(t, run, "uuid")
	if got := stringField(t, run, "node_fingerprint"); got != nodeFingerprint {
		t.Fatalf("AgentRun Node = %q, want %q", got, nodeFingerprint)
	}
	firstEvents := callMCPTool(t, session, "agent_events", map[string]any{
		"uuid": runUUID, "after_cursor": 0, "limit": 1,
	})
	cursor := uint64(numberField(t, firstEvents, "next_cursor"))
	stopMCP()

	session, _, stderr = startMCP(t, cluster, projectDir, profilePath)
	retried := mapField(t, callMCPTool(t, session, "agent_start", startArgs), "run")
	if got := stringField(t, retried, "uuid"); got != runUUID {
		t.Fatalf("retry AgentRun UUID = %q, want %q", got, runUUID)
	}
	waitRemoteRunTerminal(t, session, runUUID, cursor, deadline, stderr)
	result := callMCPTool(t, session, "agent_result", map[string]any{"uuid": runUUID})
	if got := stringField(t, result, "result"); !strings.Contains(got, "REMOTE-OVERSEER-OK") {
		t.Fatalf("remote AgentRun result = %q", got)
	}
	waitWorkspaceStopped(t, session, workspaceUUID, deadline, stderr)
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func runLocal(t *testing.T, program string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), program, args...) //nolint:gosec // test-owned programs and arguments.
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", program, err, output)
	}
	return string(output)
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path) //nolint:gosec // test artifact path supplied by make integration.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func parseShownFingerprint(t *testing.T, output string) string {
	t.Helper()
	for line := range strings.SplitSeq(output, "\n") {
		value, found := strings.CutPrefix(line, "fingerprint:")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		if _, err := identity.ParseFingerprint(value); err != nil {
			t.Fatalf("remote fingerprint %q: %v", value, err)
		}
		return value
	}
	t.Fatalf("fingerprint missing from %q", output)
	return ""
}

func remoteOverseerProject(t *testing.T) string {
	t.Helper()
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".daft"), 0o700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(project, "src")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "input.txt"), []byte("REMOTE-OVERSEER-OK\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".daft", "config.json"),
		[]byte(`{"mounts":[{"local":"src","remote":"/workspace"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return project
}

func startRemoteSandboxd(t *testing.T, server *testdaemons.SSHServer, binary, orchestrator, config, state string) string {
	t.Helper()
	output := server.RunShell(t, `set -eu
binary=$1 orchestrator=$2 config=$3 state=$4 log=$5 runtime=$6 bus=$7
nohup env DAFT_EGRESS_PROXY=0 DAFT_STATE_DIR="$state" \
  XDG_RUNTIME_DIR="$runtime" DBUS_SESSION_BUS_ADDRESS="$bus" "$binary" serve \
  --orchestrator "$orchestrator" --config-dir "$config" \
  </dev/null >"$log" 2>&1 &
printf '%s\n' "$!"
`, binary, orchestrator, config, state, filepath.Join(state, "sandboxd.log"),
		os.Getenv("XDG_RUNTIME_DIR"), os.Getenv("DBUS_SESSION_BUS_ADDRESS"))
	pid := strings.TrimSpace(output)
	if _, err := strconv.Atoi(pid); err != nil {
		t.Fatalf("remote sandboxd PID = %q: %v", pid, err)
	}
	return pid
}

func stopRemoteSandboxd(t *testing.T, server *testdaemons.SSHServer, pid string) {
	t.Helper()
	if pid == "" {
		return
	}
	server.RunShell(t, `set -eu
kill "$1" 2>/dev/null || true
i=0
while kill -0 "$1" 2>/dev/null && [ "$i" -lt 100 ]; do
  sleep 0.05
  i=$((i + 1))
done
`, pid)
}

func remoteSandboxdDiagnostic(t *testing.T, server *testdaemons.SSHServer, pid, state string) string {
	t.Helper()
	return server.RunShell(t, `set -eu
if kill -0 "$1" 2>/dev/null; then echo process=running; else echo process=stopped; fi
log=$2/sandboxd.log
if [ -f "$log" ]; then tail -n 40 "$log"; else echo log=missing; fi
`, pid, state)
}

func waitNodeConnected(t *testing.T, session *mcp.ClientSession, fingerprint string, connected bool, deadline time.Time) {
	t.Helper()
	for time.Now().Before(deadline) {
		node := mapField(t, callMCPTool(t, session, "node_registration_status", map[string]any{"fingerprint": fingerprint}), "node")
		got, _ := node["connected"].(bool)
		if got == connected {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("remote Node %s connected never became %t", fingerprint, connected)
}

func waitRemoteRunTerminal(t *testing.T, session *mcp.ClientSession, runUUID string, cursor uint64, deadline time.Time, stderr *bytes.Buffer) {
	t.Helper()
	for time.Now().Before(deadline) {
		events := callMCPTool(t, session, "agent_events", map[string]any{
			"uuid": runUUID, "after_cursor": cursor, "limit": 50,
		})
		cursor = uint64(numberField(t, events, "next_cursor"))
		terminal, _ := events["terminal"].(bool)
		if terminal {
			run := mapField(t, callMCPTool(t, session, "agent_status", map[string]any{"uuid": runUUID}), "run")
			if state := stringField(t, run, "state"); state != "AGENT_RUN_STATE_SUCCEEDED" {
				t.Fatalf("remote AgentRun state = %q; run=%#v\n%s", state, run, stderr.String())
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("remote AgentRun did not become terminal\n%s", stderr.String())
}
