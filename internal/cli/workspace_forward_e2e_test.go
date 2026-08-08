package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/nodectrlclient"
	"github.com/richiejp/darkaifactory/internal/orchestrator"
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

const (
	e2eForwardWorkspaceName = "ws-e2e-fwd"
	e2eForwardWorkspaceUUID = "ws-uuid-e2e-fwd"
	e2eForwardContainerPort = uint32(8080)
	e2eForwardLoopback      = "127.0.0.1"
)

// TestWorkspaceForwardEndToEnd wires every component built across
// phases B and C and round-trips bytes through them:
//
//	test client → CLI local TCP listener
//	  → daft workspace forward → ForwardPort bidi RPC
//	  → orchestrator runForwardPort → OpenPortBridge
//	  → push OpenPortRequest on registered sandboxd's RegisterNode
//	    stream → sandboxd dispatch
//	  → nodectrlclient.handleOpenPort → OpenPortStream RPC
//	  → Bridger.Bridge → DialFunc → in-process echo net.Pipe
//
// Each layer has its own unit tests; this test catches integration
// regressions — proto rename ripples, missed Hello-shape changes,
// cross-cancellation deadlocks, etc. — across the whole chain over
// real Connect + mTLS + HTTP/2.
func TestWorkspaceForwardEndToEnd(t *testing.T) {
	rig := newForwardE2ERig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// listenLocal binds 127.0.0.1:0 and stashes the resulting addr so
	// the test client can dial it without a seed-then-rebind race.
	// opts.localPort is set to a non-zero placeholder; the override
	// ignores it.
	var localAddr atomic.Value
	listened := make(chan struct{})
	opts := workspaceForwardOpts{
		orchestrator:  rig.orchURL,
		bind:          e2eForwardLoopback,
		localPort:     1, // placeholder; listenLocal override binds :0
		containerPort: e2eForwardContainerPort,
		workspace:     e2eForwardWorkspaceName,
	}
	deps := forwardDeps{
		dialOrchestrator: func(_, _ string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
			return rig.cliClient, nil
		},
		listenLocal: func(_ string) (net.Listener, error) {
			ln, err := net.Listen("tcp", e2eForwardLoopback+":0")
			if err == nil {
				localAddr.Store(ln.Addr().String())
				close(listened)
			}
			return ln, err
		},
		openStream: func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient) forwardClientStream {
			return c.ForwardPort(ctx)
		},
	}

	var stdout bytes.Buffer
	cliDone := make(chan error, 1)
	go func() {
		cliDone <- runWorkspaceForwardWith(ctx, &stdout, opts, deps)
	}()

	// Wait for the listener to be bound (deps.listenLocal closes
	// `listened` after a successful bind) before dialing.
	select {
	case <-listened:
	case <-time.After(3 * time.Second):
		t.Fatal("CLI listener did not bind within 3s")
	}
	addr, ok := localAddr.Load().(string)
	if !ok || addr == "" {
		t.Fatal("local listener address not stored")
	}

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial CLI listener %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()

	want := []byte("ping-e2e-c3.3")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("conn.Write: %v", err)
	}
	got := make([]byte, len(want))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("conn.Read: %v\nstdout: %s", err, stdout.String())
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}

	// Close the test conn first so the per-tunnel pump unblocks on
	// its local Read (peer-half-close → io.EOF). Then cancel ctx;
	// the listener watchdog closes the listener, accept loop drains
	// the tunnel WaitGroup and returns nil.
	_ = conn.Close()
	cancel()
	select {
	case err := <-cliDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("runWorkspaceForwardWith: %v\nstdout: %s", err, stdout.String())
		}
	case <-time.After(5 * time.Second):
		t.Errorf("CLI did not return after ctx cancel\nstdout: %s", stdout.String())
	}
}

// forwardE2ERig bundles the orchestrator URL + the CLI-side Connect
// client (already authenticated over mTLS). The orchestrator,
// sandboxd, and echo loop run in goroutines registered to t.Cleanup.
type forwardE2ERig struct {
	orchURL   string
	cliClient workspacev1connect.WorkspaceCatalogServiceClient
}

func newForwardE2ERig(t *testing.T) *forwardE2ERig {
	t.Helper()

	srvID, sbID, cliID := generateForwardE2EIdentities(t)
	srvPeers := buildSrvPeers(t, sbID, cliID)
	reg := nodectrl.NewRegistry(nil)
	cat := workspacecatalog.NewStore(nil)
	bridger := buildForwardE2EBridger(t)

	orchURL := startForwardE2EOrchestrator(t, srvID, srvPeers, reg, cat)
	startForwardE2ESandboxd(t, orchURL, sbID, srvID, bridger, reg)
	seedRunningWorkspace(t, cat, cliID.Fingerprint(), sbID.Fingerprint())
	cliClient := buildForwardE2EClient(t, cliID, srvID, orchURL)

	return &forwardE2ERig{orchURL: orchURL, cliClient: cliClient}
}

func generateForwardE2EIdentities(t *testing.T) (srv, sb, cli *identity.Identity) {
	t.Helper()
	srv, err := identity.Generate(identity.RoleOrchestrator)
	if err != nil {
		t.Fatalf("Generate orchestrator: %v", err)
	}
	sb, err = identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate sandbox: %v", err)
	}
	cli, err = identity.Generate(identity.RoleClient)
	if err != nil {
		t.Fatalf("Generate client: %v", err)
	}
	return srv, sb, cli
}

func buildSrvPeers(t *testing.T, sbID, cliID *identity.Identity) *identity.PeerStore {
	t.Helper()
	srvPeers, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "srv-peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore srv: %v", err)
	}
	if err := srvPeers.Add(identity.Peer{Fingerprint: sbID.Fingerprint(), Role: identity.RoleSandboxHost}); err != nil {
		t.Fatalf("approve sb: %v", err)
	}
	if err := srvPeers.Add(identity.Peer{Fingerprint: cliID.Fingerprint(), Role: identity.RoleClient}); err != nil {
		t.Fatalf("approve cli: %v", err)
	}
	return srvPeers
}

// buildForwardE2EBridger creates a Bridger with one DialFunc that
// returns the "remote" end of a net.Pipe; the "local" end runs an
// echo loop. Every byte the bridge tunnels in comes back out.
func buildForwardE2EBridger(t *testing.T) *sandboxgateway.Bridger {
	t.Helper()
	bridger := sandboxgateway.New()
	dsLocal, dsRemote := net.Pipe()
	t.Cleanup(func() { _ = dsLocal.Close() })
	go func() {
		buf := make([]byte, 64)
		//daft:lint:disable=forloopbound reason="exits on dsLocal close (Read returns err)"
		for {
			n, err := dsLocal.Read(buf)
			if err != nil {
				return
			}
			if _, werr := dsLocal.Write(buf[:n]); werr != nil {
				return
			}
		}
	}()
	dial := func(context.Context) (net.Conn, error) { return dsRemote, nil }
	if err := bridger.Register(e2eForwardWorkspaceUUID, e2eForwardContainerPort, dial); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return bridger
}

func startForwardE2EOrchestrator(
	t *testing.T,
	srvID *identity.Identity,
	srvPeers *identity.PeerStore,
	reg *nodectrl.Registry,
	cat *workspacecatalog.Store,
) string {
	t.Helper()
	// Leaving cfg.WorkspacePortBridger nil → resolvePortBridger
	// returns the orchestrator's own SandboxControl handler, which is
	// the production path.
	srv, err := orchestrator.New(orchestrator.Config{
		Identity:         srvID,
		Peers:            srvPeers,
		Addr:             loopbackAddrAny,
		NodeRegistry:     reg,
		WorkspaceCatalog: cat,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
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
	return "https://" + srv.Addr()
}

func startForwardE2ESandboxd(
	t *testing.T,
	orchURL string,
	sbID, srvID *identity.Identity,
	bridger *sandboxgateway.Bridger,
	reg *nodectrl.Registry,
) {
	t.Helper()
	sbPeers, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "sb-peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore sb: %v", err)
	}
	if err := sbPeers.Add(identity.Peer{Fingerprint: srvID.Fingerprint()}); err != nil {
		t.Fatalf("sb approve srv: %v", err)
	}
	sbClient, err := nodectrlclient.New(nodectrlclient.Config{
		URL:               orchURL,
		Identity:          sbID,
		Peers:             sbPeers,
		Version:           "test-e2e",
		HeartbeatInterval: 100 * time.Millisecond,
		Bridger:           bridger,
		Stderr:            io.Discard,
	})
	if err != nil {
		t.Fatalf("nodectrlclient.New: %v", err)
	}
	sbCtx, sbCancel := context.WithCancel(context.Background())
	sbDone := make(chan error, 1)
	go func() {
		sbDone <- sbClient.RegisterAndServe(sbCtx, nodectrl.Resources{CPUs: 1, MemoryBytes: 1 << 20})
	}()
	t.Cleanup(func() {
		sbCancel()
		select {
		case err := <-sbDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Logf("sandboxd: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("sandboxd did not return after cancel")
		}
	})
	// Heartbeat refresh is the level-triggered signal that the
	// RegisterNode stream is fully attached on both ends — without
	// it OpenPortBridge would race the stream-attach.
	waitForForwardHeartbeat(t, reg, sbID.Fingerprint())
}

// seedRunningWorkspace inserts a workspace owned by ownerFP and
// stamps it as RUNNING on the sandboxd identified by nodeFP.
func seedRunningWorkspace(
	t *testing.T,
	cat *workspacecatalog.Store,
	ownerFP, nodeFP identity.Fingerprint,
) {
	t.Helper()
	if _, err := cat.Create(workspacecatalog.CreateInput{
		UUID:             e2eForwardWorkspaceUUID,
		Name:             e2eForwardWorkspaceName,
		OwnerFingerprint: ownerFP,
	}); err != nil {
		t.Fatalf("catalog.Create: %v", err)
	}
	if _, err := cat.Update(e2eForwardWorkspaceUUID, workspacecatalog.MutateStatus,
		func(ws *workspacecatalog.Workspace) error {
			ws.Status.ObservedGeneration = ws.Metadata.Generation
			ws.Status.CurrentSandbox = &workspacecatalog.SandboxRef{
				UID:             "sb-1",
				NodeFingerprint: nodeFP,
			}
			return nil
		}); err != nil {
		t.Fatalf("catalog.Update: %v", err)
	}
}

func buildForwardE2EClient(
	t *testing.T,
	cliID, srvID *identity.Identity,
	orchURL string,
) workspacev1connect.WorkspaceCatalogServiceClient {
	t.Helper()
	cliPeers, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "cli-peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore cli: %v", err)
	}
	if err := cliPeers.Add(identity.Peer{Fingerprint: srvID.Fingerprint()}); err != nil {
		t.Fatalf("cli approve srv: %v", err)
	}
	tlsCfg, err := identity.ClientTLSConfig(cliID, cliPeers, "")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	httpc := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}}
	return workspacev1connect.NewWorkspaceCatalogServiceClient(httpc, orchURL)
}

func waitForForwardHeartbeat(t *testing.T, reg *nodectrl.Registry, fp identity.Fingerprint) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	//daft:lint:disable=forloopbound reason="exits on heartbeat refresh or 3s deadline"
	for time.Now().Before(deadline) {
		if e, ok := reg.Lookup(fp); ok && e.LastSeenAt.After(e.RegisteredAt) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("orchestrator did not observe a heartbeat refresh within 3s")
}
