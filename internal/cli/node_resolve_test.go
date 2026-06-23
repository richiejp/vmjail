package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/localai-org/darkaifactory/internal/genpb/nodectrl/v1/nodectrlv1connect"
	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/nodectrl"
)

// fpFromHexPrefix builds a fingerprint whose canonical hex starts with
// prefixHex, zero-padded — so tests can craft deterministic prefixes.
func fpFromHexPrefix(t *testing.T, prefixHex string) identity.Fingerprint {
	t.Helper()
	full := prefixHex + strings.Repeat("0", 64-len(prefixHex))
	fp, err := identity.ParseFingerprint(full)
	if err != nil {
		t.Fatalf("ParseFingerprint(%q): %v", full, err)
	}
	return fp
}

// nodeResolveClient stands up an in-process orchestrator seeded with
// seed, approves it client-side, and returns a NodeControl client
// dialed against it — the wiring resolveNodeSelector runs against.
func nodeResolveClient(t *testing.T, seed func(*nodectrl.Registry)) nodectrlv1connect.NodeControlServiceClient {
	t.Helper()
	clientDir := t.TempDir()
	cliID, err := loadOrGenerateClient(clientDir)
	if err != nil {
		t.Fatalf("loadOrGenerateClient: %v", err)
	}
	srvStore, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	if err := srvStore.Add(identity.Peer{Fingerprint: cliID.Fingerprint()}); err != nil {
		t.Fatalf("server Add: %v", err)
	}
	addr, srvID := runOrchestratorWithNodeRegistry(t, srvStore, seed)
	if _, _, err := runDaft(t, authCmdName, "--config-dir", clientDir, "approve",
		srvID.Fingerprint().String(), "--role", "orchestrator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	client, err := newNodeControlClient("https://"+addr, clientDir)
	if err != nil {
		t.Fatalf("newNodeControlClient: %v", err)
	}
	return client
}

func TestResolveNodeSelectorByHostnameAndPrefix(t *testing.T) {
	fpAlpha := fpFromHexPrefix(t, "01ab")
	fpBeta := fpFromHexPrefix(t, "02cd")
	client := nodeResolveClient(t, func(reg *nodectrl.Registry) {
		reg.Register(fpAlpha, "alpha", "v1", nodectrl.Resources{CPUs: 1})
		reg.Register(fpBeta, "beta", "v1", nodectrl.Resources{CPUs: 1})
	})
	ctx := context.Background()

	// By hostname.
	if got, err := resolveNodeSelector(ctx, client, "alpha"); err != nil || got != fpAlpha.String() {
		t.Errorf("by hostname = (%q, %v), want %s", got, err, fpAlpha.String())
	}
	// By unique fingerprint prefix (only fpAlpha starts with 01).
	if got, err := resolveNodeSelector(ctx, client, "01"); err != nil || got != fpAlpha.String() {
		t.Errorf("by prefix = (%q, %v), want %s", got, err, fpAlpha.String())
	}
	// Full fingerprint short-circuits (canonicalised, no lookup needed).
	if got, err := resolveNodeSelector(ctx, client, fpBeta.String()); err != nil || got != fpBeta.String() {
		t.Errorf("by full fp = (%q, %v), want %s", got, err, fpBeta.String())
	}
	// Empty clears.
	if got, err := resolveNodeSelector(ctx, client, ""); err != nil || got != "" {
		t.Errorf("empty = (%q, %v), want \"\"", got, err)
	}
	// No match.
	if _, err := resolveNodeSelector(ctx, client, "nope"); err == nil ||
		!strings.Contains(err.Error(), "no registered node") {
		t.Errorf("no-match err = %v, want 'no registered node'", err)
	}
}

func TestResolveNodeSelectorAmbiguous(t *testing.T) {
	// Two nodes sharing the hostname "dup" and the fingerprint prefix "ab".
	fp1 := fpFromHexPrefix(t, "abcd")
	fp2 := fpFromHexPrefix(t, "abce")
	client := nodeResolveClient(t, func(reg *nodectrl.Registry) {
		reg.Register(fp1, "dup", "v1", nodectrl.Resources{CPUs: 1})
		reg.Register(fp2, "dup", "v1", nodectrl.Resources{CPUs: 1})
	})
	ctx := context.Background()

	_, err := resolveNodeSelector(ctx, client, "dup")
	if err == nil || !strings.Contains(err.Error(), "ambiguous hostname") {
		t.Errorf("ambiguous hostname err = %v", err)
	}
	_, err = resolveNodeSelector(ctx, client, "ab")
	if err == nil || !strings.Contains(err.Error(), "ambiguous fingerprint prefix") {
		t.Errorf("ambiguous prefix err = %v", err)
	}
	// A longer prefix disambiguates.
	if got, err := resolveNodeSelector(ctx, client, "abcd"); err != nil || got != fp1.String() {
		t.Errorf("disambiguated = (%q, %v), want %s", got, err, fp1.String())
	}
}

// TestResolveTargetSelectorShortCircuits: a full fingerprint and an
// empty selector resolve without any orchestrator dial (so empty opts
// are fine — no URL needed).
func TestResolveTargetSelectorShortCircuits(t *testing.T) {
	fp := fpFromHexPrefix(t, "dead")
	if got, err := resolveTargetSelector(context.Background(), workspaceClientOpts{}, fp.String()); err != nil || got != fp.String() {
		t.Errorf("full fp = (%q, %v), want %s", got, err, fp.String())
	}
	if got, err := resolveTargetSelector(context.Background(), workspaceClientOpts{}, ""); err != nil || got != "" {
		t.Errorf("empty = (%q, %v), want \"\"", got, err)
	}
}

// TestWorkspaceTargetResolvesHostname is the integration proof: `daft
// workspace target <hostname>` dials NodeControl, resolves the hostname
// to the node's full fingerprint, and stores that — the command's
// summary echoes the resolved canonical fingerprint.
func TestWorkspaceTargetResolvesHostname(t *testing.T) {
	clientDir := t.TempDir()
	cliID, err := loadOrGenerateClient(clientDir)
	if err != nil {
		t.Fatalf("loadOrGenerateClient: %v", err)
	}
	srvStore, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	if err := srvStore.Add(identity.Peer{Fingerprint: cliID.Fingerprint()}); err != nil {
		t.Fatalf("server Add: %v", err)
	}
	nodeFP := fpFromHexPrefix(t, "0abc")
	addr, srvID := runOrchestratorWithNodeRegistry(t, srvStore, func(reg *nodectrl.Registry) {
		reg.Register(nodeFP, "node-x", "v1", nodectrl.Resources{CPUs: 1})
	})
	if _, _, err := runDaft(t, authCmdName, "--config-dir", clientDir, "approve",
		srvID.Fingerprint().String(), "--role", "orchestrator"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	orch := "https://" + addr
	if _, _, err := runDaft(t, "workspace", "create", "--name", "w",
		"--orchestrator", orch, "--config-dir", clientDir); err != nil {
		t.Fatalf("create: %v", err)
	}

	stdout, _, err := runDaft(t, "workspace", "target", "node-x", "--name", "w",
		"--orchestrator", orch, "--config-dir", clientDir)
	if err != nil {
		t.Fatalf("target by hostname: %v", err)
	}
	if !strings.Contains(stdout, nodeFP.String()) {
		t.Errorf("target summary should echo the resolved fingerprint %s, got:\n%s",
			nodeFP.String(), stdout)
	}
}
