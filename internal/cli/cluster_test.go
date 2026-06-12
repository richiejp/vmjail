package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/localai-org/darkaifactory/internal/localcluster"
)

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

func TestClusterUpRequiresState(t *testing.T) {
	if _, _, err := runCmd(t, clusterCmdName, "up"); err == nil {
		t.Errorf("`daft cluster up` without --state should error")
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

func TestPrintClusterEnv(t *testing.T) {
	var buf bytes.Buffer
	printClusterEnv(&buf, &localcluster.Cluster{
		URL:             "https://127.0.0.1:8443",
		ClientConfigDir: "/state/client",
		SandboxFP:       "sha256:abc",
		OrchestratorFP:  "sha256:def",
		StateDir:        "/state",
	})
	out := buf.String()
	for _, want := range []string{
		"DAFT_CLUSTER_URL=https://127.0.0.1:8443",
		"DAFT_CLUSTER_CLIENT_DIR=/state/client",
		"DAFT_CLUSTER_SANDBOX_FP=sha256:abc",
		"DAFT_CLUSTER_ORCH_FP=sha256:def",
		"DAFT_CLUSTER_STATE=/state",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("printClusterEnv output missing %q, got:\n%s", want, out)
		}
	}
}
