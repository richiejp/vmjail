package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/brokerclient"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/localai-org/darkaifactory/internal/identity"

	"connectrpc.com/connect"
)

// TestWorkspaceServeBrokeredRoundTrip drives `daft workspace serve
// --orchestrator <url>` end-to-end: start an orchestrator,
// register the workspace via the cobra command, then attach as a
// sandbox-host (using brokerclient directly) and pull a manifest
// through inner mTLS.
func TestWorkspaceServeBrokeredRoundTrip(t *testing.T) {
	clientDir, cliID, sbID, addr, srvID := setupBrokerTrio(t)
	src := writeWorkspaceFixture(t)
	startServeBrokered(t, clientDir, addr, src)

	ctx, attachCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer attachCancel()

	sbClient := mustBrokerClient(t, sbID, srvID, addr)
	userClient := mustBrokerClient(t, cliID, srvID, addr)
	attached, attachErr := startAttachAndDial(ctx, sbClient, "ws", "sb-1", cliID.Fingerprint())
	if _, _, err := userClient.Attach(ctx, cliID.Fingerprint(), "ws", "sb-1"); err != nil {
		t.Fatalf("user Attach: %v", err)
	}
	ic := awaitAttach(t, attached, attachErr)
	defer func() { _ = ic.Close() }()

	wsc := workspacev1connect.NewWorkspaceServiceClient(ic.HTTPClient, ic.BaseURL)
	resp, err := wsc.GetManifest(ctx, connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: "ws"}))
	if err != nil {
		t.Fatalf("inner GetManifest: %v", err)
	}
	if got := len(resp.Msg.GetManifest().GetEntries()); got != 2 {
		t.Errorf("manifest entries = %d, want 2 (alpha.txt + blob.bin)", got)
	}
}

// setupBrokerTrio stands up an orchestrator + user-client +
// sandbox-client identity triple, all mutually approved, returning
// the bits the brokered round-trip test needs.
func setupBrokerTrio(t *testing.T) (clientDir string, cliID, sbID *identity.Identity, orchAddr string, srvID *identity.Identity) {
	t.Helper()
	clientDir = t.TempDir()
	cliID, err := loadOrGenerateClient(clientDir)
	if err != nil {
		t.Fatalf("loadOrGenerateClient: %v", err)
	}
	sbID, err = identity.Generate(identity.RoleClient)
	if err != nil {
		t.Fatalf("Generate sb: %v", err)
	}
	srvStore, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	for _, fp := range []identity.Fingerprint{cliID.Fingerprint(), sbID.Fingerprint()} {
		if err := srvStore.Add(identity.Peer{Fingerprint: fp}); err != nil {
			t.Fatalf("approve: %v", err)
		}
	}
	orchAddr, srvID = runOrchestrator(t, srvStore)
	if _, _, err := runDaft(t, authCmdName, "--config-dir", clientDir, "approve",
		srvID.Fingerprint().String(), "--role", "orchestrator"); err != nil {
		t.Fatalf("approve orch: %v", err)
	}
	return clientDir, cliID, sbID, orchAddr, srvID
}

func writeWorkspaceFixture(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "alpha.txt"), []byte("alpha\n"))
	mustWriteFile(t, filepath.Join(src, "blob.bin"), bytes.Repeat([]byte("Z"), 4096))
	return src
}

func startServeBrokered(t *testing.T, clientDir, addr, src string) {
	t.Helper()
	stdout, stderr := &syncBuf{}, &syncBuf{}
	cmd := NewRootCmd()
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs([]string{
		workspaceCmdName, serveSubName,
		"--workspace=ws=" + src,
		"--orchestrator=https://" + addr,
		configDirArg, clientDir,
		"--cas-dir=" + filepath.Join(clientDir, "cas"),
	})
	serveCtx, cancel := context.WithCancel(context.Background())
	cmd.SetContext(serveCtx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := cmd.Execute(); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("serve: %v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
		}
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(stdout.String(), "brokered \"ws\"")
	})
}

// startAttachAndDial spawns the sandbox-side AttachAndDial half in a
// goroutine and gives the orchestrator a beat to land its
// RegisterNode subscription before returning. The sibling
// `awaitAttach` collects the result with a fatal-on-timeout.
//
// The 200ms sleep is a stand-in for "the sandbox's RegisterNode
// has been picked up by the matcher"; a non-sleep alternative
// requires lifting matcher introspection through runOrchestrator.
func startAttachAndDial(
	ctx context.Context,
	sbClient *brokerclient.Client,
	workspace, sandboxID string,
	serveFP identity.Fingerprint,
) (attached <-chan *brokerclient.InnerClient, attachErr <-chan error) {
	att := make(chan *brokerclient.InnerClient, 1)
	errCh := make(chan error, 1)
	go func() {
		ic, err := sbClient.AttachAndDial(ctx, brokerclient.AttachOptions{
			Workspace: workspace, SandboxID: sandboxID,
			ServeFingerprint: serveFP,
		})
		if err != nil {
			errCh <- err
			return
		}
		att <- ic
	}()
	time.Sleep(200 * time.Millisecond)
	return att, errCh
}

func awaitAttach(t *testing.T, attached <-chan *brokerclient.InnerClient, attachErr <-chan error) *brokerclient.InnerClient {
	t.Helper()
	select {
	case ic := <-attached:
		return ic
	case err := <-attachErr:
		t.Fatalf("AttachAndDial: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("AttachAndDial didn't return")
	}
	return nil
}

func mustBrokerClient(t *testing.T, peerID, orchID *identity.Identity, addr string) *brokerclient.Client {
	t.Helper()
	store := identity.NewMemoryPeerStore(orchID.Fingerprint())
	c, err := brokerclient.New(brokerclient.Config{
		URL: "https://" + addr, Identity: peerID, Peers: store,
	})
	if err != nil {
		t.Fatalf("brokerclient.New: %v", err)
	}
	return c
}

// TestWorkspaceServeBrokeredUnreachable runs runWorkspaceServe in
// broker mode against an unreachable orchestrator URL with a
// pre-cancelled context so RegisterServe fails fast. Covers the
// ServeBrokered error path that isn't reached by the happy-path
// round-trip test.
func TestWorkspaceServeBrokeredUnreachable(t *testing.T) {
	clientDir := t.TempDir()
	if _, err := loadOrGenerateClient(clientDir); err != nil {
		t.Fatalf("loadOrGenerateClient: %v", err)
	}
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "alpha.txt"), []byte("a"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runWorkspaceServe(ctx, &syncBuf{}, workspaceServeOpts{
		workspaceFlag: "ws=" + src,
		orchestrator:  "https://127.0.0.1:1",
		configDir:     clientDir,
		casDir:        filepath.Join(clientDir, "cas"),
	})
	if err == nil {
		t.Error("expected error against unreachable orchestrator")
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}
