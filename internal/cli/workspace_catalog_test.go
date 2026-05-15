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

	sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/orchestrator"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

// catalogFakeDispatcher mirrors the in-orchestrator fake (which
// can't be imported across package boundaries).
type catalogFakeDispatcher struct {
	mu    sync.Mutex
	calls []catalogDispatchCall
}

type catalogDispatchCall struct {
	fp        identity.Fingerprint
	eventType sandboxv1.EventType
	sandbox   *sandboxv1.Sandbox
}

func (f *catalogFakeDispatcher) PublishSandboxEvent(fp identity.Fingerprint, eventType sandboxv1.EventType, sandbox *sandboxv1.Sandbox) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, catalogDispatchCall{fp: fp, eventType: eventType, sandbox: sandbox})
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
	}
	for _, v := range wantVerbs {
		if !strings.Contains(stdout, v) {
			t.Errorf("workspace --help missing %q\nfull help:\n%s", v, stdout)
		}
	}
}

// TestWorkspaceCreateRequiresOrchestrator pins the up-front flag
// guard. The test deliberately doesn't dial anything.
func TestWorkspaceCreateRequiresOrchestrator(t *testing.T) {
	_, _, err := runDaft(t, "workspace", "create", nameFlag, nameFlagVal)
	if err == nil || !strings.Contains(err.Error(), "--orchestrator is required") {
		t.Errorf("err = %v, want --orchestrator required", err)
	}
}

// TestWorkspaceRPCErrorsBubbleUp drives several catalog commands
// against a workspace name that doesn't exist. The orchestrator
// returns NotFound and the CLI must surface a non-nil error from
// each command — exercising the post-dial err-return path in
// every thin wrapper.
func TestWorkspaceRPCErrorsBubbleUp(t *testing.T) {
	h := newWorkspaceE2EHarness(t)
	missing := []string{"--orchestrator", h.orchURL, configDirArg, h.clientDir, nameFlag, "no-such-workspace"}
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

// workspaceE2EHarness threads the in-process orchestrator + caller
// identity through the multi-step E2E test below.
type workspaceE2EHarness struct {
	t          *testing.T
	clientDir  string
	orchURL    string
	commonArgs []string
	fake       *catalogFakeDispatcher
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
	addr, srvID := runOrchestratorWithCatalog(t, srvStore, workspacecatalog.NewStore(nil), fake)
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
		sandboxFP: mustGenerate(t).Fingerprint().String(),
		commonArgs: []string{
			"--orchestrator", orchURL,
			configDirArg, clientDir,
			"--name", testE2EWorkspaceName,
		},
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
	listOut := h.run(listSubName, "--orchestrator", h.orchURL, configDirArg, h.clientDir, "--mine")
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
	nameFlag    = "--name"
	nameFlagVal = "x"
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
			ImageRef: testCatalogImage,
			Mounts:   []*sandboxv1.Mount{{Target: "/x"}, {Target: "/y", ReadOnly: true}},
			Env:      []*sandboxv1.EnvVar{{Name: "K"}},
			Ports:    []*sandboxv1.PortMapping{{ContainerPort: 80, Protocol: sandboxv1.Protocol_PROTOCOL_TCP}},
		},
		Status: &workspacev1.WorkspaceStatus{
			Missing: []string{"target"},
		},
	})
	if err != nil {
		t.Fatalf("writeWorkspaceSummary: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Missing:", "target", "/x", "/y ro", "K", "80/tcp"} {
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
