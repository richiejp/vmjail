package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/richiejp/darkaifactory/internal/brokerclient"
	"github.com/richiejp/darkaifactory/internal/cas"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/orchestrator"
	"github.com/richiejp/darkaifactory/internal/workspace"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

// workRemote is the in-VM target the sync tests map their rw mount
// to; hoisted so goconst stops flagging the repeated literal.
const workRemote = "/work"

// syncE2ERig is a full push/pull harness: an orchestrator with a
// workspace catalog + broker matcher, a node hosting a CAS-backed
// WorkspaceService over the broker, and an on-disk client identity.
type syncE2ERig struct {
	orchURL   string
	clientDir string
	nodeFP    string
}

// newSyncE2ERig stands up the orchestrator, the broker-served node,
// and the client config dir, and approves every edge. The node's
// ServeBrokeredAsSandbox runs for the test's lifetime; the returned
// nodeFP is what `daft workspace target` is pointed at.
func newSyncE2ERig(t *testing.T) *syncE2ERig {
	t.Helper()
	srvID, err := identity.Generate(identity.RoleOrchestrator)
	if err != nil {
		t.Fatalf("Generate orchestrator: %v", err)
	}
	nodeID, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate node: %v", err)
	}
	clientDir := t.TempDir()
	cliID, err := loadOrGenerateClient(clientDir)
	if err != nil {
		t.Fatalf("loadOrGenerateClient: %v", err)
	}

	srvStore, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "srv-peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore srv: %v", err)
	}
	for _, fp := range []identity.Fingerprint{cliID.Fingerprint(), nodeID.Fingerprint()} {
		if err := srvStore.Add(identity.Peer{Fingerprint: fp}); err != nil {
			t.Fatalf("srv approve: %v", err)
		}
	}

	srv := startSyncOrchestrator(t, srvID, srvStore)
	orchURL := "https://" + srv.Addr()

	// Client approves the orchestrator so its on-disk peer store
	// trusts the broker + catalog endpoints.
	if _, _, err := runDaft(t, authCmdName, configDirArg, clientDir, "approve",
		srvID.Fingerprint().String(), "--role", "orchestrator"); err != nil {
		t.Fatalf("client approve orchestrator: %v", err)
	}

	startSyncNode(t, orchURL, nodeID, srvID, srv)

	return &syncE2ERig{orchURL: orchURL, clientDir: clientDir, nodeFP: nodeID.Fingerprint().String()}
}

func startSyncOrchestrator(t *testing.T, srvID *identity.Identity, srvStore *identity.PeerStore) *orchestrator.Server {
	t.Helper()
	srv, err := orchestrator.New(orchestrator.Config{
		Identity:         srvID,
		Peers:            srvStore,
		Addr:             loopbackAddrAny,
		WorkspaceCatalog: workspacecatalog.NewStore(nil),
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
	return srv
}

// startSyncNode runs the node side: a brokerclient hosting a
// CAS-backed WorkspaceService via ServeBrokeredAsSandbox, registered
// under the node's own fingerprint. Blocks until the sandbox
// registration lands in the matcher so the client's RequestAttach
// won't race it.
func startSyncNode(t *testing.T, orchURL string, nodeID, srvID *identity.Identity, srv *orchestrator.Server) {
	t.Helper()
	nodePeers, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "node-peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore node: %v", err)
	}
	if err := nodePeers.Add(identity.Peer{Fingerprint: srvID.Fingerprint()}); err != nil {
		t.Fatalf("node approve srv: %v", err)
	}
	store, err := cas.Open(filepath.Join(t.TempDir(), "node-cas"))
	if err != nil {
		t.Fatalf("cas.Open node: %v", err)
	}
	wsServer, err := workspace.NewServer(store)
	if err != nil {
		t.Fatalf("workspace.NewServer: %v", err)
	}
	mux := http.NewServeMux()
	path, handler := workspacev1connect.NewWorkspaceServiceHandler(wsServer)
	mux.Handle(path, handler)

	bc, err := brokerclient.New(brokerclient.Config{URL: orchURL, Identity: nodeID, Peers: nodePeers})
	if err != nil {
		t.Fatalf("brokerclient.New node: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = bc.ServeBrokeredAsSandbox(ctx, nodeID.Fingerprint().String(),
			brokerclient.ServeOptions{Handler: mux})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("node ServeBrokeredAsSandbox did not exit on cancel")
		}
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, sandboxes := srv.Matcher().Counts(); sandboxes >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("node sandbox registration never landed in the matcher")
}

// syncCmd builds a throwaway cobra command carrying ctx + an output
// buffer so runWorkspaceSync can be driven directly with controlled
// opts (bypassing flag parsing).
func syncCmd(ctx context.Context) (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var out bytes.Buffer
	cmd.SetOut(&out)
	return cmd, &out
}

// TestWorkspacePushPullRoundTrip drives the full CLI sync path: a
// local dir is pushed to the node's CAS over the broker relay, then
// pulled back into a fresh local dir (with a fresh client CAS, so the
// pull genuinely fetches chunks from the node). A read-only mount is
// skipped on pull.
func TestWorkspacePushPullRoundTrip(t *testing.T) {
	rig := newSyncE2ERig(t)
	const wsName = "sync-rt"

	commonArgs := []string{orchestratorArg, rig.orchURL, configDirArg, rig.clientDir, nameFlag, wsName}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, createSubName}, commonArgs...)...); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, targetSubName, rig.nodeFP}, commonArgs...)...); err != nil {
		t.Fatalf("target: %v", err)
	}

	// A project dir with a recorded rw mount (real files) + a ro mount.
	projectDir := t.TempDir()
	t.Chdir(projectDir)
	srcDir := filepath.Join(projectDir, "src")
	roDir := filepath.Join(projectDir, "ro")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(roDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const fileName, fileBody = "hello.txt", "round-trip payload\n"
	if err := os.WriteFile(filepath.Join(srcDir, fileName), []byte(fileBody), 0o600); err != nil {
		t.Fatalf("write src file: %v", err)
	}
	if _, err := addDirLocalMount(mountMapping{Local: "src", Remote: workRemote}); err != nil {
		t.Fatalf("addDirLocalMount rw: %v", err)
	}
	if _, err := addDirLocalMount(mountMapping{Local: "ro", Remote: "/ro", ReadOnly: true}); err != nil {
		t.Fatalf("addDirLocalMount ro: %v", err)
	}

	// Drive push + pull through the real cobra commands (exercising the
	// RunE wiring), not runWorkspaceSync directly.
	pushOut, _, err := runDaft(t, append([]string{workspaceCmdName, pushSubName}, commonArgs...)...)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if !strings.Contains(pushOut, "pushed") {
		t.Errorf("push output missing 'pushed':\n%s", pushOut)
	}

	// Delete the local file so pull must restore it, and point pull at
	// a fresh CAS so chunks come from the node, not the local store.
	if err := os.Remove(filepath.Join(srcDir, fileName)); err != nil {
		t.Fatalf("remove src file: %v", err)
	}
	pullCasDir := t.TempDir()
	pullArgs := append([]string{workspaceCmdName, pullCatalogSubName, "--cas-dir", pullCasDir}, commonArgs...)
	pullOut, _, err := runDaft(t, pullArgs...)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !strings.Contains(pullOut, "skip /ro (read-only)") {
		t.Errorf("pull output missing read-only skip:\n%s", pullOut)
	}

	got, err := os.ReadFile(filepath.Join(srcDir, fileName)) //nolint:gosec // G304: test-controlled path
	if err != nil {
		t.Fatalf("read pulled file: %v", err)
	}
	if string(got) != fileBody {
		t.Errorf("pulled body = %q, want %q", got, fileBody)
	}
}

// fakeCatalog implements just Get for resolveSyncTarget unit tests.
// A nil ws + non-nil err makes Get fail; otherwise it returns ws.
type fakeCatalog struct {
	workspacev1connect.UnimplementedWorkspaceCatalogServiceHandler
	ws        *workspacev1.Workspace
	err       error
	launchErr error
	// addMountErr, when set, is returned by AddMount; addMountTargets
	// records the in-VM target of each AddMount call for assertions.
	addMountErr     error
	addMountTargets []string
}

func (f *fakeCatalog) AddMount(_ context.Context, req *connect.Request[workspacev1.AddMountRequest]) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	f.addMountTargets = append(f.addMountTargets, req.Msg.GetMount().GetTarget())
	if f.addMountErr != nil {
		return nil, f.addMountErr
	}
	return connect.NewResponse(&workspacev1.WorkspaceResponse{Workspace: f.ws}), nil
}

func (f *fakeCatalog) Get(_ context.Context, _ *connect.Request[workspacev1.GetWorkspaceRequest]) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(&workspacev1.WorkspaceResponse{Workspace: f.ws}), nil
}

func (f *fakeCatalog) Launch(_ context.Context, _ *connect.Request[workspacev1.LaunchRequest]) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	if f.launchErr != nil {
		return nil, f.launchErr
	}
	return connect.NewResponse(&workspacev1.WorkspaceResponse{Workspace: f.ws}), nil
}

// fakeCatalogClient serves f over a plain HTTP/1.1 httptest server
// (catalog RPCs are unary, so no h2c needed) and returns a client.
func fakeCatalogClient(t *testing.T, f *fakeCatalog) workspacev1connect.WorkspaceCatalogServiceClient {
	t.Helper()
	mux := http.NewServeMux()
	path, h := workspacev1connect.NewWorkspaceCatalogServiceHandler(f)
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return workspacev1connect.NewWorkspaceCatalogServiceClient(srv.Client(), srv.URL)
}

// TestResolveSyncTargetGetError covers the Get-RPC error branch.
func TestResolveSyncTargetGetError(t *testing.T) {
	c := fakeCatalogClient(t, &fakeCatalog{err: connect.NewError(connect.CodeNotFound, errors.New("nope"))})
	if _, _, err := resolveSyncTarget(context.Background(), c, handleByName("x")); err == nil {
		t.Fatal("expected Get error")
	}
}

// TestResolveSyncTargetBadFingerprint covers the ParseFingerprint
// error branch: a target string that isn't a valid fingerprint.
func TestResolveSyncTargetBadFingerprint(t *testing.T) {
	ws := &workspacev1.Workspace{
		Metadata: &workspacev1.WorkspaceMetadata{Uuid: "u1"},
		Spec:     &workspacev1.WorkspaceSpec{TargetSandboxFingerprint: "not-a-fingerprint"},
	}
	c := fakeCatalogClient(t, &fakeCatalog{ws: ws})
	_, _, err := resolveSyncTarget(context.Background(), c, handleByName("x"))
	if err == nil || !strings.Contains(err.Error(), "parse target fingerprint") {
		t.Fatalf("err = %v, want parse-fingerprint error", err)
	}
}

// TestResolveSyncTargetOK covers the happy path returning uuid + fp.
func TestResolveSyncTargetOK(t *testing.T) {
	node, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	ws := &workspacev1.Workspace{
		Metadata: &workspacev1.WorkspaceMetadata{Uuid: "u-ok"},
		Spec:     &workspacev1.WorkspaceSpec{TargetSandboxFingerprint: node.Fingerprint().String()},
	}
	c := fakeCatalogClient(t, &fakeCatalog{ws: ws})
	uuid, fp, err := resolveSyncTarget(context.Background(), c, handleByName("x"))
	if err != nil {
		t.Fatalf("resolveSyncTarget: %v", err)
	}
	if uuid != "u-ok" || fp != node.Fingerprint() {
		t.Errorf("got (%q,%s), want (u-ok,%s)", uuid, fp, node.Fingerprint())
	}
}

// erroringWorkspaceService fails PutManifest + GetManifest so the
// push/pull RPC-error branches of syncMounts can be exercised.
type erroringWorkspaceService struct {
	workspacev1connect.UnimplementedWorkspaceServiceHandler
}

func (erroringWorkspaceService) PutManifest(context.Context, *connect.Request[workspacev1.PutManifestRequest]) (*connect.Response[workspacev1.PutManifestResponse], error) {
	return nil, connect.NewError(connect.CodeInternal, errors.New("boom"))
}

func (erroringWorkspaceService) GetManifest(context.Context, *connect.Request[workspacev1.GetManifestRequest]) (*connect.Response[workspacev1.GetManifestResponse], error) {
	return nil, connect.NewError(connect.CodeInternal, errors.New("boom"))
}

func erroringWorkspaceClient(t *testing.T) workspacev1connect.WorkspaceServiceClient {
	t.Helper()
	mux := http.NewServeMux()
	path, h := workspacev1connect.NewWorkspaceServiceHandler(erroringWorkspaceService{})
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return workspacev1connect.NewWorkspaceServiceClient(srv.Client(), srv.URL)
}

// TestSyncMountsPushAndPullRPCErrors covers the RPC-error returns of
// syncMounts/pushMount/pullMount: the server fails every call, so a
// real local mount still surfaces the transport error.
func TestSyncMountsPushAndPullRPCErrors(t *testing.T) {
	store, err := cas.Open(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("cas.Open: %v", err)
	}
	cc := &clientCtx{store: store}
	wsClient := erroringWorkspaceClient(t)
	mounts := []mountMapping{{Local: t.TempDir(), Remote: workRemote}}

	if err := syncMounts(context.Background(), &bytes.Buffer{}, wsClient, cc, "u", mounts, syncPush); err == nil {
		t.Error("expected push RPC error")
	}
	if err := syncMounts(context.Background(), &bytes.Buffer{}, wsClient, cc, "u", mounts, syncPull); err == nil {
		t.Error("expected pull RPC error")
	}
}

// TestWorkspaceSyncRejectsBothHandleFlags covers runWorkspaceSync's
// resolveHandle error branch (--name and --uuid are exclusive).
func TestWorkspaceSyncRejectsBothHandleFlags(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := addDirLocalMount(mountMapping{Local: t.TempDir(), Remote: workRemote}); err != nil {
		t.Fatalf("addDirLocalMount: %v", err)
	}
	cmd, _ := syncCmd(context.Background())
	err := runWorkspaceSync(cmd, workspaceClientOpts{name: "a", uuid: "b", timeout: time.Second}, "", syncPush)
	if err == nil {
		t.Fatal("expected error for both --name and --uuid")
	}
}

// TestWorkspaceSyncCatalogDialError covers runWorkspaceSync's
// dialWorkspaceCatalog error branch: a config dir that's a regular
// file can't yield a client identity. The orchestrator flag is set
// so resolution never reads the bogus config dir.
func TestWorkspaceSyncCatalogDialError(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, err := addDirLocalMount(mountMapping{Local: t.TempDir(), Remote: workRemote}); err != nil {
		t.Fatalf("addDirLocalMount: %v", err)
	}
	badDir := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(badDir, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	cmd, _ := syncCmd(context.Background())
	err := runWorkspaceSync(cmd, workspaceClientOpts{
		orchestrator: unreachableOrchestrator, configDir: badDir, timeout: 5 * time.Second, name: "x",
	}, "", syncPush)
	if err == nil {
		t.Fatal("expected dialWorkspaceCatalog error for bad config dir")
	}
}

// TestWorkspaceSyncOpenClientCtxError covers the openClientCtx error
// branch: a valid workspace + target, but a CAS dir that's a regular
// file fails to open after the target is resolved.
func TestWorkspaceSyncOpenClientCtxError(t *testing.T) {
	rig := newSyncE2ERig(t)
	const wsName = "sync-badcas"
	commonArgs := []string{orchestratorArg, rig.orchURL, configDirArg, rig.clientDir, nameFlag, wsName}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, createSubName}, commonArgs...)...); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, targetSubName, rig.nodeFP}, commonArgs...)...); err != nil {
		t.Fatalf("target: %v", err)
	}
	t.Chdir(t.TempDir())
	if _, err := addDirLocalMount(mountMapping{Local: t.TempDir(), Remote: workRemote}); err != nil {
		t.Fatalf("addDirLocalMount: %v", err)
	}
	badCas := filepath.Join(t.TempDir(), "casfile")
	if err := os.WriteFile(badCas, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, _ := syncCmd(ctx)
	err := runWorkspaceSync(cmd, workspaceClientOpts{
		orchestrator: rig.orchURL, configDir: rig.clientDir, timeout: 8 * time.Second, name: wsName,
	}, badCas, syncPush)
	if err == nil {
		t.Fatal("expected openClientCtx error for bad cas dir")
	}
}

// TestWorkspaceSyncMountsErrorThroughStack covers runWorkspaceSync's
// syncMounts error branch end to end: the relay opens fine, but a
// mount whose local dir doesn't exist fails at manifest build.
func TestWorkspaceSyncMountsErrorThroughStack(t *testing.T) {
	rig := newSyncE2ERig(t)
	const wsName = "sync-badmount"
	commonArgs := []string{orchestratorArg, rig.orchURL, configDirArg, rig.clientDir, nameFlag, wsName}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, createSubName}, commonArgs...)...); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, targetSubName, rig.nodeFP}, commonArgs...)...); err != nil {
		t.Fatalf("target: %v", err)
	}
	t.Chdir(t.TempDir())
	if _, err := addDirLocalMount(mountMapping{Local: filepath.Join(t.TempDir(), "missing"), Remote: workRemote}); err != nil {
		t.Fatalf("addDirLocalMount: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, _ := syncCmd(ctx)
	err := runWorkspaceSync(cmd, workspaceClientOpts{
		orchestrator: rig.orchURL, configDir: rig.clientDir, timeout: 8 * time.Second, name: wsName,
	}, "", syncPush)
	if err == nil {
		t.Fatal("expected syncMounts error for missing local dir")
	}
}

// TestWorkspaceSyncNoMounts: with no dir-local mounts recorded, sync
// prints a friendly message and does no work (no orchestrator needed).
func TestWorkspaceSyncNoMounts(t *testing.T) {
	t.Chdir(t.TempDir())
	cmd, out := syncCmd(context.Background())
	if err := runWorkspaceSync(cmd, workspaceClientOpts{name: "x", timeout: time.Second}, "", syncPush); err != nil {
		t.Fatalf("runWorkspaceSync: %v", err)
	}
	if !strings.Contains(out.String(), "no mounts recorded") {
		t.Errorf("output = %q, want no-mounts message", out.String())
	}
}

// TestPushMountBuildError covers pushMount's BuildManifest error
// branch: a local path that doesn't exist can't be manifested.
func TestPushMountBuildError(t *testing.T) {
	store, err := cas.Open(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("cas.Open: %v", err)
	}
	cc := &clientCtx{store: store}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	err = pushMount(context.Background(), &bytes.Buffer{}, nil, cc,
		workspace.SyncKey("ws", workRemote), missing, workRemote)
	if err == nil {
		t.Fatal("expected BuildManifest error for missing local dir")
	}
}

// TestPullMountMkdirError covers pullMount's MkdirAll error branch: a
// regular file where the local dir should be makes the mkdir fail.
func TestPullMountMkdirError(t *testing.T) {
	store, err := cas.Open(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("cas.Open: %v", err)
	}
	cc := &clientCtx{store: store}
	asFile := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(asFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	err = pullMount(context.Background(), &bytes.Buffer{}, nil, cc,
		workspace.SyncKey("ws", workRemote), filepath.Join(asFile, "sub"), workRemote)
	if err == nil {
		t.Fatal("expected MkdirAll error when local path is under a file")
	}
}

// TestWorkspaceSyncDialFails: the workspace targets a node that isn't
// serving, so DialBrokeredServe fails (no matching sandbox
// registration) and runWorkspaceSync surfaces it.
func TestWorkspaceSyncDialFails(t *testing.T) {
	rig := newSyncE2ERig(t)
	const wsName = "sync-dial-fail"
	commonArgs := []string{orchestratorArg, rig.orchURL, configDirArg, rig.clientDir, nameFlag, wsName}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, createSubName}, commonArgs...)...); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Target a valid-but-unserved fingerprint.
	bogus, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, targetSubName, bogus.Fingerprint().String()}, commonArgs...)...); err != nil {
		t.Fatalf("target: %v", err)
	}

	t.Chdir(t.TempDir())
	if _, err := addDirLocalMount(mountMapping{Local: t.TempDir(), Remote: workRemote}); err != nil {
		t.Fatalf("addDirLocalMount: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, _ := syncCmd(ctx)
	err = runWorkspaceSync(cmd, workspaceClientOpts{
		orchestrator: rig.orchURL, configDir: rig.clientDir, timeout: 8 * time.Second, name: wsName,
	}, "", syncPush)
	if err == nil {
		t.Fatal("expected dial failure when target node isn't serving")
	}
}

// TestWorkspaceSyncNoTarget: a workspace without a target node can't
// be synced — resolveSyncTarget surfaces an actionable error.
func TestWorkspaceSyncNoTarget(t *testing.T) {
	rig := newSyncE2ERig(t)
	const wsName = "sync-no-target"
	commonArgs := []string{orchestratorArg, rig.orchURL, configDirArg, rig.clientDir, nameFlag, wsName}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, createSubName}, commonArgs...)...); err != nil {
		t.Fatalf("create: %v", err)
	}

	t.Chdir(t.TempDir())
	if _, err := addDirLocalMount(mountMapping{Local: t.TempDir(), Remote: workRemote}); err != nil {
		t.Fatalf("addDirLocalMount: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, _ := syncCmd(ctx)
	err := runWorkspaceSync(cmd, workspaceClientOpts{
		orchestrator: rig.orchURL, configDir: rig.clientDir, timeout: 8 * time.Second, name: wsName,
	}, "", syncPush)
	if err == nil || !strings.Contains(err.Error(), "no target node") {
		t.Fatalf("err = %v, want 'no target node'", err)
	}
}
