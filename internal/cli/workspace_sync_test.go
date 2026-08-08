package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	"github.com/richiejp/darkaifactory/internal/casindex"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/orchestrator"
	"github.com/richiejp/darkaifactory/internal/trace"
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
	orchURL    string
	clientDir  string
	nodeFP     string
	nodeServer *workspace.Server
	nodeIndex  *casindex.Index
	catalog    *workspacecatalog.Store
}

type cliWorkspaceDiskController struct {
	status        workspace.WorkspaceDiskStatus
	unfreezeCalls int
	deleteCalls   int
}

func (c *cliWorkspaceDiskController) Status(string) (workspace.WorkspaceDiskStatus, error) {
	return c.status, nil
}

func (c *cliWorkspaceDiskController) Unfreeze(context.Context, string) error {
	c.unfreezeCalls++
	c.status.Frozen = false
	return nil
}

func (c *cliWorkspaceDiskController) Resize(context.Context, string, uint64) error { return nil }

func (c *cliWorkspaceDiskController) Delete(context.Context, string) error {
	c.deleteCalls++
	return nil
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

	srv, catalog := startSyncOrchestrator(t, srvID, srvStore)
	orchURL := "https://" + srv.Addr()

	// Client approves the orchestrator so its on-disk peer store
	// trusts the broker + catalog endpoints.
	if _, _, err := runDaft(t, authCmdName, configDirArg, clientDir, "approve",
		srvID.Fingerprint().String(), "--role", "orchestrator"); err != nil {
		t.Fatalf("client approve orchestrator: %v", err)
	}

	nodeServer, nodeIndex := startSyncNode(t, orchURL, nodeID, srvID, srv)

	return &syncE2ERig{orchURL: orchURL, clientDir: clientDir, nodeFP: nodeID.Fingerprint().String(), nodeServer: nodeServer, nodeIndex: nodeIndex, catalog: catalog}
}

func startSyncOrchestrator(t *testing.T, srvID *identity.Identity, srvStore *identity.PeerStore) (*orchestrator.Server, *workspacecatalog.Store) {
	t.Helper()
	catalog := workspacecatalog.NewStore(nil)
	srv, err := orchestrator.New(orchestrator.Config{
		Identity:         srvID,
		Peers:            srvStore,
		Addr:             loopbackAddrAny,
		WorkspaceCatalog: catalog,
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
	return srv, catalog
}

// startSyncNode runs the node side: a brokerclient hosting a
// CAS-backed WorkspaceService via ServeBrokeredAsSandbox, registered
// under the node's own fingerprint. Blocks until the sandbox
// registration lands in the matcher so the client's RequestAttach
// won't race it.
func startSyncNode(t *testing.T, orchURL string, nodeID, srvID *identity.Identity, srv *orchestrator.Server) (*workspace.Server, *casindex.Index) {
	t.Helper()
	nodePeers, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "node-peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore node: %v", err)
	}
	if err := nodePeers.Add(identity.Peer{Fingerprint: srvID.Fingerprint()}); err != nil {
		t.Fatalf("node approve srv: %v", err)
	}
	root := filepath.Join(t.TempDir(), "node-cas")
	store, err := cas.Open(root)
	if err != nil {
		t.Fatalf("cas.Open node: %v", err)
	}
	index, err := casindex.Open(root, nil)
	if err != nil {
		t.Fatalf("casindex.Open node: %v", err)
	}
	t.Cleanup(func() { _ = index.Close() })
	wsServer, err := workspace.NewIndexedServer(store, index)
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
			return wsServer, index
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("node sandbox registration never landed in the matcher")
	return nil, nil
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
//
//nolint:funlen // One brokered lifecycle keeps the exact pushed/adopted generations needed by reject assertions.
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

	diskController := &cliWorkspaceDiskController{status: workspace.WorkspaceDiskStatus{
		UsedBytes: 7, AvailableBytes: 13, AllowanceBytes: 20, Frozen: true,
	}}
	rig.nodeServer.SetWorkspaceDiskController(diskController)
	statusOut, _, err := runDaft(t, append([]string{workspaceCmdName, storageSubName, statusSubName}, commonArgs...)...)
	if err != nil || !strings.Contains(statusOut, "hard=") || !strings.Contains(statusOut, "workspace_protected=") {
		t.Fatalf("storage status = %q, %v", statusOut, err)
	}
	pruneOut, _, err := runDaft(t, append([]string{workspaceCmdName, storageSubName, pruneSubName, "--dry-run"}, commonArgs...)...)
	if err != nil || !strings.Contains(pruneOut, "usage_before=") {
		t.Fatalf("storage prune = %q, %v", pruneOut, err)
	}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, storageSubName, pruneSubName, "--dry-run", "--max-candidates", "201"}, commonArgs...)...); err == nil {
		t.Fatal("unbounded storage prune succeeded")
	}

	var writableKey string
	for _, key := range rig.nodeServer.Names() {
		if strings.HasSuffix(key, "\x00"+workRemote) {
			writableKey = key
			break
		}
	}
	if writableKey == "" {
		t.Fatal("writable sync key not published")
	}
	live := t.TempDir()
	if err := os.WriteFile(filepath.Join(live, "sandbox.txt"), []byte("unadopted"), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.nodeServer.SetLiveMount(writableKey, live)
	if _, err := rig.nodeServer.GetManifest(t.Context(), connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: writableKey})); err != nil {
		t.Fatal(err)
	}
	rig.nodeServer.ClearLiveMount(writableKey)
	rejectOut, _, err := runDaft(t, append([]string{workspaceCmdName, changesSubName, "reject"}, commonArgs...)...)
	if err != nil || diskController.deleteCalls != 1 || !strings.Contains(rejectOut, "restored generation") {
		t.Fatalf("changes reject = %q, %v, disk deletes %d", rejectOut, err, diskController.deleteCalls)
	}
	diskController.status.Running = true
	rig.nodeServer.SetLiveMount(writableKey, live)
	rig.nodeServer.SetLiveMountRefresher(writableKey, func(context.Context, string) error {
		return errors.New("paused guest must not be refreshed")
	})
	defer rig.nodeServer.ClearLiveMount(writableKey)
	unfreezeOut, _, err := runDaft(t, append([]string{workspaceCmdName, unfreezeSubName}, commonArgs...)...)
	if err != nil || diskController.unfreezeCalls != 1 || !strings.Contains(unfreezeOut, "unfrozen") {
		t.Fatalf("workspace unfreeze = %q, %v, calls %d", unfreezeOut, err, diskController.unfreezeCalls)
	}
	if err := rig.nodeIndex.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, storageSubName, statusSubName}, commonArgs...)...); err == nil {
		t.Fatal("storage status succeeded after node index close")
	}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, unfreezeSubName}, commonArgs...)...); err == nil {
		t.Fatal("unfreeze succeeded after durable manifest index close")
	}
}

//nolint:funlen // One brokered lifecycle covers node, targetless, stale, malformed, unreachable, and writer-failure deletion outcomes.
func TestWorkspaceDeleteRemovesNodeDataAndCatalog(t *testing.T) {
	rig := newSyncE2ERig(t)
	const wsName = "delete-e2e"
	commonArgs := []string{orchestratorArg, rig.orchURL, configDirArg, rig.clientDir, nameFlag, wsName}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, createSubName}, commonArgs...)...); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, targetSubName, rig.nodeFP}, commonArgs...)...); err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	t.Chdir(project)
	if err := os.MkdirAll(filepath.Join(project, ".daft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".daft", "config.json"), []byte(`{"mounts":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	controller := &cliWorkspaceDiskController{}
	rig.nodeServer.SetWorkspaceDiskController(controller)
	output, _, err := runDaft(t, append([]string{workspaceCmdName, deleteSubName}, commonArgs...)...)
	if err != nil || controller.deleteCalls != 1 || !strings.Contains(output, "deleted workspace") {
		t.Fatalf("workspace delete = %q, %v, calls %d", output, err, controller.deleteCalls)
	}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, statusSubName}, commonArgs...)...); err == nil {
		t.Fatal("deleted workspace remains in catalog")
	}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, deleteSubName}, commonArgs...)...); err == nil || !strings.Contains(err.Error(), "get workspace") {
		t.Fatalf("repeated workspace delete = %v", err)
	}

	const freshName = "delete-never-launched"
	freshArgs := []string{orchestratorArg, rig.orchURL, configDirArg, rig.clientDir, nameFlag, freshName}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, createSubName}, freshArgs...)...); err != nil {
		t.Fatal(err)
	}
	catalog, _, err := dialWorkspaceCatalog(rig.clientDir, rig.orchURL, trace.New("workspace-delete-test"))
	if err != nil {
		t.Fatal(err)
	}
	freshHandle := &workspacev1.WorkspaceHandle{Handle: &workspacev1.WorkspaceHandle_Name{Name: freshName}}
	freshResource, err := catalog.Get(t.Context(), connect.NewRequest(&workspacev1.GetWorkspaceRequest{Handle: freshHandle}))
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteWorkspaceCatalog(t.Context(), io.Discard, catalog, freshHandle,
		freshResource.Msg.GetWorkspace().GetMetadata().GetUuid(), freshResource.Msg.GetWorkspace().GetMetadata().GetGeneration()+1); err == nil {
		t.Fatal("stale catalog delete succeeded")
	}
	const malformedName = "delete-malformed-target"
	malformedArgs := []string{orchestratorArg, rig.orchURL, configDirArg, rig.clientDir, nameFlag, malformedName}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, createSubName}, malformedArgs...)...); err != nil {
		t.Fatal(err)
	}
	malformedHandle := &workspacev1.WorkspaceHandle{Handle: &workspacev1.WorkspaceHandle_Name{Name: malformedName}}
	malformedResource, err := catalog.Get(t.Context(), connect.NewRequest(&workspacev1.GetWorkspaceRequest{Handle: malformedHandle}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = rig.catalog.Update(malformedResource.Msg.GetWorkspace().GetMetadata().GetUuid(), workspacecatalog.MutateSpec, func(value *workspacecatalog.Workspace) error {
		value.Spec.TargetSandboxFingerprint = "not-a-fingerprint"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, deleteSubName}, malformedArgs...)...); err == nil || !strings.Contains(err.Error(), "parse target fingerprint") {
		t.Fatalf("malformed target delete = %v", err)
	}
	output, _, err = runDaft(t, append([]string{workspaceCmdName, deleteSubName}, freshArgs...)...)
	if err != nil || controller.deleteCalls != 1 || !strings.Contains(output, "deleted workspace") {
		t.Fatalf("never-launched workspace delete = %q, %v, node calls %d", output, err, controller.deleteCalls)
	}

	const unreachableName = "delete-unreachable-node"
	unreachableArgs := []string{orchestratorArg, rig.orchURL, configDirArg, rig.clientDir, nameFlag, unreachableName, "--timeout", "500ms"}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, createSubName}, unreachableArgs...)...); err != nil {
		t.Fatal(err)
	}
	unreachable, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, targetSubName, unreachable.Fingerprint().String()}, unreachableArgs...)...); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, deleteSubName}, unreachableArgs...)...); err == nil {
		t.Fatal("delete unexpectedly reached an unregistered node")
	}

	const writerName = "delete-writer-failure"
	writerArgs := []string{orchestratorArg, rig.orchURL, configDirArg, rig.clientDir, nameFlag, writerName}
	if _, _, err := runDaft(t, append([]string{workspaceCmdName, createSubName}, writerArgs...)...); err != nil {
		t.Fatal(err)
	}
	writerHandle := &workspacev1.WorkspaceHandle{Handle: &workspacev1.WorkspaceHandle_Name{Name: writerName}}
	writerResource, err := catalog.Get(t.Context(), connect.NewRequest(&workspacev1.GetWorkspaceRequest{Handle: writerHandle}))
	if err != nil {
		t.Fatal(err)
	}
	writerWorkspace := writerResource.Msg.GetWorkspace()
	if err := deleteWorkspaceCatalog(t.Context(), failingWriter{}, catalog, writerHandle,
		writerWorkspace.GetMetadata().GetUuid(), writerWorkspace.GetMetadata().GetGeneration()); !errors.Is(err, errFail) {
		t.Fatalf("catalog delete writer failure = %v", err)
	}
}

func TestRunWorkspaceDeleteRejectsAmbiguousHandleBeforeDial(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	err := runWorkspaceDelete(cmd, workspaceClientOpts{name: "name", uuid: "uuid"}, "", nil)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("ambiguous delete = %v", err)
	}
	err = runWorkspaceDelete(cmd, workspaceClientOpts{name: "name", orchestrator: "://invalid", configDir: t.TempDir()}, "", nil)
	if err == nil {
		t.Fatal("delete accepted malformed orchestrator URL")
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
		Metadata: &workspacev1.WorkspaceMetadata{Uuid: "u-ok", Generation: 7},
		Spec:     &workspacev1.WorkspaceSpec{TargetSandboxFingerprint: node.Fingerprint().String()},
		Status:   &workspacev1.WorkspaceStatus{ObservedGeneration: 6},
	}
	c := fakeCatalogClient(t, &fakeCatalog{ws: ws})
	uuid, fp, err := resolveSyncTarget(context.Background(), c, handleByName("x"))
	if err != nil {
		t.Fatalf("resolveSyncTarget: %v", err)
	}
	if uuid != "u-ok" || fp != node.Fingerprint() {
		t.Errorf("got (%q,%s), want (u-ok,%s)", uuid, fp, node.Fingerprint())
	}
	target, err := resolveSyncTargetDetails(context.Background(), c, handleByName("x"))
	if err != nil || target.generation != 7 || target.observedGeneration != 6 {
		t.Fatalf("details = %+v, %v; want generations 7/6", target, err)
	}
}

func TestRequireObservedSyncTargetRejectsStaleMaterialization(t *testing.T) {
	target := syncTarget{generation: 9, observedGeneration: 8, launchGeneration: 8, active: true}
	err := requireObservedSyncTarget(target)
	if err == nil || !strings.Contains(err.Error(), "desired generation 9, launched 8") {
		t.Fatalf("err = %v, want both generations", err)
	}
	target.launchGeneration = 9
	if err := requireObservedSyncTarget(target); err != nil {
		t.Fatalf("equal generations rejected: %v", err)
	}
	target.observedGeneration = 0
	if err := requireObservedSyncTarget(target); err != nil {
		t.Fatalf("first-heartbeat lag rejected: %v", err)
	}
	target.launchGeneration = 8
	target.active = false
	if err := requireObservedSyncTarget(target); err != nil {
		t.Fatalf("inactive workspace rejected: %v", err)
	}
}

// erroringWorkspaceService fails PutManifest + GetManifest so the
// push/pull RPC-error branches of syncMounts can be exercised.
type erroringWorkspaceService struct {
	workspacev1connect.UnimplementedWorkspaceServiceHandler
}

type controlWorkspaceService struct {
	workspacev1connect.UnimplementedWorkspaceServiceHandler
	rejectErr error
	present   bool
}

func (s controlWorkspaceService) GetManifest(context.Context, *connect.Request[workspacev1.GetManifestRequest]) (*connect.Response[workspacev1.GetManifestResponse], error) {
	return connect.NewResponse(&workspacev1.GetManifestResponse{ManifestDigest: strings.Repeat("a", 64), Generation: 2}), nil
}

func (s controlWorkspaceService) RejectManifest(context.Context, *connect.Request[workspacev1.RejectManifestRequest]) (*connect.Response[workspacev1.ManifestControlResponse], error) {
	if s.rejectErr != nil {
		return nil, s.rejectErr
	}
	return connect.NewResponse(&workspacev1.ManifestControlResponse{
		CurrentManifestPresent: s.present, ManifestDigest: strings.Repeat("b", 64), Generation: 1,
	}), nil
}

func workspaceClientForHandler(t *testing.T, handler workspacev1connect.WorkspaceServiceHandler) workspacev1connect.WorkspaceServiceClient {
	t.Helper()
	mux := http.NewServeMux()
	path, connectHandler := workspacev1connect.NewWorkspaceServiceHandler(handler)
	mux.Handle(path, connectHandler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return workspacev1connect.NewWorkspaceServiceClient(server.Client(), server.URL)
}

func (erroringWorkspaceService) PutManifest(context.Context, *connect.Request[workspacev1.PutManifestRequest]) (*connect.Response[workspacev1.PutManifestResponse], error) {
	return nil, connect.NewError(connect.CodeInternal, errors.New("boom"))
}

func (erroringWorkspaceService) GetManifest(context.Context, *connect.Request[workspacev1.GetManifestRequest]) (*connect.Response[workspacev1.GetManifestResponse], error) {
	return nil, connect.NewError(connect.CodeInternal, errors.New("boom"))
}

func erroringWorkspaceClient(t *testing.T) workspacev1connect.WorkspaceServiceClient {
	return workspaceClientForHandler(t, erroringWorkspaceService{})
}

func TestRejectWorkspaceChangesBranchesAndStorageFormatting(t *testing.T) {
	var output bytes.Buffer
	if err := rejectWorkspaceChanges(t.Context(), &output, nil, "uuid", []mountMapping{{ReadOnly: true}}); err != nil || !strings.Contains(output.String(), "no writable mounts") {
		t.Fatalf("read-only rejection = %q, %v", output.String(), err)
	}
	if err := rejectWorkspaceChanges(t.Context(), io.Discard, erroringWorkspaceClient(t), "uuid", []mountMapping{{Remote: workRemote}}); err == nil || !strings.Contains(err.Error(), "inspect") {
		t.Fatalf("inspection error = %v", err)
	}
	rejectErr := connect.NewError(connect.CodeUnavailable, errors.New("reject failed"))
	client := workspaceClientForHandler(t, controlWorkspaceService{rejectErr: rejectErr})
	if err := rejectWorkspaceChanges(t.Context(), io.Discard, client, "uuid", []mountMapping{{Remote: workRemote}}); err == nil || !strings.Contains(err.Error(), "reject failed") {
		t.Fatalf("rejection error = %v", err)
	}
	output.Reset()
	client = workspaceClientForHandler(t, controlWorkspaceService{})
	if err := rejectWorkspaceChanges(t.Context(), &output, client, "uuid", []mountMapping{{Remote: workRemote}}); err != nil || !strings.Contains(output.String(), "no prior client manifest") {
		t.Fatalf("no-prior rejection = %q, %v", output.String(), err)
	}
	output.Reset()
	if err := writeWorkspaceStorageStatus(&output, &workspacev1.GetWorkspaceDiskStatusResponse{UsedBytes: 3, ProtectedBytes: 2}, &workspacev1.GetStorageStatusResponse{UsageBytes: 7, HardBytes: 10}); err != nil || !strings.Contains(output.String(), "disk_used=3") || !strings.Contains(output.String(), "usage=7") {
		t.Fatalf("status output = %q, %v", output.String(), err)
	}
	output.Reset()
	if err := writeWorkspacePrunePlan(&output, &workspacev1.PruneStorageResponse{
		Candidates:  []*workspacev1.PruneCandidate{{Digest: "digest", Size: 3, Score: 0.5, Disposition: "rejected", LastUseUnix: 4}},
		UsageBefore: 10, UsageAfter: 7, Truncated: true,
	}); err != nil || !strings.Contains(output.String(), "digest size=3") || !strings.Contains(output.String(), "truncated=true") {
		t.Fatalf("prune output = %q, %v", output.String(), err)
	}
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
