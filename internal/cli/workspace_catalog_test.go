package cli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/orchestrator"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

// catalogFakeDispatcher mirrors the in-orchestrator fake (which
// can't be imported across package boundaries).
type catalogFakeDispatcher struct {
	mu     sync.Mutex
	calls  []catalogDispatchCall
	onCall func(catalogDispatchCall)
}

type catalogDispatchCall struct {
	fp        identity.Fingerprint
	eventType sandboxv1.EventType
	sandbox   *sandboxv1.Sandbox
}

func (f *catalogFakeDispatcher) PublishSandboxEvent(fp identity.Fingerprint, eventType sandboxv1.EventType, sandbox *sandboxv1.Sandbox) error {
	f.mu.Lock()
	call := catalogDispatchCall{fp: fp, eventType: eventType, sandbox: sandbox}
	f.calls = append(f.calls, call)
	onCall := f.onCall
	f.mu.Unlock()
	if onCall != nil {
		onCall(call)
	}
	return nil
}

func (f *catalogFakeDispatcher) last() (catalogDispatchCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return catalogDispatchCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// runOrchestratorWithCatalog boots an in-process orchestrator
// configured with the supplied workspace catalog + dispatcher, and
// returns the listen address and orchestrator server identity.
func runOrchestratorWithCatalog(
	t *testing.T,
	srvStore *identity.PeerStore,
	catalog *workspacecatalog.Store,
	dispatcher orchestrator.Dispatcher,
) (addr string, srvID *identity.Identity) {
	t.Helper()
	srvID, err := identity.Generate(identity.RoleOrchestrator)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	srv, err := orchestrator.New(orchestrator.Config{
		Identity:            srvID,
		Peers:               srvStore,
		Addr:                loopbackAddrAny,
		WorkspaceCatalog:    catalog,
		WorkspaceDispatcher: dispatcher,
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

// TestWorkspaceCatalogSubcommandsRegistered confirms every new
// verb is reachable from the cobra help tree. Caught a real
// wiring bug when AddCommand was forgotten during refactor.
func TestWorkspaceCatalogSubcommandsRegistered(t *testing.T) {
	stdout, _, err := runCmd(t, "workspace", "--help")
	if err != nil {
		t.Fatalf("daft workspace --help: %v", err)
	}
	wantVerbs := []string{
		createSubName, statusSubName, listSubName, imageSubName, targetSubName,
		mountSubName, envSubName, portSubName, validateSubName, launchSubName, stopSubName,
		acquireSubName, releaseSubName, changesSubName, storageSubName, unfreezeSubName, deleteSubName,
	}
	for _, v := range wantVerbs {
		if !strings.Contains(stdout, v) {
			t.Errorf("workspace --help missing %q\nfull help:\n%s", v, stdout)
		}
	}
}

func TestWorkspaceStoragePruneRequiresDryRun(t *testing.T) {
	_, _, err := runCmd(t, "workspace", storageSubName, pruneSubName)
	if err == nil || !strings.Contains(err.Error(), "--dry-run is required") {
		t.Fatalf("workspace storage prune error = %v", err)
	}
}

func TestWorkspaceControlCommandsRejectLocalConfigurationErrors(t *testing.T) {
	if _, _, err := runCmd(t, "workspace", storageSubName, statusSubName, nameFlag, "x", "--uuid", "y"); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("conflicting control handle = %v", err)
	}
	project := t.TempDir()
	t.Chdir(project)
	if err := os.MkdirAll(filepath.Join(project, ".daft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".daft", "config.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCmd(t, "workspace", changesSubName, "reject"); err == nil {
		t.Fatal("changes reject accepted corrupt project configuration")
	}
}

func TestResolveImageArg(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		clear bool
		want  string
		fail  bool
	}{
		{name: "set", args: []string{"registry.example/image:tag"}, want: "registry.example/image:tag"},
		{name: "clear", clear: true},
		{name: "missing", fail: true},
		{name: "clear with ref", args: []string{imageSubName}, clear: true, fail: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveImageArg(tc.args, tc.clear)
			if (err != nil) != tc.fail {
				t.Fatalf("resolveImageArg() error = %v, want failure %v", err, tc.fail)
			}
			if got != tc.want {
				t.Errorf("resolveImageArg() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWorkspaceImageRejectsMissingOrConflictingValue(t *testing.T) {
	for _, args := range [][]string{
		{workspaceCmdName, imageSubName},
		{workspaceCmdName, imageSubName, "--clear", imageSubName},
	} {
		if _, _, err := runCmd(t, args...); err == nil {
			t.Fatalf("daft %v succeeded, want argument error", args)
		}
	}
}

// TestWorkspaceRPCErrorsBubbleUp drives several catalog commands
// against a workspace name that doesn't exist. The orchestrator
// returns NotFound and the CLI must surface a non-nil error from
// each command — exercising the post-dial err-return path in
// every thin wrapper.
func TestWorkspaceRPCErrorsBubbleUp(t *testing.T) {
	h := newWorkspaceE2EHarness(t)
	missing := []string{orchestratorArg, h.orchURL, configDirArg, h.clientDir, nameFlag, "no-such-workspace"}
	cases := [][]string{
		{statusSubName},
		{imageSubName, testCatalogImage},
		{targetSubName, "sha256:" + strings.Repeat("AB:", 31) + "AB"},
		{mountSubName, addSubName, "/x"},
		{mountSubName, rmSubName, "/x"},
		{envSubName, addSubName, "K=v"},
		{envSubName, rmSubName, "K"},
		{portSubName, addSubName, "80"},
		{portSubName, rmSubName, "80"},
		{validateSubName},
		{launchSubName},
		{stopSubName},
	}
	for _, args := range cases {
		t.Run(args[0]+"-"+strings.Join(args, "-"), func(t *testing.T) {
			full := append([]string{workspaceCmdName}, args...)
			full = append(full, missing...)
			_, _, err := runDaft(t, full...)
			if err == nil {
				t.Errorf("daft workspace %v: want error, got nil", args)
			}
		})
	}
}

// TestWorkspaceArgValidationCoversPreDialPaths pins the RunE-side
// arg parsing that happens before runCatalogCmd dials. The
// orchestrator flag is deliberately omitted; each command should
// fail at the inline validator without ever needing TLS.
func TestWorkspaceArgValidationCoversPreDialPaths(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			"env add without equals",
			[]string{workspaceCmdName, "env", addSubName, "bogus", nameFlag, nameFlagVal},
			"NAME=VALUE",
		},
		{
			"env add empty name",
			[]string{workspaceCmdName, "env", addSubName, "=value", nameFlag, nameFlagVal},
			"empty NAME",
		},
		{
			"port add bad port",
			[]string{workspaceCmdName, "port", addSubName, "notanumber", nameFlag, nameFlagVal},
			"invalid port",
		},
		{
			"port rm bad proto",
			[]string{workspaceCmdName, "port", rmSubName, "80/sctp", nameFlag, nameFlagVal},
			"unknown protocol",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := runDaft(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestParsePortSpecCoversTCPAndUDPDefaultsAndErrors(t *testing.T) {
	cases := []struct {
		in        string
		wantPort  uint32
		wantProto sandboxv1.Protocol
		wantErr   bool
	}{
		{"3000", 3000, sandboxv1.Protocol_PROTOCOL_TCP, false},
		{"443/tcp", 443, sandboxv1.Protocol_PROTOCOL_TCP, false},
		{"53/udp", 53, sandboxv1.Protocol_PROTOCOL_UDP, false},
		{"80/TCP", 80, sandboxv1.Protocol_PROTOCOL_TCP, false},
		{"80/sctp", 0, 0, true},
		{"notanumber", 0, 0, true},
		{"100000", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			port, proto, err := parsePortSpec(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePortSpec(%q): %v", tc.in, err)
			}
			if port != tc.wantPort {
				t.Errorf("port = %d, want %d", port, tc.wantPort)
			}
			if proto != tc.wantProto {
				t.Errorf("proto = %s, want %s", proto, tc.wantProto)
			}
		})
	}
}

// e2eTestCommon is the workspace name + flag set every step of
// TestWorkspaceEndToEndCPUFlow shares. Extracted so the test body
// stays under funlen and the per-step helpers don't repeat it.
const testE2EWorkspaceName = "e2e"

const testE2EMountTarget = "/workspace"

// testMountRemote is the in-VM target used by the mount-mapping tests.
const (
	testMountRemote = "/workspace/src"
	testDiskFlag    = "--disk"
)

func TestWorkspaceClientDefaultTimeoutExceedsServerAcknowledgement(t *testing.T) {
	var opts workspaceClientOpts
	cmd := &cobra.Command{}
	bindWorkspaceClientFlags(cmd, &opts)
	if opts.timeout != defaultWorkspaceRPCTimeout || opts.timeout <= 30*time.Second {
		t.Fatalf("workspace timeout = %s, want %s and greater than server bound", opts.timeout, defaultWorkspaceRPCTimeout)
	}
}

// workspaceE2EHarness threads the in-process orchestrator + caller
// identity through the multi-step E2E test below.
type workspaceE2EHarness struct {
	t          *testing.T
	clientDir  string
	orchURL    string
	commonArgs []string
	fake       *catalogFakeDispatcher
	store      *workspacecatalog.Store
	sandboxFP  string
}

func newWorkspaceE2EHarness(t *testing.T) *workspaceE2EHarness {
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
		t.Fatalf("approve client: %v", err)
	}
	fake := &catalogFakeDispatcher{}
	catalogStore := workspacecatalog.NewStore(nil)
	fake.onCall = func(call catalogDispatchCall) {
		if call.eventType == sandboxv1.EventType_EVENT_TYPE_ADDED {
			changes, stop := catalogStore.Watch()
			go acknowledgeCatalogSandboxRunning(catalogStore, call.sandbox, changes, stop)
			return
		}
		if call.eventType != sandboxv1.EventType_EVENT_TYPE_DELETED {
			return
		}
		uid := call.sandbox.GetMetadata().GetUid()
		matches := catalogStore.List(func(ws *workspacecatalog.Workspace) bool {
			return ws.Status.CurrentSandbox != nil && ws.Status.CurrentSandbox.UID == uid
		})
		for index := range matches {
			ws := &matches[index]
			_, _ = catalogStore.Update(ws.Metadata.UUID, workspacecatalog.MutateStatus, func(current *workspacecatalog.Workspace) error {
				current.Status.CurrentSandbox = nil
				current.Status.SetCondition(workspacecatalog.ConditionDispatched, workspacecatalog.ConditionStatusFalse, "Killed", "", time.Now())
				return nil
			})
		}
	}
	addr, srvID := runOrchestratorWithCatalog(t, srvStore, catalogStore, fake)
	if _, _, err := runDaft(t, authCmdName, configDirArg, clientDir, "approve",
		srvID.Fingerprint().String(), "--role", "orchestrator"); err != nil {
		t.Fatalf("approve orchestrator: %v", err)
	}
	orchURL := "https://" + addr
	return &workspaceE2EHarness{
		t:         t,
		clientDir: clientDir,
		orchURL:   orchURL,
		fake:      fake,
		store:     catalogStore,
		sandboxFP: mustGenerate(t).Fingerprint().String(),
		commonArgs: []string{
			orchestratorArg, orchURL,
			configDirArg, clientDir,
			"--name", testE2EWorkspaceName,
		},
	}
}

func acknowledgeCatalogSandboxRunning(
	store *workspacecatalog.Store,
	sandbox *sandboxv1.Sandbox,
	changes <-chan uint64,
	stop func(),
) {
	defer stop()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	//daft:lint:disable=forloopbound reason="test Node acknowledgment is bounded by a one-second timer and the exact Sandbox binding"
	for {
		ws, err := store.Get(sandbox.GetSpec().GetWorkspaceUuid())
		if err == nil && ws.Status.CurrentSandbox != nil && ws.Status.CurrentSandbox.UID == sandbox.GetMetadata().GetUid() {
			_, _ = store.Update(ws.Metadata.UUID, workspacecatalog.MutateStatus, func(current *workspacecatalog.Workspace) error {
				current.Status.ObservedGeneration = current.Status.CurrentSandbox.WorkspaceGeneration
				current.Status.SetCondition(workspacecatalog.ConditionReconciled, workspacecatalog.ConditionStatusTrue, "RUNNING", "", time.Now())
				return nil
			})
			return
		}
		select {
		case <-changes:
		case <-deadline.C:
			return
		}
	}
}

func (h *workspaceE2EHarness) run(args ...string) string {
	h.t.Helper()
	stdout, stderr, err := runDaft(h.t, append([]string{"workspace"}, args...)...)
	if err != nil {
		h.t.Fatalf("daft workspace %v: %v\nstderr: %s", args, err, stderr)
	}
	return stdout
}

// runErr mirrors run but returns the error instead of t.Fatal'ing
// so a negative-path test can assert on the wire error.
func (h *workspaceE2EHarness) runErr(args ...string) (string, error) {
	h.t.Helper()
	stdout, _, err := runDaft(h.t, append([]string{"workspace"}, args...)...)
	return stdout, err
}

func (h *workspaceE2EHarness) assemble() {
	h.run(append([]string{createSubName}, h.commonArgs...)...)
	h.run(append([]string{imageSubName, testCatalogImage}, h.commonArgs...)...)
	h.run(append([]string{targetSubName, h.sandboxFP}, h.commonArgs...)...)
	h.run(append([]string{mountSubName, addSubName, testE2EMountTarget}, h.commonArgs...)...)
	h.run(append([]string{envSubName, addSubName, "NODE_ENV=development"}, h.commonArgs...)...)
	h.run(append([]string{portSubName, addSubName, "3000"}, h.commonArgs...)...)
}

func (h *workspaceE2EHarness) assertLaunchAndDispatch() {
	h.t.Helper()
	validateOut := h.run(append([]string{validateSubName}, h.commonArgs...)...)
	if !strings.Contains(validateOut, "Ready") {
		h.t.Errorf("validate did not flip Ready=True:\n%s", validateOut)
	}
	launchOut := h.run(append([]string{launchSubName}, h.commonArgs...)...)
	if !strings.Contains(launchOut, "Dispatched") {
		h.t.Errorf("launch did not flip Dispatched=True:\n%s", launchOut)
	}
	call, ok := h.fake.last()
	if !ok {
		h.t.Fatal("fake dispatcher saw no calls")
	}
	if call.sandbox.GetSpec().GetImageRef() != testCatalogImage {
		h.t.Errorf("spec.image_ref = %q, want %q", call.sandbox.GetSpec().GetImageRef(), testCatalogImage)
	}
	if len(call.sandbox.GetSpec().GetMounts()) != 1 || call.sandbox.GetSpec().GetMounts()[0].GetTarget() != testE2EMountTarget {
		h.t.Errorf("spec.mounts = %v, want [%s]", call.sandbox.GetSpec().GetMounts(), testE2EMountTarget)
	}
	if len(call.sandbox.GetSpec().GetEnv()) != 1 || call.sandbox.GetSpec().GetEnv()[0].GetName() != "NODE_ENV" {
		h.t.Errorf("spec.env = %v, want [NODE_ENV]", call.sandbox.GetSpec().GetEnv())
	}
	if len(call.sandbox.GetSpec().GetPorts()) != 1 || call.sandbox.GetSpec().GetPorts()[0].GetContainerPort() != 3000 {
		h.t.Errorf("spec.ports = %v, want [3000]", call.sandbox.GetSpec().GetPorts())
	}
}

func (h *workspaceE2EHarness) assertStopAndRemovals() {
	h.t.Helper()
	stopOut := h.run(append([]string{stopSubName}, h.commonArgs...)...)
	if !strings.Contains(stopOut, "Ready") || strings.Contains(stopOut, "Dispatched") {
		h.t.Errorf("stop did not clear Dispatched (expect Ready, no Dispatched):\n%s", stopOut)
	}
	h.run(append([]string{portSubName, rmSubName, "3000"}, h.commonArgs...)...)
	h.run(append([]string{envSubName, rmSubName, "NODE_ENV"}, h.commonArgs...)...)
	h.run(append([]string{mountSubName, rmSubName, testE2EMountTarget}, h.commonArgs...)...)
	finalOut := h.run(append([]string{statusSubName}, h.commonArgs...)...)
	if !strings.Contains(finalOut, "Missing:") {
		h.t.Errorf("removals should have re-introduced missing fields:\n%s", finalOut)
	}
	listOut := h.run(listSubName, orchestratorArg, h.orchURL, configDirArg, h.clientDir, "--mine")
	if !strings.Contains(listOut, testE2EWorkspaceName) {
		h.t.Errorf("list missing %q:\n%s", testE2EWorkspaceName, listOut)
	}
}

// TestWorkspaceEndToEndCPUFlow drives the workflow PLAN.md
// describes: create → image → target → mount add → env add → port
// add → validate → launch → stop. Asserts that the orchestrator's
// catalog reaches the expected end state and that the fake
// dispatcher saw a SandboxSpec carrying every field the CLI
// declared.
func TestWorkspaceEndToEndCPUFlow(t *testing.T) {
	h := newWorkspaceE2EHarness(t)
	h.assemble()
	h.assertLaunchAndDispatch()
	h.assertStopAndRemovals()
}

func mustGenerate(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return id
}

// nameFlag and nameFlagVal are the workspace-name flag plus a
// stub value used by the arg-validation tests; extracted so
// goconst stops flagging them.
const (
	nameFlag        = "--name"
	nameFlagVal     = "x"
	orchestratorArg = "--orchestrator"
	enableFlag      = "--enable"
	disableFlag     = "--disable"
)

const (
	// testUUIDStub is the placeholder UUID used by the resolveHandle
	// tests — the catalog never sees it because these tests don't
	// dial the orchestrator.
	testUUIDStub = "abc"
	// testCatalogImage is the image ref every catalog-flow test
	// asserts against. goconst stops flagging it once it's a const.
	testCatalogImage = "alpine:3"
	// testRenderUUID is the placeholder UUID used by the
	// writeWorkspaceList / writeWorkspaceSummary renderers.
	testRenderUUID = "u-1"
)

func TestResolveHandleRejectsBothFlags(t *testing.T) {
	_, err := workspaceClientOpts{uuid: testUUIDStub, name: "def"}.resolveHandle()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("err = %v, want mutually-exclusive message", err)
	}
}

func TestResolveHandleByUUID(t *testing.T) {
	h, err := workspaceClientOpts{uuid: testUUIDStub}.resolveHandle()
	if err != nil {
		t.Fatalf("resolveHandle: %v", err)
	}
	if h.GetUuid() != testUUIDStub {
		t.Errorf("handle uuid = %q, want %q", h.GetUuid(), testUUIDStub)
	}
}

func TestResolveHandleByExplicitName(t *testing.T) {
	h, err := workspaceClientOpts{name: "myapp"}.resolveHandle()
	if err != nil {
		t.Fatalf("resolveHandle: %v", err)
	}
	if h.GetName() != "myapp" {
		t.Errorf("handle name = %q, want myapp", h.GetName())
	}
}

// TestResolveHandleFallsBackToCwd pins the basename($PWD) default
// by cd'ing into a known temp dir and asserting the derived name.
func TestResolveHandleFallsBackToCwd(t *testing.T) {
	tmp := t.TempDir()
	wsName := "fallback-ws"
	target := tmp + "/" + wsName
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(target); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	h, err := workspaceClientOpts{}.resolveHandle()
	if err != nil {
		t.Fatalf("resolveHandle: %v", err)
	}
	if h.GetName() != wsName {
		t.Errorf("name = %q, want %q", h.GetName(), wsName)
	}
}

func TestWorkspaceNameForCreateRejectsUUID(t *testing.T) {
	_, err := workspaceNameForCreate(workspaceClientOpts{uuid: "x"})
	if err == nil || !strings.Contains(err.Error(), "--uuid is not valid for create") {
		t.Errorf("err = %v, want --uuid rejected", err)
	}
}

func TestWorkspaceNameForCreateExplicitWins(t *testing.T) {
	name, err := workspaceNameForCreate(workspaceClientOpts{name: "explicit"})
	if err != nil {
		t.Fatalf("workspaceNameForCreate: %v", err)
	}
	if name != "explicit" {
		t.Errorf("name = %q, want explicit", name)
	}
}

// TestWorkspaceNameForCreateFallsBackToCwd pins the no-name path:
// cd into a known temp directory and confirm basename($PWD) is
// what Create would send.
func TestWorkspaceNameForCreateFallsBackToCwd(t *testing.T) {
	tmp := t.TempDir()
	target := tmp + "/derived-ws"
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(target); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	name, err := workspaceNameForCreate(workspaceClientOpts{})
	if err != nil {
		t.Fatalf("workspaceNameForCreate: %v", err)
	}
	if name != "derived-ws" {
		t.Errorf("name = %q, want derived-ws", name)
	}
}

// TestWriteWorkspaceListWithEntries exercises the row-rendering
// path that the empty-list test doesn't reach.
func TestWriteWorkspaceListWithEntries(t *testing.T) {
	var buf strings.Builder
	if err := writeWorkspaceList(&buf, []*workspacev1.Workspace{
		{
			Metadata: &workspacev1.WorkspaceMetadata{Uuid: testRenderUUID, Name: "ws-a"},
			Spec:     &workspacev1.WorkspaceSpec{ImageRef: testCatalogImage},
		},
		{
			Metadata: &workspacev1.WorkspaceMetadata{Uuid: "u-2", Name: "ws-b"},
			Spec:     &workspacev1.WorkspaceSpec{},
		},
	}); err != nil {
		t.Fatalf("writeWorkspaceList: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"ws-a", testRenderUUID, testCatalogImage, "ws-b", "u-2"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
}

func TestWriteWorkspaceSummaryWithFullState(t *testing.T) {
	var buf strings.Builder
	err := writeWorkspaceSummary(&buf, &workspacev1.Workspace{
		Metadata: &workspacev1.WorkspaceMetadata{Uuid: testRenderUUID, Name: "ws"},
		Spec: &workspacev1.WorkspaceSpec{
			ImageRef:             testCatalogImage,
			Mounts:               []*sandboxv1.Mount{{Target: "/x"}, {Target: "/y", ReadOnly: true}},
			Env:                  []*sandboxv1.EnvVar{{Name: "K"}},
			Ports:                []*sandboxv1.PortMapping{{ContainerPort: 80, Protocol: sandboxv1.Protocol_PROTOCOL_TCP}},
			NestedVirtualization: &sandboxv1.NestedVirtualization{Enable: true},
			PrivilegedFuseMounts: &sandboxv1.PrivilegedFuseMounts{Enable: true},
			PrivilegedBuild:      &sandboxv1.PrivilegedBuild{Enable: true},
		},
		Status: &workspacev1.WorkspaceStatus{
			Missing: []string{"target"},
			LastTerminalSandbox: &workspacev1.TerminalSandbox{
				Uid: "failed-sandbox", Phase: "Killed", ExitCode: 17, Message: "disk preparation failed",
			},
		},
	})
	if err != nil {
		t.Fatalf("writeWorkspaceSummary: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Missing:", "target", "Last-Sandbox:", "failed-sandbox phase=Killed exit_code=17: disk preparation failed", "/x", "/y ro", "K", "80/tcp", "Nested-virt", "Privileged-FUSE", "Privileged-Build"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

func TestWriteWorkspaceListEmpty(t *testing.T) {
	var buf strings.Builder
	if err := writeWorkspaceList(&buf, nil); err != nil {
		t.Fatalf("writeWorkspaceList: %v", err)
	}
	if !strings.Contains(buf.String(), "NAME") {
		t.Errorf("empty list should still emit header, got:\n%s", buf.String())
	}
}

func TestWriteWorkspaceListPropagatesOutputFailure(t *testing.T) {
	if err := writeWorkspaceList(failingWriter{}, []*workspacev1.Workspace{{
		Metadata: &workspacev1.WorkspaceMetadata{Name: "broken"},
		Spec:     &workspacev1.WorkspaceSpec{},
	}}); err == nil {
		t.Fatal("writeWorkspaceList accepted a broken output writer")
	}
}

func TestWriteWorkspaceSummaryPropagatesOutputFailure(t *testing.T) {
	if err := writeWorkspaceSummary(failingWriter{}, &workspacev1.Workspace{
		Metadata: &workspacev1.WorkspaceMetadata{Name: "broken"},
		Spec:     &workspacev1.WorkspaceSpec{},
	}); err == nil {
		t.Fatal("writeWorkspaceSummary accepted a broken output writer")
	}
}

// TestWorkspaceAcquireAndReleaseRoundTrip drives the CLI's lock
// surface end-to-end: create → acquire → release. Mirrors the
// other RPC e2e tests; runs against a fresh in-process
// orchestrator with a fake dispatcher.
func TestWorkspaceAcquireAndReleaseRoundTrip(t *testing.T) {
	h := newWorkspaceE2EHarness(t)
	h.run(append([]string{createSubName}, h.commonArgs...)...)
	acquireOut := h.run(append([]string{acquireSubName, "--ttl", "30s"}, h.commonArgs...)...)
	// The summary block shows "Lock:" implicitly via the owner
	// being set; assert on the caller's fingerprint appearing
	// alongside the workspace name.
	if !strings.Contains(acquireOut, "Owner:") {
		t.Errorf("acquire output missing Owner row:\n%s", acquireOut)
	}
	if _, err := h.runErr(append([]string{releaseSubName}, h.commonArgs...)...); err != nil {
		t.Errorf("release: %v", err)
	}
}

// TestWorkspaceResourcesSetsMemoryAndCPUs drives the resources
// subcommand end-to-end: parse the human size, call SetResources,
// render the summary.
func TestWorkspaceResourcesSetsMemoryCPUsAndDisk(t *testing.T) {
	h := newWorkspaceE2EHarness(t)
	h.run(append([]string{createSubName}, h.commonArgs...)...)
	h.run(append([]string{resourcesSubName, "--memory", "2G", "--cpus", "2", testDiskFlag, "24G"}, h.commonArgs...)...)
}

func TestWorkspaceResourcesHelpNamesDisk(t *testing.T) {
	stdout, _, err := runCmd(t, workspaceCmdName, resourcesSubName, "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "RAM, retained disk allowance, and/or vCPUs") {
		t.Fatalf("resources help omitted disk allowance: %q", stdout)
	}
}

func TestWorkspaceResourcesRejectsBadDisk(t *testing.T) {
	h := newWorkspaceE2EHarness(t)
	h.run(append([]string{createSubName}, h.commonArgs...)...)
	_, err := h.runErr(append([]string{resourcesSubName, testDiskFlag, "lots"}, h.commonArgs...)...)
	if err == nil {
		t.Error("expected error for --disk lots")
	}
}

func TestWorkspaceResourcesGrowsExistingDiskBeforeCatalogUpdate(t *testing.T) {
	h := newWorkspaceE2EHarness(t)
	h.run(append([]string{createSubName}, h.commonArgs...)...)
	workspace := h.store.List(nil)[0]
	_, err := h.store.Update(workspace.Metadata.UUID, workspacecatalog.MutateStatus, func(item *workspacecatalog.Workspace) error {
		// Launch binding can become visible before the first disk-usage
		// heartbeat. The CLI must still treat the default allowance as a live
		// disk and route growth through the Node rather than merely updating
		// the catalog declaration.
		item.Status.CurrentSandbox = &workspacecatalog.SandboxRef{UID: "launch-heartbeat-race"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	original := resizeCatalogWorkspaceDisk
	t.Cleanup(func() { resizeCatalogWorkspaceDisk = original })
	var resized uint64
	resizeCatalogWorkspaceDisk = func(context.Context, workspaceClientOpts, string, uint64) error {
		resized = 24 << 30
		return nil
	}
	h.run(append([]string{resourcesSubName, testDiskFlag, "24G"}, h.commonArgs...)...)
	if resized != 24<<30 {
		t.Fatalf("resized = %d", resized)
	}
	resizeFailure := errors.New("resize failed")
	resizeCatalogWorkspaceDisk = func(context.Context, workspaceClientOpts, string, uint64) error { return resizeFailure }
	if _, err := h.runErr(append([]string{resourcesSubName, testDiskFlag, "25G"}, h.commonArgs...)...); !errors.Is(err, resizeFailure) {
		t.Fatalf("resize failure = %v", err)
	}
	if _, err := h.runErr(append([]string{resourcesSubName, testDiskFlag, "10G"}, h.commonArgs...)...); err == nil {
		t.Fatal("disk shrink succeeded")
	}
}

func TestResizeCatalogWorkspaceDiskRejectsMissingClientConfiguration(t *testing.T) {
	if err := resizeCatalogWorkspaceDisk(t.Context(), workspaceClientOpts{}, "workspace", 24<<30); err == nil {
		t.Fatal("resize opened without orchestrator configuration")
	}
}

// TestWorkspaceResourcesRejectsBadMemory pins the client-side
// parse guard: a non-numeric --memory never reaches the wire.
func TestWorkspaceResourcesRejectsBadMemory(t *testing.T) {
	h := newWorkspaceE2EHarness(t)
	h.run(append([]string{createSubName}, h.commonArgs...)...)
	_, err := h.runErr(append([]string{resourcesSubName, "--memory", "lots"}, h.commonArgs...)...)
	if err == nil {
		t.Error("expected error for --memory lots")
	}
}

// TestWorkspaceNestedEnable drives the nested subcommand end-to-end:
// --enable calls SetNestedVirtualization and the rendered summary
// surfaces the grant.
func TestWorkspaceNestedEnable(t *testing.T) {
	h := newWorkspaceE2EHarness(t)
	h.run(append([]string{createSubName}, h.commonArgs...)...)
	out := h.run(append([]string{nestedSubName, enableFlag}, h.commonArgs...)...)
	if !strings.Contains(out, "Nested-virt:") {
		t.Errorf("nested --enable summary missing Nested-virt row:\n%s", out)
	}
}

// TestWorkspaceNestedRequiresExactlyOneFlag pins the client-side
// guard: neither flag (or both) is a usage error that never reaches
// the wire, so the grant is never toggled by accident.
func TestWorkspaceNestedRequiresExactlyOneFlag(t *testing.T) {
	h := newWorkspaceE2EHarness(t)
	h.run(append([]string{createSubName}, h.commonArgs...)...)
	for _, flags := range [][]string{
		{},                        // neither
		{enableFlag, disableFlag}, // both
	} {
		args := append(append([]string{nestedSubName}, flags...), h.commonArgs...)
		if _, err := h.runErr(args...); err == nil {
			t.Errorf("nested %v: expected error, got nil", flags)
		}
	}
}

// TestWorkspacePrivilegedFuseEnable drives the privileged-fuse
// subcommand end-to-end: --enable calls SetPrivilegedFuseMounts and
// the rendered summary surfaces the grant (with its CAP_SYS_ADMIN
// caveat).
func TestWorkspacePrivilegedFuseEnable(t *testing.T) {
	h := newWorkspaceE2EHarness(t)
	h.run(append([]string{createSubName}, h.commonArgs...)...)
	out := h.run(append([]string{fuseSubName, enableFlag}, h.commonArgs...)...)
	if !strings.Contains(out, "Privileged-FUSE:") || !strings.Contains(out, "CAP_SYS_ADMIN") {
		t.Errorf("privileged-fuse --enable summary missing Privileged-FUSE/CAP_SYS_ADMIN row:\n%s", out)
	}
}

// TestWorkspacePrivilegedFuseRequiresExactlyOneFlag pins the same
// client-side guard for the FUSE grant: neither flag (or both) is a
// usage error that never reaches the wire.
func TestWorkspacePrivilegedFuseRequiresExactlyOneFlag(t *testing.T) {
	h := newWorkspaceE2EHarness(t)
	h.run(append([]string{createSubName}, h.commonArgs...)...)
	for _, flags := range [][]string{
		{},                        // neither
		{enableFlag, disableFlag}, // both
	} {
		args := append(append([]string{fuseSubName}, flags...), h.commonArgs...)
		if _, err := h.runErr(args...); err == nil {
			t.Errorf("privileged-fuse %v: expected error, got nil", flags)
		}
	}
}

// TestWorkspacePrivilegedBuildEnable drives the privileged-build
// subcommand end-to-end: --enable calls SetPrivilegedBuild and the
// rendered summary surfaces the grant.
func TestWorkspacePrivilegedBuildEnable(t *testing.T) {
	h := newWorkspaceE2EHarness(t)
	h.run(append([]string{createSubName}, h.commonArgs...)...)
	out := h.run(append([]string{buildSubName, enableFlag}, h.commonArgs...)...)
	if !strings.Contains(out, "Privileged-Build:") {
		t.Errorf("privileged-build --enable summary missing Privileged-Build row:\n%s", out)
	}
}

// TestWorkspacePrivilegedBuildRequiresExactlyOneFlag pins the client-side
// guard for the build grant: neither flag (or both) is a usage error that
// never reaches the wire.
func TestWorkspacePrivilegedBuildRequiresExactlyOneFlag(t *testing.T) {
	h := newWorkspaceE2EHarness(t)
	h.run(append([]string{createSubName}, h.commonArgs...)...)
	for _, flags := range [][]string{
		{},                        // neither
		{enableFlag, disableFlag}, // both
	} {
		args := append(append([]string{buildSubName}, flags...), h.commonArgs...)
		if _, err := h.runErr(args...); err == nil {
			t.Errorf("privileged-build %v: expected error, got nil", flags)
		}
	}
}

func TestParseMemoryBytes(t *testing.T) {
	cases := map[string]uint64{
		"":     0,
		"1024": 1024,
		"512M": 512 << 20,
		"4G":   4 << 30,
		"2k":   2 << 10,
		"1T":   1 << 40,
	}
	for in, want := range cases {
		got, err := parseMemoryBytes(in)
		if err != nil {
			t.Errorf("parseMemoryBytes(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseMemoryBytes(%q) = %d, want %d", in, got, want)
		}
	}
	if _, err := parseMemoryBytes("nope"); err == nil {
		t.Error("parseMemoryBytes(nope): want error")
	}
	if _, err := parseMemoryBytes("18446744073709551615T"); err == nil {
		t.Error("parseMemoryBytes overflow: want error")
	}
}

// TestWorkspaceAcquireRejectsZeroTTL pins the InvalidArgument
// guard at the wire — passing --ttl 0s surfaces the error from
// the orchestrator handler.
func TestWorkspaceAcquireRejectsZeroTTL(t *testing.T) {
	h := newWorkspaceE2EHarness(t)
	h.run(append([]string{createSubName}, h.commonArgs...)...)
	args := append([]string{acquireSubName, "--ttl", "0s"}, h.commonArgs...)
	_, err := h.runErr(args...)
	if err == nil {
		t.Error("expected error for --ttl 0s")
	}
}

// TestWorkspaceMountMappingRecordsAndRegisters drives the positional
// `daft workspace mount <local> <remote>` form: it must register the
// mount on the orchestrator AND record the local↔in-VM mapping in
// ./.daft/config.json so push/pull can find it.
func TestWorkspaceMountMappingRecordsAndRegisters(t *testing.T) {
	chdirClean(t)
	clearOrchestratorEnv(t)
	local := t.TempDir()
	h := newWorkspaceE2EHarness(t)
	h.run(append([]string{createSubName}, h.commonArgs...)...)

	out := h.run(append([]string{mountSubName, local, testMountRemote}, h.commonArgs...)...)
	if !strings.Contains(out, "recorded in") {
		t.Errorf("mount output = %q, want a 'recorded in' line", out)
	}
	mounts, err := dirLocalMounts()
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 || mounts[0].Remote != testMountRemote || mounts[0].Local != local {
		t.Fatalf("dir-local mounts = %+v, want [{%s /workspace/src}]", mounts, local)
	}
	// Bare `mount` lists the mapping.
	listOut := h.run(append([]string{mountSubName}, h.commonArgs...)...)
	if !strings.Contains(listOut, testMountRemote) {
		t.Errorf("mount list = %q, want the /workspace/src mapping", listOut)
	}
	// `mount rm` drops both the spec mount and the dir-local mapping.
	h.run(append([]string{mountSubName, rmSubName, testMountRemote}, h.commonArgs...)...)
	if mounts, _ := dirLocalMounts(); len(mounts) != 0 {
		t.Errorf("after rm, dir-local mounts = %+v, want empty", mounts)
	}
}

func TestWorkspaceMountRemoveRejectsCorruptLocalConfigBeforeCatalogMutation(t *testing.T) {
	chdirClean(t)
	clearOrchestratorEnv(t)
	local := t.TempDir()
	h := newWorkspaceE2EHarness(t)
	h.run(append([]string{createSubName}, h.commonArgs...)...)
	h.run(append([]string{mountSubName, local, testMountRemote}, h.commonArgs...)...)
	writeBadDirLocal(t)

	if _, err := h.runErr(append([]string{mountSubName, rmSubName, testMountRemote}, h.commonArgs...)...); err == nil {
		t.Fatal("mount rm with corrupt local config succeeded")
	}
	workspaces := h.store.List(nil)
	if len(workspaces) != 1 || len(workspaces[0].Spec.Mounts) != 1 || workspaces[0].Spec.Mounts[0].Target != testMountRemote {
		t.Fatalf("catalog mounts after rejected remove = %+v, want %s retained", workspaces, testMountRemote)
	}
}

func TestWorkspaceMountRemoveRollsCatalogBackAfterLocalWriteFailure(t *testing.T) {
	chdirClean(t)
	clearOrchestratorEnv(t)
	local := t.TempDir()
	h := newWorkspaceE2EHarness(t)
	h.run(append([]string{createSubName}, h.commonArgs...)...)
	h.run(append([]string{mountSubName, local, testMountRemote}, h.commonArgs...)...)
	previous := removeDirLocalMountForCatalog
	removeDirLocalMountForCatalog = func(string) (bool, error) { return false, errors.New("injected local write failure") }
	t.Cleanup(func() { removeDirLocalMountForCatalog = previous })

	if _, err := h.runErr(append([]string{mountSubName, rmSubName, testMountRemote}, h.commonArgs...)...); err == nil {
		t.Fatal("mount rm unexpectedly ignored local write failure")
	}
	workspaces := h.store.List(nil)
	if len(workspaces) != 1 || len(workspaces[0].Spec.Mounts) != 1 || workspaces[0].Spec.Mounts[0].Target != testMountRemote {
		t.Fatalf("catalog mounts after compensated remove = %+v, want %s retained", workspaces, testMountRemote)
	}
}

// TestWorkspaceMountMapValidation pins the up-front checks that run
// before any RPC: the in-VM target must be absolute and the local
// path must be an existing directory.
func TestWorkspaceMountMapValidation(t *testing.T) {
	chdirClean(t)
	clearOrchestratorEnv(t)
	if _, _, err := runDaft(t, "workspace", "mount", t.TempDir(), "relative/path"); err == nil {
		t.Error("relative in-VM target should error")
	}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, _, err := runDaft(t, "workspace", "mount", missing, "/remote"); err == nil {
		t.Error("non-existent local dir should error")
	}
}

// TestWorkspaceMountBareListsEmpty: with no mappings recorded, the
// bare command prints the empty-state line rather than erroring.
func TestWorkspaceMountBareListsEmpty(t *testing.T) {
	chdirClean(t)
	out, _, err := runDaft(t, "workspace", "mount")
	if err != nil {
		t.Fatalf("daft workspace mount: %v", err)
	}
	if !strings.Contains(out, "no mounts recorded") {
		t.Errorf("bare mount output = %q, want empty-state line", out)
	}
}

// TestWorkspaceMountMapSurfacesConfigWriteError: AddMount succeeds on
// the orchestrator, but recording the dir-local mapping then fails on
// a corrupt ./.daft/config.json. The error must surface (and the
// command rolls the server mount back so the two sides don't drift).
func TestWorkspaceMountMapSurfacesConfigWriteError(t *testing.T) {
	chdirClean(t)
	clearOrchestratorEnv(t)
	writeBadDirLocal(t)
	local := t.TempDir()
	h := newWorkspaceE2EHarness(t)
	h.run(append([]string{createSubName}, h.commonArgs...)...)
	if _, err := h.runErr(append([]string{mountSubName, local, testMountRemote}, h.commonArgs...)...); err == nil {
		t.Error("mount with corrupt dir-local config: want error after AddMount")
	}
}

// TestWorkspaceMountMapRPCError: the positional mount form must
// surface a failing AddMount (here: a workspace that was never
// created) rather than recording a dangling local mapping.
func TestWorkspaceMountMapRPCError(t *testing.T) {
	chdirClean(t)
	clearOrchestratorEnv(t)
	local := t.TempDir()
	h := newWorkspaceE2EHarness(t)
	// Deliberately skip create → AddMount targets a missing workspace.
	if _, err := h.runErr(append([]string{mountSubName, local, testMountRemote}, h.commonArgs...)...); err == nil {
		t.Error("mount on a missing workspace: want RPC error")
	}
}
