package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/localcluster"
)

// stubClusterUp swaps the clusterUp seam to return a fixed handle (no real
// daemons), restoring it on cleanup.
func stubClusterUp(t *testing.T) {
	t.Helper()
	prev := clusterUp
	clusterUp = func(context.Context, localcluster.Config) (*localcluster.Cluster, error) {
		return &localcluster.Cluster{
			URL: testClusterURL, ClientConfigDir: testClusterClientDir,
			SandboxFP: testSandboxFP, OrchestratorFP: testOrchFP, StateDir: testStateDir,
		}, nil
	}
	t.Cleanup(func() { clusterUp = prev })
}

func TestClusterUpEmitsSummaryByDefault(t *testing.T) {
	stubClusterUp(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	out, _, err := runCmd(t, clusterCmdName, "up")
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if !strings.Contains(out, "automatically") || strings.Contains(out, "DAFT_CLUSTER_URL=") {
		t.Errorf("default `up` should print the human summary, not env lines, got:\n%s", out)
	}
}

func TestClusterUpShellEmitsEnv(t *testing.T) {
	stubClusterUp(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	out, _, err := runCmd(t, clusterCmdName, "up", "--shell")
	if err != nil {
		t.Fatalf("up --shell: %v", err)
	}
	if !strings.Contains(out, "DAFT_CLUSTER_URL=") {
		t.Errorf("`up --shell` should print eval-able env lines, got:\n%s", out)
	}
}

func TestClusterSubcommandRegistered(t *testing.T) {
	stdout, _, err := runCmd(t, clusterCmdName, "--help")
	if err != nil {
		t.Fatalf("daft cluster --help: %v", err)
	}
	for _, want := range []string{"up", "down"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("`daft cluster --help` should advertise %q, got:\n%s", want, stdout)
		}
	}
}

func TestClusterUpDefaultsStateDir(t *testing.T) {
	// --state is no longer required: it falls back to
	// $XDG_STATE_HOME/daft/cluster. Pin XDG at a temp dir so the test never
	// touches the real per-user state, and use a missing daemon binary so
	// the run fails fast (after the default-state path is exercised) rather
	// than spawning anything.
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	_, _, err := runCmd(t, clusterCmdName, "up",
		"--orchestrator-bin", "/nonexistent-daft-orchestrator-xyz",
		"--ready-timeout", "2s")
	if err == nil {
		t.Errorf("`daft cluster up` should still error on a missing binary")
	}
	// The default-state path should have minted identities under the XDG
	// location, proving --state defaulted rather than the command bailing on
	// a required flag.
	if _, statErr := os.Stat(filepath.Join(xdg, "daft", "cluster")); statErr != nil {
		t.Errorf("expected default state dir under XDG_STATE_HOME, stat: %v", statErr)
	}
}

func TestClusterUpFailsFastOnMissingBinary(t *testing.T) {
	// A nonexistent orchestrator binary makes the spawn fail
	// immediately, exercising the up RunE error path without waiting
	// on any readiness timeout.
	_, _, err := runCmd(t, clusterCmdName, "up",
		"--state", t.TempDir(),
		"--orchestrator-bin", "/nonexistent-daft-orchestrator-xyz",
		"--ready-timeout", "2s")
	if err == nil {
		t.Errorf("`daft cluster up` with a missing binary should error")
	}
}

func TestClusterDownEmptyStateIsOK(t *testing.T) {
	if _, _, err := runCmd(t, clusterCmdName, "down", "--state", t.TempDir()); err != nil {
		t.Errorf("`daft cluster down` on an empty state dir: %v", err)
	}
}

func TestClusterDownDefaultsStateDir(t *testing.T) {
	// No --state: down falls back to $XDG_STATE_HOME/daft/cluster. An empty
	// (never-up) default dir is a no-op, not an error — proving the flag
	// defaulted rather than being required.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, _, err := runCmd(t, clusterCmdName, "down"); err != nil {
		t.Errorf("`daft cluster down` with default state: %v", err)
	}
}

func TestStateDirOrDefault(t *testing.T) {
	if got, err := stateDirOrDefault("/explicit"); err != nil || got != "/explicit" {
		t.Fatalf("explicit flag: got (%q, %v), want /explicit", got, err)
	}
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	got, err := stateDirOrDefault("")
	if err != nil {
		t.Fatalf("default: %v", err)
	}
	if want := filepath.Join(xdg, "daft", "cluster"); got != want {
		t.Fatalf("default: got %q, want %q", got, want)
	}
}

func TestEmitClusterHandle(t *testing.T) {
	c := &localcluster.Cluster{
		URL: testClusterURL, ClientConfigDir: testClusterClientDir,
		SandboxFP: testSandboxFP, OrchestratorFP: testOrchFP, StateDir: testStateDir,
	}
	// --shell: machine-readable env lines.
	var shell bytes.Buffer
	emitClusterHandle(&shell, c, true)
	if !strings.Contains(shell.String(), "DAFT_CLUSTER_URL=") {
		t.Errorf("--shell should emit env lines, got:\n%s", shell.String())
	}
	// default: human summary, no env lines.
	var human bytes.Buffer
	emitClusterHandle(&human, c, false)
	if strings.Contains(human.String(), "DAFT_CLUSTER_URL=") {
		t.Errorf("default output must be the human summary, got:\n%s", human.String())
	}
}

func TestPrintClusterSummary(t *testing.T) {
	var buf bytes.Buffer
	printClusterSummary(&buf, &localcluster.Cluster{
		URL:       testClusterURL,
		SandboxFP: testSandboxFP,
		StateDir:  testStateDir,
	})
	out := buf.String()
	// Human summary: shows where the cluster is and that the client finds it
	// automatically — and must NOT emit the eval-able env lines (that's --shell).
	for _, want := range []string{testClusterURL, testStateDir, "automatically"} {
		if !strings.Contains(out, want) {
			t.Errorf("printClusterSummary missing %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "DAFT_CLUSTER_URL=") {
		t.Errorf("human summary must not emit eval-able env lines, got:\n%s", out)
	}
}

func TestPrintClusterEnv(t *testing.T) {
	var buf bytes.Buffer
	printClusterEnv(&buf, &localcluster.Cluster{
		URL:             testClusterURL,
		ClientConfigDir: testClusterClientDir,
		SandboxFP:       testSandboxFP,
		OrchestratorFP:  testOrchFP,
		StateDir:        testStateDir,
	})
	out := buf.String()
	for _, want := range []string{
		"DAFT_CLUSTER_URL=https://127.0.0.1:8443",
		"DAFT_CLUSTER_CLIENT_DIR=/state/client",
		"DAFT_CLUSTER_SANDBOX_FP=sha256:abc",
		"DAFT_CLUSTER_ORCH_FP=sha256:def",
		"DAFT_CLUSTER_STATE=" + testStateDir,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("printClusterEnv output missing %q, got:\n%s", want, out)
		}
	}
}
