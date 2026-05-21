package sandboxdcli

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"github.com/localai-org/darkaifactory/internal/cas"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/workspace"
)

// TestWorkspaceRoundTripIntegration exercises the push + pull-back
// workspace pipeline with every leg in-process. Per the unit /
// integration / e2e taxonomy in CONTRIBUTING.md this is an
// integration test: the in-VM container is *simulated* by writing
// directly into the sandbox-host tmpdir (step 3). A real e2e check
// would boot a Cloud Hypervisor VM whose container does the writes
// over a virtiofs share, and lives behind the `linux,integration`
// build tag — that gap is intentionally still open here.
//
//  1. user-host has a workspace ./code and runs `daft workspace serve`
//  2. sandbox-host's `daft-sandboxd run --workspace-from=…
//     --keep-workspace` pulls that workspace into a tmpdir (here
//     simulated by calling pullWorkspaceFrom directly)
//  3. the in-VM container modifies a file in the tmpdir — this
//     simulates (NOT exercises) the rw virtiofs share the real VM has
//  4. sandbox-host then runs `daft-sandboxd workspace serve` on the
//     tmpdir
//  5. user-host pulls the modifications back via the existing
//     workspace.ApplyManifest pipeline
//  6. the round-tripped file's contents reflect the in-VM change
//
// All three identities (user-host, sandbox-host, user-host-pull-back)
// are separate and TOFU-approved against each other, mirroring the
// production deployment.
func TestWorkspaceRoundTripIntegration(t *testing.T) {
	// Step 1: user-host has a workspace.
	userSrc := t.TempDir()
	mustWriteFile(t, filepath.Join(userSrc, "alpha.txt"), []byte("from-user\n"))
	mustWriteFile(t, filepath.Join(userSrc, "subdir", "beta.txt"), []byte("beta-original\n"))

	userDir := t.TempDir()     // user-host serve identity (push side)
	sbxDir := t.TempDir()      // sandbox-host identity
	pullbackDir := t.TempDir() // user-host identity used for the pull-back side

	userID, err := identity.LoadOrGenerate(userDir, identity.RoleClient)
	if err != nil {
		t.Fatalf("user identity: %v", err)
	}
	sbxID, err := loadOrGenerateSandboxd(sbxDir)
	if err != nil {
		t.Fatalf("sandboxd identity: %v", err)
	}
	pullbackID, err := identity.LoadOrGenerate(pullbackDir, identity.RoleClient)
	if err != nil {
		t.Fatalf("pullback identity: %v", err)
	}

	// Mutual approval: user-host ↔ sandbox-host (push leg) and
	// sandbox-host ↔ pull-back-host (pull-back leg).
	mutualApprove(t, userDir, sbxID.Fingerprint(), peersFilename)
	mutualApprove(t, sbxDir, userID.Fingerprint(), peersFilename)
	mutualApprove(t, sbxDir, pullbackID.Fingerprint(), peersFilename)
	mutualApprove(t, pullbackDir, sbxID.Fingerprint(), peersFilename)

	// Step 1 (cont): start the user-host workspace serve.
	pushAddr := startWorkspaceServe(t, userDir, userSrc)

	// Step 2: sandbox-host pulls the workspace.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tmpdir, cleanup, err := pullWorkspaceFrom(ctx, pullWorkspaceOpts{
		addr: pushAddr, name: "ws", configDir: sbxDir,
	})
	if err != nil {
		t.Fatalf("pullWorkspaceFrom: %v", err)
	}
	defer cleanup()

	// Verify the push actually delivered the user's files.
	if got := mustRead(t, filepath.Join(tmpdir, "alpha.txt")); got != "from-user\n" {
		t.Fatalf("post-pull alpha.txt = %q, want from-user", got)
	}

	// Step 3: simulate the in-VM container modifying files. The
	// virtiofs share is read-write so writes from inside the VM
	// land in the same tmpdir on the sandbox-host.
	mustWriteFile(t, filepath.Join(tmpdir, "alpha.txt"), []byte("from-sandbox\n"))
	mustWriteFile(t, filepath.Join(tmpdir, "newfile.txt"), []byte("created-in-sandbox\n"))

	// Step 4: sandbox-host serves the modified tmpdir back.
	pullbackAddr := startSandboxdWorkspaceServe(t, sbxDir, tmpdir)

	// Step 5: user-host pulls the modifications back.
	dest := t.TempDir()
	pullbackWorkspace(t, pullbackDir, pullbackAddr, "ws", dest)

	// Step 6: the round-tripped state reflects the in-VM change.
	if got := mustRead(t, filepath.Join(dest, "alpha.txt")); got != "from-sandbox\n" {
		t.Errorf("round-tripped alpha.txt = %q, want from-sandbox", got)
	}
	if got := mustRead(t, filepath.Join(dest, "newfile.txt")); got != "created-in-sandbox\n" {
		t.Errorf("round-tripped newfile.txt = %q, want created-in-sandbox", got)
	}
	// The unmodified file should also survive the round trip.
	if got := mustRead(t, filepath.Join(dest, "subdir", "beta.txt")); got != "beta-original\n" {
		t.Errorf("round-tripped beta.txt = %q, want beta-original", got)
	}
}

// mutualApprove writes the peer fingerprint into <dir>/<peersFile>.
func mutualApprove(t *testing.T, dir string, fp identity.Fingerprint, peersFile string) {
	t.Helper()
	peers, err := identity.OpenPeerStore(filepath.Join(dir, peersFile))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	if err := peers.Add(identity.Peer{Fingerprint: fp}); err != nil {
		t.Fatalf("Add: %v", err)
	}
}

// startSandboxdWorkspaceServe launches `daft-sandboxd workspace
// serve` in a goroutine on path and returns the bound addr. The
// listener is shut down on test cleanup.
func startSandboxdWorkspaceServe(t *testing.T, sbxDir, path string) string {
	t.Helper()
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuf{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := runWorkspaceServe(ctx, out, path, "ws", addr, sbxDir)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("sandboxd serve: %v", err)
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "fingerprint") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "fingerprint") {
		cancel()
		wg.Wait()
		t.Fatalf("sandboxd workspace serve never started: %q", out.String())
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return addr
}

// pullbackWorkspace dials the sandbox-host's workspace serve as the
// user-host pull-back peer and reconstructs the workspace at dest.
// Mirrors what the user runs as `daft workspace pull --from <addr>
// --workspace <name> <dest>` on their laptop.
func pullbackWorkspace(t *testing.T, pullbackDir, addr, name, dest string) {
	t.Helper()
	id, err := identity.LoadOrGenerate(pullbackDir, identity.RoleClient)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	peers, err := identity.OpenPeerStore(filepath.Join(pullbackDir, peersFilename))
	if err != nil {
		t.Fatalf("peers: %v", err)
	}
	tlsCfg, err := identity.ClientTLSConfig(id, peers, "")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	store, err := cas.Open(filepath.Join(pullbackDir, "cas"))
	if err != nil {
		t.Fatalf("cas.Open: %v", err)
	}
	httpc := &http.Client{Transport: pullbackTransport(tlsCfg)}
	client := workspacev1connect.NewWorkspaceServiceClient(httpc, "https://"+addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := client.GetManifest(ctx,
		connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: name}))
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := workspace.ApplyManifest(ctx, client, store, resp.Msg.GetManifest(),
		dest, workspace.ApplyOptions{}); err != nil {
		t.Fatalf("ApplyManifest: %v", err)
	}
}

func pullbackTransport(tlsCfg *tls.Config) *http2.Transport {
	return &http2.Transport{TLSClientConfig: tlsCfg}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // path is under t.TempDir().
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(b)
}
