package sandboxdcli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPullWorkspaceFromConfigDirError forces resolveSandboxdDir to
// fail by clearing both HOME and XDG_CONFIG_HOME.
func TestPullWorkspaceFromConfigDirError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, _, err := pullWorkspaceFrom(context.Background(), pullWorkspaceOpts{
		addr: testFakeAddr, name: "ws",
	}); err == nil {
		t.Fatal("expected error when config dir is unresolvable")
	}
}

// TestPullWorkspaceFromCorruptIdentity plants garbage cert files so
// loadOrGenerateSandboxd fails — covering the identity-load error
// branch.
//
//nolint:dupl // Parallel to TestAttachLazyWorkspaceFromCorruptIdentity by design.
func TestPullWorkspaceFromCorruptIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("WriteFile cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("WriteFile key: %v", err)
	}
	if _, _, err := pullWorkspaceFrom(context.Background(), pullWorkspaceOpts{
		addr: testFakeAddr, name: "ws", configDir: dir,
	}); err == nil {
		t.Fatal("expected error for corrupt identity")
	}
}

// TestPullWorkspaceFromCorruptPeerStore plants corrupt peers.json
// so openSandboxdPeerStore fails after identity loads.
func TestPullWorkspaceFromCorruptPeerStore(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrGenerateSandboxd(dir); err != nil {
		t.Fatalf("loadOrGenerate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, peersFilename), []byte("not json"), 0o600); err != nil {
		t.Fatalf("WriteFile peers: %v", err)
	}
	if _, _, err := pullWorkspaceFrom(context.Background(), pullWorkspaceOpts{
		addr: testFakeAddr, name: "ws", configDir: dir,
	}); err == nil {
		t.Fatal("expected error for corrupt peer store")
	}
}

// TestPullWorkspaceFromUnreachable fails inside GetManifest because
// nothing's listening at testFakeAddr. Covers the "happy path up to
// the wire" branch.
func TestPullWorkspaceFromUnreachable(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the dial doesn't loop
	_, _, err := pullWorkspaceFrom(ctx, pullWorkspaceOpts{
		addr: testFakeAddr, name: "ws", configDir: dir,
	})
	if err == nil {
		t.Fatal("expected dial error against unreachable addr")
	}
	if !strings.Contains(err.Error(), "GetManifest") {
		t.Errorf("error = %v, want it to mention GetManifest", err)
	}
}

// TestPullWorkspaceBrokerConfigDirError pins the early-out path
// when resolveSandboxdDir can't find any usable config dir.
func TestPullWorkspaceBrokerConfigDirError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	_, _, err := pullWorkspaceBroker(context.Background(), pullWorkspaceBrokerOpts{
		orchestratorURL: testFakeOrchURL, workspace: "ws", sandboxID: "sb",
		serveFP: "sha256:" + strings.Repeat("AB:", 31) + "AB",
	})
	if err == nil {
		t.Fatal("expected error when config dir is unresolvable")
	}
}

// TestPullWorkspaceBrokerCorruptIdentity hits the
// loadOrGenerateSandboxd error branch.
func TestPullWorkspaceBrokerCorruptIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("WriteFile cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("WriteFile key: %v", err)
	}
	_, _, err := pullWorkspaceBroker(context.Background(), pullWorkspaceBrokerOpts{
		orchestratorURL: testFakeOrchURL, workspace: "ws", sandboxID: "sb",
		serveFP: "sha256:" + strings.Repeat("AB:", 31) + "AB", configDir: dir,
	})
	if err == nil {
		t.Fatal("expected error for corrupt identity")
	}
}

// TestPullWorkspaceBrokerCorruptPeerStore hits the
// openSandboxdPeerStore error branch.
func TestPullWorkspaceBrokerCorruptPeerStore(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrGenerateSandboxd(dir); err != nil {
		t.Fatalf("loadOrGenerate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, peersFilename), []byte("not json"), 0o600); err != nil {
		t.Fatalf("WriteFile peers: %v", err)
	}
	_, _, err := pullWorkspaceBroker(context.Background(), pullWorkspaceBrokerOpts{
		orchestratorURL: testFakeOrchURL, workspace: "ws", sandboxID: "sb",
		serveFP: "sha256:" + strings.Repeat("AB:", 31) + "AB", configDir: dir,
	})
	if err == nil {
		t.Fatal("expected error for corrupt peer store")
	}
}

// TestPullWorkspaceBrokerBadServeFP hits the ParseFingerprint
// branch.
func TestPullWorkspaceBrokerBadServeFP(t *testing.T) {
	dir := t.TempDir()
	_, _, err := pullWorkspaceBroker(context.Background(), pullWorkspaceBrokerOpts{
		orchestratorURL: testFakeOrchURL, workspace: "ws", sandboxID: "sb",
		serveFP: "not-a-fingerprint", configDir: dir,
	})
	if err == nil {
		t.Fatal("expected error for bad serve fingerprint")
	}
	if !strings.Contains(err.Error(), "parse serve fingerprint") {
		t.Errorf("error = %v, want parse error", err)
	}
}

// TestPullWorkspaceBrokerEmptyURL hits the brokerclient.New error
// branch (empty URL).
func TestPullWorkspaceBrokerEmptyURL(t *testing.T) {
	dir := t.TempDir()
	_, _, err := pullWorkspaceBroker(context.Background(), pullWorkspaceBrokerOpts{
		orchestratorURL: "", workspace: "ws", sandboxID: "sb",
		serveFP: "sha256:" + strings.Repeat("AB:", 31) + "AB", configDir: dir,
	})
	if err == nil {
		t.Fatal("expected error for empty orchestrator URL")
	}
}

// TestPullWorkspaceBrokerCasOpenError forces cas.Open to fail by
// blocking the cas dir with a regular file.
func TestPullWorkspaceBrokerCasOpenError(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrGenerateSandboxd(dir); err != nil {
		t.Fatalf("loadOrGenerate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cas"), []byte("blocker"), 0o600); err != nil {
		t.Fatalf("plant blocker: %v", err)
	}
	_, _, err := pullWorkspaceBroker(context.Background(), pullWorkspaceBrokerOpts{
		orchestratorURL: testFakeOrchURL, workspace: "ws", sandboxID: "sb",
		serveFP: "sha256:" + strings.Repeat("AB:", 31) + "AB", configDir: dir,
	})
	if err == nil {
		t.Fatal("expected cas.Open error")
	}
}

// TestPullWorkspaceBrokerUnreachableOrchestrator pre-cancels ctx so
// the broker connect fails cleanly inside AttachAndDial.
func TestPullWorkspaceBrokerUnreachableOrchestrator(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := pullWorkspaceBroker(ctx, pullWorkspaceBrokerOpts{
		orchestratorURL: "https://127.0.0.1:1", workspace: "ws", sandboxID: "sb",
		serveFP: "sha256:" + strings.Repeat("AB:", 31) + "AB", configDir: dir,
	})
	if err == nil {
		t.Fatal("expected error against unreachable orchestrator")
	}
}
