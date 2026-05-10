package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/orchestrator"
)

// runOrchestrator boots an in-process orchestrator listening on a
// loopback ephemeral port. Returns its address and the on-disk
// fingerprint a client would need to approve.
func runOrchestrator(t *testing.T, srvStore *identity.PeerStore) (addr string, srvID *identity.Identity) {
	t.Helper()
	srvID, err := identity.Generate(identity.RoleOrchestrator)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	srv, err := orchestrator.New(orchestrator.Config{
		Identity: srvID,
		Peers:    srvStore,
		Addr:     "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() {
		if err := srv.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv.Addr(), srvID
}

func TestPingApprovedRoundTrip(t *testing.T) {
	clientDir := t.TempDir()

	// Server-side: open a peers file and approve the client. We
	// have to know the client's fingerprint up front so we drive
	// the client identity through loadOrGenerateClient first.
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

	addr, srvID := runOrchestrator(t, srvStore)

	// Client-side: approve the orchestrator we just started.
	if _, _, err := runDaft(t, authCmdName, "--config-dir", clientDir, "approve",
		srvID.Fingerprint().String(), "--role", "orchestrator"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	stdout, _, err := runDaft(t, "ping", addr,
		"--config-dir", clientDir, "--message", "hello", "--timeout", "3s")
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if !strings.Contains(stdout, "pong: hello") {
		t.Errorf("stdout = %q, want 'pong: hello'", stdout)
	}
}

func TestPingFailsWhenServerNotApproved(t *testing.T) {
	clientDir := t.TempDir()
	if _, err := loadOrGenerateClient(clientDir); err != nil {
		t.Fatalf("loadOrGenerateClient: %v", err)
	}

	// Server approves the client, but the client doesn't approve
	// the server: the handshake must fail on the client side.
	cliID, _ := loadOrGenerateClient(clientDir)
	srvStore, _ := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err := srvStore.Add(identity.Peer{Fingerprint: cliID.Fingerprint()}); err != nil {
		t.Fatalf("server Add: %v", err)
	}
	addr, _ := runOrchestrator(t, srvStore)

	_, _, err := runDaft(t, "ping", addr, "--config-dir", clientDir, "--timeout", "2s")
	if err == nil {
		t.Fatal("expected ping to fail when server is unapproved")
	}
}

func TestPingFailsWhenClientNotApproved(t *testing.T) {
	clientDir := t.TempDir()
	cliID, err := loadOrGenerateClient(clientDir)
	if err != nil {
		t.Fatalf("loadOrGenerateClient: %v", err)
	}
	_ = cliID

	// Server peer-store empty; the orchestrator will reject the
	// client at handshake time.
	srvStore, _ := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	addr, srvID := runOrchestrator(t, srvStore)

	if _, _, err := runDaft(t, authCmdName, "--config-dir", clientDir, "approve",
		srvID.Fingerprint().String()); err != nil {
		t.Fatalf("approve: %v", err)
	}
	_, _, err = runDaft(t, "ping", addr, "--config-dir", clientDir, "--timeout", "2s")
	if err == nil {
		t.Fatal("expected ping to fail when client is unapproved")
	}
}

func TestRunPingPropagatesIdentityLoadError(t *testing.T) {
	// Plant a corrupt cert.pem so loadOrGenerateClient aborts.
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "cert.pem"), "garbage")
	mustWrite(t, filepath.Join(dir, "key.pem"), "garbage")

	var out bytes.Buffer
	err := runPing(context.Background(), &out, "127.0.0.1:1", "x", dir)
	if err == nil {
		t.Fatal("expected error for corrupt identity")
	}
}

func TestRunPingPropagatesStdoutWriteError(t *testing.T) {
	clientDir := t.TempDir()
	cliID, err := loadOrGenerateClient(clientDir)
	if err != nil {
		t.Fatalf("loadOrGenerateClient: %v", err)
	}
	srvStore, _ := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err := srvStore.Add(identity.Peer{Fingerprint: cliID.Fingerprint()}); err != nil {
		t.Fatalf("server Add: %v", err)
	}
	addr, srvID := runOrchestrator(t, srvStore)
	if _, _, err := runDaft(t, authCmdName, "--config-dir", clientDir, "approve",
		srvID.Fingerprint().String()); err != nil {
		t.Fatalf("approve: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runPing(ctx, failingWriter{}, addr, "x", clientDir); err == nil {
		t.Fatal("expected error when stdout write fails on success path")
	}
}

func TestRunPingPropagatesPeerStoreError(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrGenerateClient(dir); err != nil {
		t.Fatalf("loadOrGenerateClient: %v", err)
	}
	mustWrite(t, filepath.Join(dir, peersFilename), "not json")

	var out bytes.Buffer
	err := runPing(context.Background(), &out, "127.0.0.1:1", "x", dir)
	if err == nil {
		t.Fatal("expected error for corrupt peer store")
	}
}
