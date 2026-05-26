package cli

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestWorkspaceAttachRoundTrip drives `daft workspace attach`
// end-to-end: an orchestrator-brokered serve is already up, a
// sandbox-host has registered, then the user runs `attach` and
// the AttachAndDial half on the sandbox side completes.
func TestWorkspaceAttachRoundTrip(t *testing.T) {
	clientDir, cliID, sbID, addr, srvID := setupBrokerTrio(t)
	src := writeWorkspaceFixture(t)
	startServeBrokered(t, clientDir, addr, src)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sbClient := mustBrokerClient(t, sbID, srvID, addr)
	attached, attachErr := startAttachAndDial(ctx, sbClient, "ws", "sb-attach", cliID.Fingerprint())
	stdout, stderr, err := runDaft(
		t, workspaceCmdName, debugSubName, attachSubName,
		configDirArg, clientDir,
		"--orchestrator=https://"+addr,
		"--workspace=ws",
		"--sandbox=sb-attach",
		"--timeout=10s",
	)
	if err != nil {
		t.Fatalf("attach: %v stdout=%q stderr=%q", err, stdout, stderr)
	}
	if !strings.Contains(stdout, `requested attach of sandbox "sb-attach"`) {
		t.Errorf("stdout missing attach line: %q", stdout)
	}
	ic := awaitAttach(t, attached, attachErr)
	_ = ic.Close()
}

// TestWorkspaceAttachRejectsBadFlags pins the three required-flag
// validations on the bare command path.
func TestWorkspaceAttachRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"missing-orchestrator", []string{"--workspace=ws", "--sandbox=sb"}},
		{"missing-workspace", []string{"--orchestrator=https://x", "--sandbox=sb"}},
		{"missing-sandbox", []string{"--orchestrator=https://x", "--workspace=ws"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{workspaceCmdName, debugSubName, attachSubName}, tc.args...)
			_, _, err := runDaft(t, args...)
			if err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

// TestWorkspaceAttachUnreachable exercises the RequestAttach
// error path: a pre-cancelled context against a stub orchestrator
// URL fails the RPC before the broker can respond.
func TestWorkspaceAttachUnreachable(t *testing.T) {
	clientDir := t.TempDir()
	if _, err := loadOrGenerateClient(clientDir); err != nil {
		t.Fatalf("loadOrGenerateClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runWorkspaceAttach(ctx, &syncBuf{}, workspaceAttachOpts{
		orchestrator: unreachableOrchestrator,
		workspace:    "ws",
		sandbox:      "sb",
		configDir:    clientDir,
	})
	if err == nil {
		t.Error("expected error against unreachable orchestrator")
	}
}
