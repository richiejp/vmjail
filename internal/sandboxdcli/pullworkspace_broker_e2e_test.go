package sandboxdcli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/brokerclient"
	"github.com/localai-org/darkaifactory/internal/cas"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/orchestrator"
	"github.com/localai-org/darkaifactory/internal/workspace"
)

// TestPullWorkspaceBrokerHappyPath drives the full brokered pull
// pipeline: orchestrator + user-host's ServeBrokered + sandbox's
// pullWorkspaceBroker all wired through a real Connect-RPC stack.
// Verifies pullWorkspaceBroker returns a populated tmpdir.
const testSandboxID = "sb-1"

// TestPullWorkspaceBrokerMkdirTempError forces os.MkdirTemp to
// fail after a successful attach by pointing TMPDIR at a path that
// doesn't exist. Covers the rare-error branch between GetManifest
// success and ApplyManifest.
func TestPullWorkspaceBrokerMkdirTempError(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "alpha.txt"), []byte("alpha\n"))

	userDir, sbxDir := t.TempDir(), t.TempDir()
	userID, sbxID := mustLoadUserSandboxIDs(t, userDir, sbxDir)
	orchID, orchURL, orchSrv := mustStartOrchestrator(t, userID.Fingerprint(), sbxID.Fingerprint())
	mustApproveOrchOnBoth(t, userDir, sbxDir, orchID.Fingerprint())

	userPeers, err := identity.OpenPeerStore(filepath.Join(userDir, peersFilename))
	if err != nil {
		t.Fatalf("user peers: %v", err)
	}
	startBrokeredServe(t, userID, userPeers, orchURL, userDir, src)
	waitForServes(t, orchSrv, 1)

	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does", "not", "exist"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, _, err := pullWorkspaceBroker(ctx, pullWorkspaceBrokerOpts{
			orchestratorURL: orchURL,
			workspace:       "ws",
			sandboxID:       testSandboxID,
			serveFP:         userID.Fingerprint().String(),
			configDir:       sbxDir,
		})
		errCh <- err
	}()
	waitForSandboxes(t, orchSrv, 1)
	attachBC, err := brokerclient.New(brokerclient.Config{
		URL: orchURL, Identity: userID, Peers: userPeers,
	})
	if err != nil {
		t.Fatalf("attach brokerclient: %v", err)
	}
	if _, _, err := attachBC.Attach(ctx, userID.Fingerprint(), "ws", testSandboxID); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Error("expected MkdirTemp error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pullWorkspaceBroker didn't return")
	}
}

func TestPullWorkspaceBrokerHappyPath(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "alpha.txt"), []byte("alpha\n"))

	userDir, sbxDir := t.TempDir(), t.TempDir()
	userID, sbxID := mustLoadUserSandboxIDs(t, userDir, sbxDir)
	orchID, orchURL, orchSrv := mustStartOrchestrator(t, userID.Fingerprint(), sbxID.Fingerprint())
	mustApproveOrchOnBoth(t, userDir, sbxDir, orchID.Fingerprint())

	userPeers, err := identity.OpenPeerStore(filepath.Join(userDir, peersFilename))
	if err != nil {
		t.Fatalf("user peers: %v", err)
	}
	startBrokeredServe(t, userID, userPeers, orchURL, userDir, src)
	waitForServes(t, orchSrv, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dirCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		dir, cleanup, err := pullWorkspaceBroker(ctx, pullWorkspaceBrokerOpts{
			orchestratorURL: orchURL,
			workspace:       "ws",
			sandboxID:       testSandboxID,
			serveFP:         userID.Fingerprint().String(),
			configDir:       sbxDir,
		})
		if err != nil {
			errCh <- err
			return
		}
		t.Cleanup(cleanup)
		dirCh <- dir
	}()
	waitForSandboxes(t, orchSrv, 1)
	attachBC, err := brokerclient.New(brokerclient.Config{
		URL: orchURL, Identity: userID, Peers: userPeers,
	})
	if err != nil {
		t.Fatalf("attach brokerclient: %v", err)
	}
	if _, _, err := attachBC.Attach(ctx, userID.Fingerprint(), "ws", testSandboxID); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	var dir string
	select {
	case dir = <-dirCh:
	case err := <-errCh:
		t.Fatalf("pullWorkspaceBroker: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("pullWorkspaceBroker didn't return")
	}
	got, err := os.ReadFile(filepath.Join(dir, "alpha.txt")) //nolint:gosec // dir is t.TempDir().
	if err != nil {
		t.Fatalf("read alpha.txt: %v", err)
	}
	if string(got) != "alpha\n" {
		t.Errorf("alpha.txt = %q, want %q", got, "alpha\n")
	}
}

func mustLoadUserSandboxIDs(t *testing.T, userDir, sbxDir string) (userID, sbxID *identity.Identity) {
	t.Helper()
	userID, err := identity.LoadOrGenerate(userDir, identity.RoleClient)
	if err != nil {
		t.Fatalf("user identity: %v", err)
	}
	sbxID, err = loadOrGenerateSandboxd(sbxDir)
	if err != nil {
		t.Fatalf("sbx identity: %v", err)
	}
	return userID, sbxID
}

func mustStartOrchestrator(t *testing.T, fps ...identity.Fingerprint) (orchID *identity.Identity, url string, srv *orchestrator.Server) {
	t.Helper()
	orchStore, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	for _, fp := range fps {
		if err := orchStore.Add(identity.Peer{Fingerprint: fp}); err != nil {
			t.Fatalf("approve: %v", err)
		}
	}
	orchID, err = identity.Generate(identity.RoleOrchestrator)
	if err != nil {
		t.Fatalf("orch identity: %v", err)
	}
	srv, err = orchestrator.New(orchestrator.Config{
		Identity: orchID, Peers: orchStore, Addr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() {
		if err := srv.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("orch Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	})
	return orchID, "https://" + srv.Addr(), srv
}

func mustApproveOrchOnBoth(t *testing.T, userDir, sbxDir string, orchFP identity.Fingerprint) {
	t.Helper()
	userPeers, err := identity.OpenPeerStore(filepath.Join(userDir, peersFilename))
	if err != nil {
		t.Fatalf("user peers: %v", err)
	}
	if err := userPeers.Add(identity.Peer{Fingerprint: orchFP}); err != nil {
		t.Fatalf("user approve orch: %v", err)
	}
	sbxPeers, err := openSandboxdPeerStore(sbxDir)
	if err != nil {
		t.Fatalf("sbx peers: %v", err)
	}
	if err := sbxPeers.Add(identity.Peer{Fingerprint: orchFP}); err != nil {
		t.Fatalf("sbx approve orch: %v", err)
	}
}

func startBrokeredServe(t *testing.T, userID *identity.Identity, userPeers *identity.PeerStore, orchURL, userDir, src string) {
	t.Helper()
	store, err := cas.Open(filepath.Join(userDir, "cas"))
	if err != nil {
		t.Fatalf("cas.Open: %v", err)
	}
	manifest, err := workspace.BuildManifest(src, store, workspace.BuildOptions{})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	wsServer, err := workspace.NewServer(store)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := wsServer.Add("ws", manifest); err != nil {
		t.Fatalf("Add: %v", err)
	}
	mux := http.NewServeMux()
	path, handler := workspacev1connect.NewWorkspaceServiceHandler(wsServer)
	mux.Handle(path, handler)
	userBC, err := brokerclient.New(brokerclient.Config{
		URL: orchURL, Identity: userID, Peers: userPeers,
	})
	if err != nil {
		t.Fatalf("user brokerclient: %v", err)
	}
	serveCtx, serveCancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = userBC.ServeBrokered(serveCtx, brokerclient.ServeOptions{
			Workspace: "ws", Handler: mux,
		})
	}()
	t.Cleanup(func() { serveCancel(); wg.Wait() })
}

func waitForServes(t *testing.T, srv *orchestrator.Server, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		serves, _ := srv.Matcher().Counts()
		if serves >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never observed %d serves", want)
}

func waitForSandboxes(t *testing.T, srv *orchestrator.Server, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, sandboxes := srv.Matcher().Counts()
		if sandboxes >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never observed %d sandboxes", want)
}
