package orchestrator

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	sandboxv1 "github.com/localai-org/darkaifactory/internal/genpb/sandbox/v1"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/workspacecatalog"
)

const (
	// testImageRef is the image ref used by every test that exercises
	// SetImage / image-missing paths.
	testImageRef = "alpine:3"
	// testMountTarget is the canonical in-VM mount target used
	// across the Add/Remove mount tests.
	testMountTarget = "/workspace"
	// testMountSource is the canonical Source value paired with
	// testMountTarget — opaque to the catalog at this stage.
	testMountSource = "src"
)

// TestServerExposesWorkspaceCatalog mirrors
// TestServerExposesNodeRegistry — a configured catalog flows
// through New and is returned by Server.WorkspaceCatalog().
func TestServerExposesWorkspaceCatalog(t *testing.T) {
	srvID := mustGenerate(t, identity.RoleOrchestrator)
	store := mustOpenStoreForWorkspaceCatalog(t)
	cat := workspacecatalog.NewStore(nil)
	srv, err := New(Config{
		Identity:         srvID,
		Peers:            store,
		WorkspaceCatalog: cat,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv.WorkspaceCatalog() != cat {
		t.Error("WorkspaceCatalog() did not return the configured catalog")
	}
}

// TestServerCreatesDefaultWorkspaceCatalog verifies New mints a
// default catalog when Config.WorkspaceCatalog is nil.
func TestServerCreatesDefaultWorkspaceCatalog(t *testing.T) {
	srvID := mustGenerate(t, identity.RoleOrchestrator)
	store := mustOpenStoreForWorkspaceCatalog(t)
	srv, err := New(Config{Identity: srvID, Peers: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv.WorkspaceCatalog() == nil {
		t.Error("WorkspaceCatalog() = nil, want default")
	}
}

func mustOpenStoreForWorkspaceCatalog(t *testing.T) *identity.PeerStore {
	t.Helper()
	s, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	return s
}

// TestNewWorkspaceCatalogHandlerRejectsNilStore covers the
// constructor's input guard.
func TestNewWorkspaceCatalogHandlerRejectsNilStore(t *testing.T) {
	if _, err := NewWorkspaceCatalogHandler(nil, nil); err == nil {
		t.Error("expected error for nil store")
	}
}

// TestWorkspaceCatalogHandlerRejectsMissingPeerIdentity covers
// every RPC's auth check.
func TestWorkspaceCatalogHandlerRejectsMissingPeerIdentity(t *testing.T) {
	h, err := NewWorkspaceCatalogHandler(workspacecatalog.NewStore(nil), nil)
	if err != nil {
		t.Fatalf("NewWorkspaceCatalogHandler: %v", err)
	}
	checks := []struct {
		name string
		fn   func() error
	}{
		{"Create", func() error {
			_, e := h.Create(t.Context(),
				connect.NewRequest(&workspacev1.CreateWorkspaceRequest{Name: "x"}))
			return e
		}},
		{"Get", func() error {
			_, e := h.Get(t.Context(),
				connect.NewRequest(&workspacev1.GetWorkspaceRequest{}))
			return e
		}},
		{"List", func() error {
			_, e := h.List(t.Context(),
				connect.NewRequest(&workspacev1.ListWorkspacesRequest{}))
			return e
		}},
		{"SetImage", func() error {
			_, e := h.SetImage(t.Context(),
				connect.NewRequest(&workspacev1.SetImageRequest{}))
			return e
		}},
		{"SetTarget", func() error {
			_, e := h.SetTarget(t.Context(),
				connect.NewRequest(&workspacev1.SetTargetRequest{}))
			return e
		}},
		{"AddMount", func() error {
			_, e := h.AddMount(t.Context(),
				connect.NewRequest(&workspacev1.AddMountRequest{}))
			return e
		}},
		{"AddEnv", func() error {
			_, e := h.AddEnv(t.Context(),
				connect.NewRequest(&workspacev1.AddEnvRequest{}))
			return e
		}},
		{"AddPort", func() error {
			_, e := h.AddPort(t.Context(),
				connect.NewRequest(&workspacev1.AddPortRequest{}))
			return e
		}},
		{"RemoveMount", func() error {
			_, e := h.RemoveMount(t.Context(),
				connect.NewRequest(&workspacev1.RemoveMountRequest{Target: "/x"}))
			return e
		}},
		{"RemoveEnv", func() error {
			_, e := h.RemoveEnv(t.Context(),
				connect.NewRequest(&workspacev1.RemoveEnvRequest{Name: "X"}))
			return e
		}},
		{"RemovePort", func() error {
			_, e := h.RemovePort(t.Context(),
				connect.NewRequest(&workspacev1.RemovePortRequest{
					ContainerPort: 80, Protocol: sandboxv1.Protocol_PROTOCOL_TCP,
				}))
			return e
		}},
	}
	for _, c := range checks {
		if code := connect.CodeOf(c.fn()); code != connect.CodeUnauthenticated {
			t.Errorf("%s: code = %s, want Unauthenticated", c.name, code)
		}
	}
}

// workspaceCatalogRig wires an in-process orchestrator + a Connect
// client for the WorkspaceCatalogService. Mirrors sandboxCtrlRig.
type workspaceCatalogRig struct {
	server   *Server
	catalog  *workspacecatalog.Store
	callerID *identity.Identity
	client   workspacev1connect.WorkspaceCatalogServiceClient
	// dispatcher is the fake injected when newWorkspaceCatalogRig
	// is called with a non-nil dispatcher option; nil otherwise so
	// the production NodeControlHandler is in play.
	dispatcher *fakeDispatcher
}

func newWorkspaceCatalogRig(t *testing.T) *workspaceCatalogRig {
	return newWorkspaceCatalogRigWith(t, nil)
}

func newWorkspaceCatalogRigWith(t *testing.T, dispatcher *fakeDispatcher) *workspaceCatalogRig {
	t.Helper()
	cat := workspacecatalog.NewStore(nil)
	cfg := Config{WorkspaceCatalog: cat}
	if dispatcher != nil {
		cfg.WorkspaceDispatcher = dispatcher
	}
	env := newOrchTestEnv(t, identity.RoleClient, cfg)
	client := workspacev1connect.NewWorkspaceCatalogServiceClient(env.httpc, env.baseURL)
	return &workspaceCatalogRig{
		server:     env.server,
		catalog:    cat,
		callerID:   env.callerID,
		client:     client,
		dispatcher: dispatcher,
	}
}

// fakeDispatcher records every PublishSandboxEvent call. The
// next-error knob lets tests cover the FailedPrecondition /
// Unavailable / Internal mappings without going through a real
// Node stream.
type fakeDispatcher struct {
	mu        sync.Mutex
	calls     []dispatchCall
	nextError error
}

type dispatchCall struct {
	fp        identity.Fingerprint
	eventType sandboxv1.EventType
	sandbox   *sandboxv1.Sandbox
}

func (f *fakeDispatcher) PublishSandboxEvent(fp identity.Fingerprint, eventType sandboxv1.EventType, sandbox *sandboxv1.Sandbox) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nextError != nil {
		err := f.nextError
		f.nextError = nil
		return err
	}
	f.calls = append(f.calls, dispatchCall{fp: fp, eventType: eventType, sandbox: sandbox})
	return nil
}

// conditionIsTrue is the test-side mirror of
// workspacecatalog.Status.IsTrue. Returns true iff a condition
// with the given Type exists and reports CONDITION_STATUS_TRUE.
func conditionIsTrue(status *workspacev1.WorkspaceStatus, typ string) bool {
	for _, c := range status.GetConditions() {
		if c.GetType() == typ && c.GetStatus() == sandboxv1.ConditionStatus_CONDITION_STATUS_TRUE {
			return true
		}
	}
	return false
}

// conditionReason returns the Reason field of the condition with
// the given Type, or "" if it doesn't exist. Useful when the test
// wants to assert *why* a condition is False.
func conditionReason(status *workspacev1.WorkspaceStatus, typ string) string {
	for _, c := range status.GetConditions() {
		if c.GetType() == typ {
			return c.GetReason()
		}
	}
	return ""
}

func (f *fakeDispatcher) last() (dispatchCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return dispatchCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// TestCreateMintsUUIDAndStampsOwner pins that Create generates an
// RFC-4122 v4 UUID and stamps the caller's mTLS fingerprint as the
// owner. The wire echo includes both, so the CLI can subsequently
// address the workspace by either handle.
func TestCreateMintsUUIDAndStampsOwner(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	resp, err := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "myapp"},
	))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ws := resp.Msg.GetWorkspace()
	if ws.GetMetadata().GetUuid() == "" {
		t.Error("Workspace.uuid is empty")
	}
	if ws.GetMetadata().GetName() != "myapp" {
		t.Errorf("Workspace.name = %q, want %q", ws.GetMetadata().GetName(), "myapp")
	}
	if ws.GetMetadata().GetOwnerFingerprint() != rig.callerID.Fingerprint().String() {
		t.Errorf("Workspace.owner_fingerprint = %q, want caller's", ws.GetMetadata().GetOwnerFingerprint())
	}
	if conditionIsTrue(ws.GetStatus(), workspacecatalog.ConditionReady) {
		t.Errorf("fresh workspace should not be Ready; conditions=%v", ws.GetStatus().GetConditions())
	}
	if reason := conditionReason(ws.GetStatus(), workspacecatalog.ConditionReady); reason != "MissingFields" {
		t.Errorf("Ready.reason = %q, want MissingFields", reason)
	}
	if len(ws.GetStatus().GetMissing()) == 0 {
		t.Error("Workspace.missing is empty; expected at least mount/image/target")
	}
	if ws.GetMetadata().GetGeneration() != 1 {
		t.Errorf("generation = %d, want 1 on Create", ws.GetMetadata().GetGeneration())
	}
}

func TestCreateRejectsEmptyName(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	_, err := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{},
	))
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Errorf("err code = %s, want InvalidArgument", code)
	}
}

func TestCreateDuplicateNameMapsToAlreadyExists(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	if _, err := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "dup"},
	)); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "dup"},
	))
	if code := connect.CodeOf(err); code != connect.CodeAlreadyExists {
		t.Errorf("err code = %s, want AlreadyExists", code)
	}
}

// TestGetByUUIDAndByNameAgree confirms both addressing modes
// resolve to the same workspace.
func TestGetByUUIDAndByNameAgree(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, err := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "addr"},
	))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	uuid := created.Msg.GetWorkspace().GetMetadata().GetUuid()

	byUUID, err := rig.client.Get(t.Context(), connect.NewRequest(
		&workspacev1.GetWorkspaceRequest{
			Handle: &workspacev1.WorkspaceHandle{
				Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: uuid},
			},
		},
	))
	if err != nil {
		t.Fatalf("Get by uuid: %v", err)
	}
	byName, err := rig.client.Get(t.Context(), connect.NewRequest(
		&workspacev1.GetWorkspaceRequest{
			Handle: &workspacev1.WorkspaceHandle{
				Handle: &workspacev1.WorkspaceHandle_Name{Name: "addr"},
			},
		},
	))
	if err != nil {
		t.Fatalf("Get by name: %v", err)
	}
	if byUUID.Msg.GetWorkspace().GetMetadata().GetUuid() != byName.Msg.GetWorkspace().GetMetadata().GetUuid() {
		t.Errorf("uuid mismatch: byUUID=%q byName=%q",
			byUUID.Msg.GetWorkspace().GetMetadata().GetUuid(),
			byName.Msg.GetWorkspace().GetMetadata().GetUuid())
	}
}

func TestGetUnknownUUIDMapsToNotFound(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	_, err := rig.client.Get(t.Context(), connect.NewRequest(
		&workspacev1.GetWorkspaceRequest{
			Handle: &workspacev1.WorkspaceHandle{
				Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: "no-such-uuid"},
			},
		},
	))
	if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Errorf("err code = %s, want NotFound", code)
	}
}

// TestGetEmptyHandleAndUnsetHandleRejected covers the two
// invalid-input paths through resolveHandle: nil handle and a
// handle with an empty oneof variant.
func TestGetEmptyHandleAndUnsetHandleRejected(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	// Nil handle.
	_, err := rig.client.Get(t.Context(), connect.NewRequest(
		&workspacev1.GetWorkspaceRequest{},
	))
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Errorf("nil handle: code = %s, want InvalidArgument", code)
	}
	// Empty uuid string in the uuid variant.
	_, err = rig.client.Get(t.Context(), connect.NewRequest(
		&workspacev1.GetWorkspaceRequest{
			Handle: &workspacev1.WorkspaceHandle{
				Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: ""},
			},
		},
	))
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Errorf("empty uuid: code = %s, want InvalidArgument", code)
	}
	// Empty name string in the name variant.
	_, err = rig.client.Get(t.Context(), connect.NewRequest(
		&workspacev1.GetWorkspaceRequest{
			Handle: &workspacev1.WorkspaceHandle{
				Handle: &workspacev1.WorkspaceHandle_Name{Name: ""},
			},
		},
	))
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Errorf("empty name: code = %s, want InvalidArgument", code)
	}
}

// TestListMineOnlyFiltersByOwner pins the mine_only filter. The
// rig uses one caller identity, so we seed the underlying store
// with a second-owner workspace directly to assert the filter
// path drops it.
func TestListMineOnlyFiltersByOwner(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	if _, err := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "mine"},
	)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Seed a foreign-owner workspace into the store directly.
	otherID := mustGenerate(t, identity.RoleClient)
	if _, err := rig.catalog.Create(workspacecatalog.CreateInput{
		UUID:             "11111111-1111-4111-8111-aaaaaaaaaaaa",
		Name:             "theirs",
		OwnerFingerprint: otherID.Fingerprint(),
	}); err != nil {
		t.Fatalf("seed foreign: %v", err)
	}

	all, err := rig.client.List(t.Context(), connect.NewRequest(
		&workspacev1.ListWorkspacesRequest{MineOnly: false},
	))
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if got := len(all.Msg.GetWorkspaces()); got != 2 {
		t.Errorf("List all = %d, want 2", got)
	}

	mine, err := rig.client.List(t.Context(), connect.NewRequest(
		&workspacev1.ListWorkspacesRequest{MineOnly: true},
	))
	if err != nil {
		t.Fatalf("List mine: %v", err)
	}
	if got := len(mine.Msg.GetWorkspaces()); got != 1 {
		t.Errorf("List mine = %d, want 1", got)
	}
	if mine.Msg.GetWorkspaces()[0].GetMetadata().GetName() != "mine" {
		t.Errorf("List mine[0].name = %q, want mine", mine.Msg.GetWorkspaces()[0].GetMetadata().GetName())
	}
}

// TestIncrementalAssemblyRetiresMissingFields drives the full
// "create + add mount + set image + set target" sequence that the
// B3 CLI will issue, and confirms that Workspace.missing[] empties
// as each field lands. This is the integration test for the
// happy-path workflow.
func TestIncrementalAssemblyRetiresMissingFields(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, err := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "asm"},
	))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	handle := &workspacev1.WorkspaceHandle{
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: created.Msg.GetWorkspace().GetMetadata().GetUuid()},
	}

	afterMount, err := rig.client.AddMount(t.Context(), connect.NewRequest(
		&workspacev1.AddMountRequest{
			Handle: handle,
			Mount: &sandboxv1.Mount{
				Target: testMountTarget,
				Source: "user-host-cwd",
			},
		},
	))
	if err != nil {
		t.Fatalf("AddMount: %v", err)
	}
	if hasMissing(afterMount.Msg.GetWorkspace().GetStatus().GetMissing(), "mount") {
		t.Error("missing still contains 'mount' after AddMount")
	}

	afterImg, err := rig.client.SetImage(t.Context(), connect.NewRequest(
		&workspacev1.SetImageRequest{Handle: handle, ImageRef: testImageRef},
	))
	if err != nil {
		t.Fatalf("SetImage: %v", err)
	}
	if hasMissing(afterImg.Msg.GetWorkspace().GetStatus().GetMissing(), "image") {
		t.Error("missing still contains 'image' after SetImage")
	}

	final, err := rig.client.SetTarget(t.Context(), connect.NewRequest(
		&workspacev1.SetTargetRequest{Handle: handle, SandboxFingerprint: "sha256:abc"},
	))
	if err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	if got := len(final.Msg.GetWorkspace().GetStatus().GetMissing()); got != 0 {
		t.Errorf("missing = %v, want empty", final.Msg.GetWorkspace().GetStatus().GetMissing())
	}
}

// TestAddMountRejectsRelativeTarget pins the validation that the
// store doesn't enforce but the wire surface does.
func TestAddMountRejectsRelativeTarget(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, err := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "ws"},
	))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	handle := &workspacev1.WorkspaceHandle{
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: created.Msg.GetWorkspace().GetMetadata().GetUuid()},
	}
	cases := []struct {
		name   string
		target string
	}{
		{"empty", ""},
		{"relative", "workspace"},
		{"dot", "./workspace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rig.client.AddMount(t.Context(), connect.NewRequest(
				&workspacev1.AddMountRequest{
					Handle: handle,
					Mount:  &sandboxv1.Mount{Target: tc.target},
				},
			))
			if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
				t.Errorf("code = %s, want InvalidArgument", code)
			}
		})
	}
}

// TestAddMountRejectsDuplicateTarget pins the in-memory dedupe of
// mount targets so the SandboxSpec projection at Launch doesn't
// end up with two mounts at the same path.
func TestAddMountRejectsDuplicateTarget(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, err := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "dup-mount"},
	))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	handle := &workspacev1.WorkspaceHandle{
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: created.Msg.GetWorkspace().GetMetadata().GetUuid()},
	}
	build := func() *workspacev1.AddMountRequest {
		return &workspacev1.AddMountRequest{
			Handle: handle,
			Mount:  &sandboxv1.Mount{Target: testMountTarget, Source: "a"},
		}
	}
	if _, err := rig.client.AddMount(t.Context(), connect.NewRequest(build())); err != nil {
		t.Fatalf("first AddMount: %v", err)
	}
	_, err = rig.client.AddMount(t.Context(), connect.NewRequest(build()))
	if code := connect.CodeOf(err); code != connect.CodeAlreadyExists {
		t.Errorf("dup AddMount: code = %s, want AlreadyExists", code)
	}
}

func TestAddMountRejectsNilMount(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "nm"},
	))
	handle := &workspacev1.WorkspaceHandle{
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: created.Msg.GetWorkspace().GetMetadata().GetUuid()},
	}
	_, err := rig.client.AddMount(t.Context(), connect.NewRequest(
		&workspacev1.AddMountRequest{Handle: handle},
	))
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", code)
	}
}

func TestAddEnvRejectsEmptyNameAndDuplicate(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "env"},
	))
	handle := &workspacev1.WorkspaceHandle{
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: created.Msg.GetWorkspace().GetMetadata().GetUuid()},
	}
	if _, err := rig.client.AddEnv(t.Context(), connect.NewRequest(
		&workspacev1.AddEnvRequest{Handle: handle, Env: &sandboxv1.EnvVar{}},
	)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("empty name: code = %s, want InvalidArgument", connect.CodeOf(err))
	}
	if _, err := rig.client.AddEnv(t.Context(), connect.NewRequest(
		&workspacev1.AddEnvRequest{Handle: handle, Env: &sandboxv1.EnvVar{Name: "K", Value: "v"}},
	)); err != nil {
		t.Fatalf("first AddEnv: %v", err)
	}
	if _, err := rig.client.AddEnv(t.Context(), connect.NewRequest(
		&workspacev1.AddEnvRequest{Handle: handle, Env: &sandboxv1.EnvVar{Name: "K", Value: "v2"}},
	)); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("dup AddEnv: code = %s, want AlreadyExists", connect.CodeOf(err))
	}
}

func TestAddPortRejectsUnspecifiedProtocol(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "port"},
	))
	handle := &workspacev1.WorkspaceHandle{
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: created.Msg.GetWorkspace().GetMetadata().GetUuid()},
	}
	cases := []struct {
		name string
		port *sandboxv1.PortMapping
		want connect.Code
	}{
		{"nil port", nil, connect.CodeInvalidArgument},
		{
			"unspecified proto",
			&sandboxv1.PortMapping{ContainerPort: 3000},
			connect.CodeInvalidArgument,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := rig.client.AddPort(t.Context(), connect.NewRequest(
				&workspacev1.AddPortRequest{Handle: handle, Port: tc.port},
			))
			if code := connect.CodeOf(err); code != tc.want {
				t.Errorf("code = %s, want %s", code, tc.want)
			}
		})
	}
}

func TestAddPortAllowsSamePortDifferentProtocol(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "pp"},
	))
	handle := &workspacev1.WorkspaceHandle{
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: created.Msg.GetWorkspace().GetMetadata().GetUuid()},
	}
	if _, err := rig.client.AddPort(t.Context(), connect.NewRequest(
		&workspacev1.AddPortRequest{
			Handle: handle,
			Port:   &sandboxv1.PortMapping{ContainerPort: 3000, Protocol: sandboxv1.Protocol_PROTOCOL_TCP},
		},
	)); err != nil {
		t.Fatalf("first AddPort: %v", err)
	}
	if _, err := rig.client.AddPort(t.Context(), connect.NewRequest(
		&workspacev1.AddPortRequest{
			Handle: handle,
			Port:   &sandboxv1.PortMapping{ContainerPort: 3000, Protocol: sandboxv1.Protocol_PROTOCOL_UDP},
		},
	)); err != nil {
		t.Errorf("same-port-different-proto: %v, want nil", err)
	}
	if _, err := rig.client.AddPort(t.Context(), connect.NewRequest(
		&workspacev1.AddPortRequest{
			Handle: handle,
			Port:   &sandboxv1.PortMapping{ContainerPort: 3000, Protocol: sandboxv1.Protocol_PROTOCOL_TCP},
		},
	)); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("dup port/proto: code = %s, want AlreadyExists", connect.CodeOf(err))
	}
}

func TestSetImageClearsField(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "img"},
	))
	handle := &workspacev1.WorkspaceHandle{
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: created.Msg.GetWorkspace().GetMetadata().GetUuid()},
	}
	if _, err := rig.client.SetImage(t.Context(), connect.NewRequest(
		&workspacev1.SetImageRequest{Handle: handle, ImageRef: testImageRef},
	)); err != nil {
		t.Fatalf("SetImage: %v", err)
	}
	cleared, err := rig.client.SetImage(t.Context(), connect.NewRequest(
		&workspacev1.SetImageRequest{Handle: handle, ImageRef: ""},
	))
	if err != nil {
		t.Fatalf("SetImage clear: %v", err)
	}
	if cleared.Msg.GetWorkspace().GetSpec().GetImageRef() != "" {
		t.Errorf("image_ref = %q, want empty after clear", cleared.Msg.GetWorkspace().GetSpec().GetImageRef())
	}
	if !hasMissing(cleared.Msg.GetWorkspace().GetStatus().GetMissing(), "image") {
		t.Error("missing should re-include 'image' after clear")
	}
}

func TestSetResourcesRoundTripsAndClears(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "res"},
	))
	handle := uuidHandle(created.Msg.GetWorkspace().GetMetadata().GetUuid())

	const wantMem = uint64(4) << 30
	const wantCPU = uint32(4)
	set, err := rig.client.SetResources(t.Context(), connect.NewRequest(
		&workspacev1.SetResourcesRequest{Handle: handle, MemoryBytes: wantMem, CpuCores: wantCPU},
	))
	if err != nil {
		t.Fatalf("SetResources: %v", err)
	}
	spec := set.Msg.GetWorkspace().GetSpec()
	if spec.GetMemoryBytes() != wantMem || spec.GetCpuCores() != wantCPU {
		t.Errorf("got memory=%d cpus=%d, want %d/%d",
			spec.GetMemoryBytes(), spec.GetCpuCores(), wantMem, wantCPU)
	}

	cleared, err := rig.client.SetResources(t.Context(), connect.NewRequest(
		&workspacev1.SetResourcesRequest{Handle: handle, MemoryBytes: 0, CpuCores: 0},
	))
	if err != nil {
		t.Fatalf("SetResources clear: %v", err)
	}
	cspec := cleared.Msg.GetWorkspace().GetSpec()
	if cspec.GetMemoryBytes() != 0 || cspec.GetCpuCores() != 0 {
		t.Errorf("after clear got memory=%d cpus=%d, want 0/0",
			cspec.GetMemoryBytes(), cspec.GetCpuCores())
	}
}

// TestRemoveMountDropsTheTargetAndReReinstatesMissing pins the
// symmetric "AddMount → RemoveMount" cycle: after AddMount removes
// "mount" from Missing[], RemoveMount must put it back when the
// last mount drops out.
func TestRemoveMountDropsTheTargetAndReReinstatesMissing(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "rm-mount"},
	))
	handle := uuidHandle(created.Msg.GetWorkspace().GetMetadata().GetUuid())
	if _, err := rig.client.AddMount(t.Context(), connect.NewRequest(
		&workspacev1.AddMountRequest{
			Handle: handle,
			Mount:  &sandboxv1.Mount{Target: testMountTarget, Source: testMountSource},
		},
	)); err != nil {
		t.Fatalf("AddMount: %v", err)
	}
	got, err := rig.client.RemoveMount(t.Context(), connect.NewRequest(
		&workspacev1.RemoveMountRequest{Handle: handle, Target: testMountTarget},
	))
	if err != nil {
		t.Fatalf("RemoveMount: %v", err)
	}
	if len(got.Msg.GetWorkspace().GetSpec().GetMounts()) != 0 {
		t.Errorf("Mounts = %d, want 0", len(got.Msg.GetWorkspace().GetSpec().GetMounts()))
	}
	if !hasMissing(got.Msg.GetWorkspace().GetStatus().GetMissing(), "mount") {
		t.Error("missing should include 'mount' after RemoveMount drops the last")
	}
}

//nolint:dupl // Structurally parallel to the RemoveEnv / RemovePort unknown-key tests by design — each pins one RPC's missing-key error mapping.
func TestRemoveMountUnknownTargetMapsToNotFound(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "missing-mount"},
	))
	handle := uuidHandle(created.Msg.GetWorkspace().GetMetadata().GetUuid())
	_, err := rig.client.RemoveMount(t.Context(), connect.NewRequest(
		&workspacev1.RemoveMountRequest{Handle: handle, Target: "/nope"},
	))
	if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Errorf("code = %s, want NotFound", code)
	}
}

func TestRemoveMountRejectsEmptyTarget(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "empty-target"},
	))
	handle := uuidHandle(created.Msg.GetWorkspace().GetMetadata().GetUuid())
	_, err := rig.client.RemoveMount(t.Context(), connect.NewRequest(
		&workspacev1.RemoveMountRequest{Handle: handle},
	))
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", code)
	}
}

func TestRemoveMountMiddleEntryPreservesOthers(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "three-mounts"},
	))
	handle := uuidHandle(created.Msg.GetWorkspace().GetMetadata().GetUuid())
	for _, target := range []string{"/a", "/b", "/c"} {
		if _, err := rig.client.AddMount(t.Context(), connect.NewRequest(
			&workspacev1.AddMountRequest{
				Handle: handle,
				Mount:  &sandboxv1.Mount{Target: target, Source: testMountSource},
			},
		)); err != nil {
			t.Fatalf("AddMount %s: %v", target, err)
		}
	}
	got, err := rig.client.RemoveMount(t.Context(), connect.NewRequest(
		&workspacev1.RemoveMountRequest{Handle: handle, Target: "/b"},
	))
	if err != nil {
		t.Fatalf("RemoveMount: %v", err)
	}
	mounts := got.Msg.GetWorkspace().GetSpec().GetMounts()
	if len(mounts) != 2 || mounts[0].GetTarget() != "/a" || mounts[1].GetTarget() != "/c" {
		t.Errorf("mounts = %v, want [/a /c]", mountTargets(mounts))
	}
}

func TestRemoveEnvDropsByName(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "rm-env"},
	))
	handle := uuidHandle(created.Msg.GetWorkspace().GetMetadata().GetUuid())
	for _, name := range []string{"A", "B"} {
		if _, err := rig.client.AddEnv(t.Context(), connect.NewRequest(
			&workspacev1.AddEnvRequest{
				Handle: handle,
				Env:    &sandboxv1.EnvVar{Name: name, Value: "v"},
			},
		)); err != nil {
			t.Fatalf("AddEnv: %v", err)
		}
	}
	got, err := rig.client.RemoveEnv(t.Context(), connect.NewRequest(
		&workspacev1.RemoveEnvRequest{Handle: handle, Name: "A"},
	))
	if err != nil {
		t.Fatalf("RemoveEnv: %v", err)
	}
	envs := got.Msg.GetWorkspace().GetSpec().GetEnv()
	if len(envs) != 1 || envs[0].GetName() != "B" {
		t.Errorf("env = %v, want [B]", envs)
	}
}

//nolint:dupl // Structurally parallel to the RemoveMount / RemovePort unknown-key tests by design — each pins one RPC's missing-key error mapping.
func TestRemoveEnvUnknownMapsToNotFound(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "nope-env"},
	))
	handle := uuidHandle(created.Msg.GetWorkspace().GetMetadata().GetUuid())
	_, err := rig.client.RemoveEnv(t.Context(), connect.NewRequest(
		&workspacev1.RemoveEnvRequest{Handle: handle, Name: "MISSING"},
	))
	if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Errorf("code = %s, want NotFound", code)
	}
}

func TestRemoveEnvRejectsEmptyName(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "empty-env"},
	))
	handle := uuidHandle(created.Msg.GetWorkspace().GetMetadata().GetUuid())
	_, err := rig.client.RemoveEnv(t.Context(), connect.NewRequest(
		&workspacev1.RemoveEnvRequest{Handle: handle},
	))
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", code)
	}
}

// TestRemovePortKeyedByPortAndProtocol pins that the (port, proto)
// pair is the key — removing 3000/TCP must leave 3000/UDP intact.
func TestRemovePortKeyedByPortAndProtocol(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "rm-port"},
	))
	handle := uuidHandle(created.Msg.GetWorkspace().GetMetadata().GetUuid())
	if _, err := rig.client.AddPort(t.Context(), connect.NewRequest(
		&workspacev1.AddPortRequest{
			Handle: handle,
			Port: &sandboxv1.PortMapping{
				ContainerPort: 3000, Protocol: sandboxv1.Protocol_PROTOCOL_TCP,
			},
		},
	)); err != nil {
		t.Fatalf("AddPort TCP: %v", err)
	}
	if _, err := rig.client.AddPort(t.Context(), connect.NewRequest(
		&workspacev1.AddPortRequest{
			Handle: handle,
			Port: &sandboxv1.PortMapping{
				ContainerPort: 3000, Protocol: sandboxv1.Protocol_PROTOCOL_UDP,
			},
		},
	)); err != nil {
		t.Fatalf("AddPort UDP: %v", err)
	}
	got, err := rig.client.RemovePort(t.Context(), connect.NewRequest(
		&workspacev1.RemovePortRequest{
			Handle: handle, ContainerPort: 3000,
			Protocol: sandboxv1.Protocol_PROTOCOL_TCP,
		},
	))
	if err != nil {
		t.Fatalf("RemovePort: %v", err)
	}
	ports := got.Msg.GetWorkspace().GetSpec().GetPorts()
	if len(ports) != 1 || ports[0].GetProtocol() != sandboxv1.Protocol_PROTOCOL_UDP {
		t.Errorf("ports = %v, want [3000/UDP]", ports)
	}
}

func TestRemovePortUnknownMapsToNotFound(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "nope-port"},
	))
	handle := uuidHandle(created.Msg.GetWorkspace().GetMetadata().GetUuid())
	_, err := rig.client.RemovePort(t.Context(), connect.NewRequest(
		&workspacev1.RemovePortRequest{
			Handle: handle, ContainerPort: 9999,
			Protocol: sandboxv1.Protocol_PROTOCOL_TCP,
		},
	))
	if code := connect.CodeOf(err); code != connect.CodeNotFound {
		t.Errorf("code = %s, want NotFound", code)
	}
}

//nolint:dupl // Structurally parallel to the RemoveMount / RemoveEnv missing-input tests by design — pins one RPC's required-input error mapping.
func TestRemovePortRejectsUnspecifiedProtocol(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "rm-port-bad"},
	))
	handle := uuidHandle(created.Msg.GetWorkspace().GetMetadata().GetUuid())
	_, err := rig.client.RemovePort(t.Context(), connect.NewRequest(
		&workspacev1.RemovePortRequest{Handle: handle, ContainerPort: 80},
	))
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", code)
	}
}

// uuidHandle is a tiny constructor for the verbose uuid-form of
// WorkspaceHandle. The remove tests do this 8+ times each; the
// helper keeps the test body focused on what's actually under test.
func uuidHandle(uuid string) *workspacev1.WorkspaceHandle {
	return &workspacev1.WorkspaceHandle{
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: uuid},
	}
}

func mountTargets(mounts []*sandboxv1.Mount) []string {
	out := make([]string, len(mounts))
	for i, m := range mounts {
		out[i] = m.GetTarget()
	}
	return out
}

func hasMissing(missing []string, want string) bool {
	for _, m := range missing {
		if m == want {
			return true
		}
	}
	return false
}

// readyWorkspace creates a workspace, populates the three required
// fields, and returns its uuid. Used as the prelude for every B2c
// test that needs a launchable workspace.
func readyWorkspace(t *testing.T, rig *workspaceCatalogRig, name, target string) string {
	t.Helper()
	created, err := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: name},
	))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	handle := uuidHandle(created.Msg.GetWorkspace().GetMetadata().GetUuid())
	if _, err := rig.client.AddMount(t.Context(), connect.NewRequest(
		&workspacev1.AddMountRequest{
			Handle: handle,
			Mount:  &sandboxv1.Mount{Target: testMountTarget, Source: testMountSource},
		},
	)); err != nil {
		t.Fatalf("AddMount: %v", err)
	}
	if _, err := rig.client.SetImage(t.Context(), connect.NewRequest(
		&workspacev1.SetImageRequest{Handle: handle, ImageRef: testImageRef},
	)); err != nil {
		t.Fatalf("SetImage: %v", err)
	}
	if _, err := rig.client.SetTarget(t.Context(), connect.NewRequest(
		&workspacev1.SetTargetRequest{Handle: handle, SandboxFingerprint: target},
	)); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	return created.Msg.GetWorkspace().GetMetadata().GetUuid()
}

// TestValidatePromotesDraftToReady pins the "ready means missing[]
// is empty and target is set" sieve: a workspace with all three
// required fields auto-transitions to READY on Validate.
func TestValidatePromotesDraftToReady(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	target := mustGenerate(t, identity.RoleSandboxHost).Fingerprint().String()
	uuid := readyWorkspace(t, rig, "promote", target)
	resp, err := rig.client.Validate(t.Context(), connect.NewRequest(
		&workspacev1.ValidateRequest{Handle: uuidHandle(uuid)},
	))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !conditionIsTrue(resp.Msg.GetWorkspace().GetStatus(), workspacecatalog.ConditionReady) {
		t.Errorf("Ready not True after spec satisfied; conditions=%v", resp.Msg.GetWorkspace().GetStatus().GetConditions())
	}
}

// TestValidateKeepsIncompleteAsNotReady pins the negative half —
// a workspace still missing a field stays Ready=False after
// Validate.
func TestValidateKeepsIncompleteAsNotReady(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "incomplete"},
	))
	resp, err := rig.client.Validate(t.Context(), connect.NewRequest(
		&workspacev1.ValidateRequest{Handle: uuidHandle(created.Msg.GetWorkspace().GetMetadata().GetUuid())},
	))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if conditionIsTrue(resp.Msg.GetWorkspace().GetStatus(), workspacecatalog.ConditionReady) {
		t.Errorf("Ready should be False on incomplete spec; conditions=%v", resp.Msg.GetWorkspace().GetStatus().GetConditions())
	}
}

// TestValidateDemotesReadyOnDrift pins that a workspace that was
// Ready=True but has since lost a required field flips back to
// Ready=False on the next mutation.
func TestValidateDemotesReadyOnDrift(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	target := mustGenerate(t, identity.RoleSandboxHost).Fingerprint().String()
	uuid := readyWorkspace(t, rig, "drift", target)
	if _, err := rig.client.Validate(t.Context(), connect.NewRequest(
		&workspacev1.ValidateRequest{Handle: uuidHandle(uuid)},
	)); err != nil {
		t.Fatalf("Validate up: %v", err)
	}
	if _, err := rig.client.RemoveMount(t.Context(), connect.NewRequest(
		&workspacev1.RemoveMountRequest{Handle: uuidHandle(uuid), Target: testMountTarget},
	)); err != nil {
		t.Fatalf("RemoveMount: %v", err)
	}
	resp, err := rig.client.Validate(t.Context(), connect.NewRequest(
		&workspacev1.ValidateRequest{Handle: uuidHandle(uuid)},
	))
	if err != nil {
		t.Fatalf("Validate down: %v", err)
	}
	if conditionIsTrue(resp.Msg.GetWorkspace().GetStatus(), workspacecatalog.ConditionReady) {
		t.Errorf("Ready should be False after drift; conditions=%v", resp.Msg.GetWorkspace().GetStatus().GetConditions())
	}
}

// TestLaunchDispatchesComposedSandboxSpec drives the happy path:
// Validate first to reach READY, then Launch composes the spec and
// hands it to the dispatcher. Asserts the spec carries every field
// the workspace declared.
func TestLaunchDispatchesComposedSandboxSpec(t *testing.T) {
	fake := &fakeDispatcher{}
	rig := newWorkspaceCatalogRigWith(t, fake)
	targetID := mustGenerate(t, identity.RoleSandboxHost)
	target := targetID.Fingerprint().String()
	uuid := readyWorkspace(t, rig, "launch-ok", target)
	if _, err := rig.client.AddEnv(t.Context(), connect.NewRequest(
		&workspacev1.AddEnvRequest{
			Handle: uuidHandle(uuid),
			Env:    &sandboxv1.EnvVar{Name: "K", Value: "v"},
		},
	)); err != nil {
		t.Fatalf("AddEnv: %v", err)
	}
	if _, err := rig.client.Validate(t.Context(), connect.NewRequest(
		&workspacev1.ValidateRequest{Handle: uuidHandle(uuid)},
	)); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	resp, err := rig.client.Launch(t.Context(), connect.NewRequest(
		&workspacev1.LaunchRequest{Handle: uuidHandle(uuid)},
	))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	got := resp.Msg.GetWorkspace()
	if !conditionIsTrue(got.GetStatus(), workspacecatalog.ConditionDispatched) {
		t.Errorf("Dispatched not True after Launch; conditions=%v", got.GetStatus().GetConditions())
	}
	if got.GetStatus().GetCurrentSandbox().GetUid() == "" {
		t.Error("current_sandbox.uid not populated")
	}
	call, ok := fake.last()
	if !ok {
		t.Fatal("fake dispatcher saw no calls")
	}
	if call.fp != targetID.Fingerprint() {
		t.Errorf("dispatch target = %s, want %s", call.fp.String(), target)
	}
	if call.sandbox.GetMetadata().GetUid() != got.GetStatus().GetCurrentSandbox().GetUid() {
		t.Errorf("sandbox.uid = %q, want %q", call.sandbox.GetMetadata().GetUid(), got.GetStatus().GetCurrentSandbox().GetUid())
	}
	if call.sandbox.GetSpec().GetImageRef() != testImageRef {
		t.Errorf("spec.image_ref = %q, want %q", call.sandbox.GetSpec().GetImageRef(), testImageRef)
	}
	if call.sandbox.GetSpec().GetWorkspaceUuid() != uuid {
		t.Errorf("spec.workspace_uuid = %q, want %q", call.sandbox.GetSpec().GetWorkspaceUuid(), uuid)
	}
	if len(call.sandbox.GetSpec().GetMounts()) != 1 || call.sandbox.GetSpec().GetMounts()[0].GetTarget() != testMountTarget {
		t.Errorf("spec.mounts = %v, want [%s]", call.sandbox.GetSpec().GetMounts(), testMountTarget)
	}
	if len(call.sandbox.GetSpec().GetEnv()) != 1 || call.sandbox.GetSpec().GetEnv()[0].GetName() != "K" {
		t.Errorf("spec.env = %v, want [K=v]", call.sandbox.GetSpec().GetEnv())
	}
}

// TestLaunchRejectsDraftAndMissing pins the readiness preconditions:
// a workspace still in DRAFT (or one missing fields after drift)
// gets FailedPrecondition before the dispatcher is touched.
func TestLaunchRejectsDraftAndMissing(t *testing.T) {
	fake := &fakeDispatcher{}
	rig := newWorkspaceCatalogRigWith(t, fake)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "draft"},
	))
	_, err := rig.client.Launch(t.Context(), connect.NewRequest(
		&workspacev1.LaunchRequest{Handle: uuidHandle(created.Msg.GetWorkspace().GetMetadata().GetUuid())},
	))
	if code := connect.CodeOf(err); code != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want FailedPrecondition", code)
	}
	if _, ok := fake.last(); ok {
		t.Error("dispatcher was called for an unready workspace")
	}
}

// TestLaunchPropagatesDispatchErrors pins the FailedPrecondition
// (sandbox offline) and Unavailable (backlog) mappings.
func TestLaunchPropagatesDispatchErrors(t *testing.T) {
	cases := []struct {
		name string
		in   error
		want connect.Code
	}{
		{"not connected", ErrSandboxNotConnected, connect.CodeFailedPrecondition},
		{"backlog", ErrDispatchBacklog, connect.CodeUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeDispatcher{nextError: tc.in}
			rig := newWorkspaceCatalogRigWith(t, fake)
			target := mustGenerate(t, identity.RoleSandboxHost).Fingerprint().String()
			uuid := readyWorkspace(t, rig, "dispatch-err", target)
			if _, err := rig.client.Validate(t.Context(), connect.NewRequest(
				&workspacev1.ValidateRequest{Handle: uuidHandle(uuid)},
			)); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			_, err := rig.client.Launch(t.Context(), connect.NewRequest(
				&workspacev1.LaunchRequest{Handle: uuidHandle(uuid)},
			))
			if code := connect.CodeOf(err); code != tc.want {
				t.Errorf("code = %s, want %s", code, tc.want)
			}
		})
	}
}

// TestLaunchRefusesInvalidTargetFingerprint pins the target-parse
// guard: an opaque non-fingerprint stored via SetTarget gets caught
// at Launch rather than confusing the dispatcher.
func TestLaunchRefusesInvalidTargetFingerprint(t *testing.T) {
	fake := &fakeDispatcher{}
	rig := newWorkspaceCatalogRigWith(t, fake)
	// SetTarget accepts any string for now (Phase B doesn't validate),
	// so we plant a bogus value and let Launch's ParseFingerprint
	// catch it. Bypass the alias-uniqueness path by writing through
	// the store directly.
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "bad-target"},
	))
	uuid := created.Msg.GetWorkspace().GetMetadata().GetUuid()
	if _, err := rig.client.AddMount(t.Context(), connect.NewRequest(
		&workspacev1.AddMountRequest{
			Handle: uuidHandle(uuid),
			Mount:  &sandboxv1.Mount{Target: testMountTarget, Source: testMountSource},
		},
	)); err != nil {
		t.Fatalf("AddMount: %v", err)
	}
	if _, err := rig.client.SetImage(t.Context(), connect.NewRequest(
		&workspacev1.SetImageRequest{Handle: uuidHandle(uuid), ImageRef: testImageRef},
	)); err != nil {
		t.Fatalf("SetImage: %v", err)
	}
	if _, err := rig.client.SetTarget(t.Context(), connect.NewRequest(
		&workspacev1.SetTargetRequest{Handle: uuidHandle(uuid), SandboxFingerprint: "not-a-real-fingerprint"},
	)); err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	if _, err := rig.client.Validate(t.Context(), connect.NewRequest(
		&workspacev1.ValidateRequest{Handle: uuidHandle(uuid)},
	)); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	_, err := rig.client.Launch(t.Context(), connect.NewRequest(
		&workspacev1.LaunchRequest{Handle: uuidHandle(uuid)},
	))
	if code := connect.CodeOf(err); code != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want FailedPrecondition", code)
	}
	if _, ok := fake.last(); ok {
		t.Error("dispatcher was called with a bogus target")
	}
}

// TestLaunchWithoutDispatcherIsUnimplemented covers the catalog-
// only test-rig path: a handler with nil dispatcher refuses Launch
// with Unimplemented rather than panicking.
func TestLaunchWithoutDispatcherIsUnimplemented(t *testing.T) {
	h, err := NewWorkspaceCatalogHandler(workspacecatalog.NewStore(nil), nil)
	if err != nil {
		t.Fatalf("NewWorkspaceCatalogHandler: %v", err)
	}
	req := connect.NewRequest(&workspacev1.LaunchRequest{})
	req.Header().Set(peerFingerprintHeader, mustGenerate(t, identity.RoleClient).Fingerprint().String())
	_, err = h.Launch(t.Context(), req)
	if code := connect.CodeOf(err); code != connect.CodeUnimplemented {
		t.Errorf("code = %s, want Unimplemented", code)
	}
}

// TestStopFromRunningTransitionsAndClearsDispatch pins the full
// Launch → Stop cycle: dispatch_id populated by Launch must be
// cleared on Stop.
func TestStopFromRunningTransitionsAndClearsDispatch(t *testing.T) {
	fake := &fakeDispatcher{}
	rig := newWorkspaceCatalogRigWith(t, fake)
	target := mustGenerate(t, identity.RoleSandboxHost).Fingerprint().String()
	uuid := readyWorkspace(t, rig, "stop", target)
	if _, err := rig.client.Validate(t.Context(), connect.NewRequest(
		&workspacev1.ValidateRequest{Handle: uuidHandle(uuid)},
	)); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if _, err := rig.client.Launch(t.Context(), connect.NewRequest(
		&workspacev1.LaunchRequest{Handle: uuidHandle(uuid)},
	)); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	resp, err := rig.client.Stop(t.Context(), connect.NewRequest(
		&workspacev1.StopRequest{Handle: uuidHandle(uuid)},
	))
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	ws := resp.Msg.GetWorkspace()
	if conditionIsTrue(ws.GetStatus(), workspacecatalog.ConditionDispatched) {
		t.Errorf("Dispatched should be False after Stop; conditions=%v", ws.GetStatus().GetConditions())
	}
	if ws.GetStatus().GetCurrentSandbox().GetUid() != "" {
		t.Errorf("current_sandbox.uid = %q, want empty after Stop", ws.GetStatus().GetCurrentSandbox().GetUid())
	}
}

// TestStopWithoutLaunchIsRejected pins that Stop requires
// Dispatched=True; calling it on a workspace that's never been
// launched returns FailedPrecondition.
func TestStopWithoutLaunchIsRejected(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "stop-fresh"},
	))
	_, err := rig.client.Stop(t.Context(), connect.NewRequest(
		&workspacev1.StopRequest{Handle: uuidHandle(created.Msg.GetWorkspace().GetMetadata().GetUuid())},
	))
	if code := connect.CodeOf(err); code != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want FailedPrecondition", code)
	}
}

// TestMintWorkspaceUUIDProducesValidV4 pins the format the rest of
// the handler relies on: 36 chars (32 hex + 4 dashes), v4 nibble at
// position 14, RFC-4122 variant bits at position 19. Two
// consecutive calls must differ — duplicate UUIDs would corrupt the
// catalog's key space.
func TestMintWorkspaceUUIDProducesValidV4(t *testing.T) {
	a := mintWorkspaceUUID()
	b := mintWorkspaceUUID()
	if a == b {
		t.Error("two consecutive mintWorkspaceUUID calls produced the same value")
	}
	if len(a) != 36 {
		t.Fatalf("len(uuid) = %d, want 36 (%q)", len(a), a)
	}
	if a[14] != '4' {
		t.Errorf("version nibble = %q, want '4' (got %q)", a[14], a)
	}
	switch a[19] {
	case '8', '9', 'a', 'b':
	default:
		t.Errorf("variant nibble = %q, want 8/9/a/b (got %q)", a[19], a)
	}
	for _, idx := range []int{8, 13, 18, 23} {
		if a[idx] != '-' {
			t.Errorf("expected '-' at position %d, got %q (%q)", idx, a[idx], a)
		}
	}
}

// TestMapCatalogErrCoversEveryBranch exercises each catalog
// sentinel → Connect code mapping plus the already-wrapped and
// catchall paths. Direct table-driven test rather than going
// through every RPC because some branches (ImmutableField, already
// -wrapped) aren't reachable from the B2a wire surface today.
func TestMapCatalogErrCoversEveryBranch(t *testing.T) {
	already := connect.NewError(connect.CodePermissionDenied, errMapTestSentinel)
	cases := []struct {
		name string
		in   error
		want connect.Code
		nil  bool
	}{
		{"nil", nil, connect.Code(0), true},
		{"not found", workspacecatalog.ErrNotFound, connect.CodeNotFound, false},
		{"alias in use", workspacecatalog.ErrAliasInUse, connect.CodeAlreadyExists, false},
		{"immutable", workspacecatalog.ErrImmutableField, connect.CodeFailedPrecondition, false},
		{"already wrapped", already, connect.CodePermissionDenied, false},
		{"catchall", errMapTestSentinel, connect.CodeInternal, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapCatalogErr(tc.in)
			if tc.nil {
				if got != nil {
					t.Errorf("got %v, want nil", got)
				}
				return
			}
			if code := connect.CodeOf(got); code != tc.want {
				t.Errorf("code = %s, want %s", code, tc.want)
			}
		})
	}
}

// errMapTestSentinel is referenced by the mapCatalogErr test only.
// Declared at file scope so the test doesn't allocate a fresh
// error on every sub-case.
var errMapTestSentinel = workspacecatalogTestErr("synthetic")

type workspacecatalogTestErr string

func (e workspacecatalogTestErr) Error() string { return string(e) }

// TestWorkspaceToProtoCarriesLock pins the lock-projection branch:
// workspaceToProto only emits the Lock message when the catalog
// value is held (owner fingerprint non-zero). The B2a wire surface
// can't set a lock yet (Phase F3), so write through the store
// directly.
func TestWorkspaceToProtoCarriesLock(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	cat := rig.catalog
	owner := mustGenerate(t, identity.RoleClient).Fingerprint()
	ws, err := cat.Create(workspacecatalog.CreateInput{
		UUID:             "fffffff0-1111-4111-8111-aaaaaaaaaaaa",
		Name:             "locked",
		OwnerFingerprint: owner,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	expiry := ws.Metadata.CreatedAt.Add(60 * time.Second)
	_, err = cat.Update(ws.Metadata.UUID, workspacecatalog.MutateStatus, func(w *workspacecatalog.Workspace) error {
		w.Status.Lock = workspacecatalog.Lock{OwnerFingerprint: owner, ExpiresAt: expiry}
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	updated, _ := cat.Get(ws.Metadata.UUID)
	proto := workspaceToProto(updated)
	lock := proto.GetStatus().GetLock()
	if lock == nil {
		t.Fatal("Lock should be populated when owner is set")
	}
	if lock.GetOwnerFingerprint() == "" {
		t.Error("Lock.owner_fingerprint is empty")
	}
	if !lock.GetExpiresAt().IsValid() {
		t.Error("Lock.expires_at should be populated")
	}
}

// uuidHandleFromMsg pulls the workspace UUID off a Create
// response — terser than repeating the chain in every lock test.
func uuidHandleFromMsg(resp *connect.Response[workspacev1.WorkspaceResponse]) *workspacev1.WorkspaceHandle {
	return uuidHandle(resp.Msg.GetWorkspace().GetMetadata().GetUuid())
}

// TestAcquireTakesLockAndPopulatesExpiry pins the happy path:
// caller acquires a workspace with no held lock; the response
// reflects the new owner and an expiry derived from the TTL.
func TestAcquireTakesLockAndPopulatesExpiry(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, err := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "lock-acquire"},
	))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	resp, err := rig.client.Acquire(t.Context(), connect.NewRequest(
		&workspacev1.AcquireRequest{
			Handle: uuidHandleFromMsg(created),
			Ttl:    durationpb.New(10 * time.Second),
		},
	))
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	lock := resp.Msg.GetWorkspace().GetStatus().GetLock()
	if lock == nil {
		t.Fatal("Lock should be populated after Acquire")
	}
	if lock.GetOwnerFingerprint() != rig.callerID.Fingerprint().String() {
		t.Errorf("Lock.owner = %q, want caller's fingerprint", lock.GetOwnerFingerprint())
	}
	if !lock.GetExpiresAt().IsValid() {
		t.Error("Lock.expires_at should be set")
	}
}

// TestAcquireRejectsZeroTTL pins the InvalidArgument guard on TTL.
func TestAcquireRejectsZeroTTL(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "lock-zero-ttl"},
	))
	_, err := rig.client.Acquire(t.Context(), connect.NewRequest(
		&workspacev1.AcquireRequest{Handle: uuidHandleFromMsg(created)},
	))
	if code := connect.CodeOf(err); code != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", code)
	}
}

// TestAcquireRefusesContestedLock plants a lock held by a
// different fingerprint and asserts Acquire returns
// FailedPrecondition.
func TestAcquireRefusesContestedLock(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "lock-contested"},
	))
	uuid := created.Msg.GetWorkspace().GetMetadata().GetUuid()
	other := mustGenerate(t, identity.RoleClient).Fingerprint()
	_, err := rig.catalog.Update(uuid, workspacecatalog.MutateStatus, func(w *workspacecatalog.Workspace) error {
		w.Status.Lock = workspacecatalog.Lock{OwnerFingerprint: other, ExpiresAt: time.Now().Add(time.Hour)}
		return nil
	})
	if err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	_, err = rig.client.Acquire(t.Context(), connect.NewRequest(
		&workspacev1.AcquireRequest{
			Handle: uuidHandle(uuid),
			Ttl:    durationpb.New(10 * time.Second),
		},
	))
	if code := connect.CodeOf(err); code != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want FailedPrecondition", code)
	}
}

// TestAcquireRefreshesOwnLock pins the refresh semantic: the
// same caller can Acquire repeatedly to extend their expiry.
func TestAcquireRefreshesOwnLock(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "lock-refresh"},
	))
	handle := uuidHandleFromMsg(created)
	first, err := rig.client.Acquire(t.Context(), connect.NewRequest(
		&workspacev1.AcquireRequest{Handle: handle, Ttl: durationpb.New(10 * time.Second)},
	))
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	firstExpiry := first.Msg.GetWorkspace().GetStatus().GetLock().GetExpiresAt().AsTime()
	time.Sleep(10 * time.Millisecond)
	second, err := rig.client.Acquire(t.Context(), connect.NewRequest(
		&workspacev1.AcquireRequest{Handle: handle, Ttl: durationpb.New(20 * time.Second)},
	))
	if err != nil {
		t.Fatalf("refresh Acquire: %v", err)
	}
	if !second.Msg.GetWorkspace().GetStatus().GetLock().GetExpiresAt().AsTime().After(firstExpiry) {
		t.Error("refresh did not extend ExpiresAt")
	}
}

// TestMutationRefusedUnderContestedLock pins the gating: a
// mutator (SetImage) called by a caller other than the lock
// holder returns FailedPrecondition without touching state.
func TestMutationRefusedUnderContestedLock(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "lock-mutate"},
	))
	uuid := created.Msg.GetWorkspace().GetMetadata().GetUuid()
	other := mustGenerate(t, identity.RoleClient).Fingerprint()
	_, _ = rig.catalog.Update(uuid, workspacecatalog.MutateStatus, func(w *workspacecatalog.Workspace) error {
		w.Status.Lock = workspacecatalog.Lock{OwnerFingerprint: other, ExpiresAt: time.Now().Add(time.Hour)}
		return nil
	})
	_, err := rig.client.SetImage(t.Context(), connect.NewRequest(
		&workspacev1.SetImageRequest{Handle: uuidHandle(uuid), ImageRef: testImageRef},
	))
	if code := connect.CodeOf(err); code != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want FailedPrecondition", code)
	}
}

// TestReleaseClearsOwnLock pins Release's happy path.
func TestReleaseClearsOwnLock(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "lock-release"},
	))
	handle := uuidHandleFromMsg(created)
	if _, err := rig.client.Acquire(t.Context(), connect.NewRequest(
		&workspacev1.AcquireRequest{Handle: handle, Ttl: durationpb.New(10 * time.Second)},
	)); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	resp, err := rig.client.Release(t.Context(), connect.NewRequest(
		&workspacev1.ReleaseRequest{Handle: handle},
	))
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if resp.Msg.GetWorkspace().GetStatus().GetLock() != nil {
		t.Errorf("Lock = %+v, want nil after Release", resp.Msg.GetWorkspace().GetStatus().GetLock())
	}
}

// TestReleaseIsIdempotentOnUnheld pins that Release against an
// unheld workspace is a no-op (not an error).
func TestReleaseIsIdempotentOnUnheld(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "lock-no-op"},
	))
	if _, err := rig.client.Release(t.Context(), connect.NewRequest(
		&workspacev1.ReleaseRequest{Handle: uuidHandleFromMsg(created)},
	)); err != nil {
		t.Errorf("Release on unheld: %v, want nil", err)
	}
}

// TestReleaseRefusesOtherCallersLock pins the negative half:
// Release against a lock held by someone else returns
// FailedPrecondition.
func TestReleaseRefusesOtherCallersLock(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "lock-foreign"},
	))
	uuid := created.Msg.GetWorkspace().GetMetadata().GetUuid()
	other := mustGenerate(t, identity.RoleClient).Fingerprint()
	_, _ = rig.catalog.Update(uuid, workspacecatalog.MutateStatus, func(w *workspacecatalog.Workspace) error {
		w.Status.Lock = workspacecatalog.Lock{OwnerFingerprint: other, ExpiresAt: time.Now().Add(time.Hour)}
		return nil
	})
	_, err := rig.client.Release(t.Context(), connect.NewRequest(
		&workspacev1.ReleaseRequest{Handle: uuidHandle(uuid)},
	))
	if code := connect.CodeOf(err); code != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want FailedPrecondition", code)
	}
}

// TestExpiredLockAllowsAcquire pins the expiry-as-released rule:
// once a lock's ExpiresAt is in the past, a different caller can
// Acquire without waiting for the old holder to Release.
func TestExpiredLockAllowsAcquire(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "lock-expired"},
	))
	uuid := created.Msg.GetWorkspace().GetMetadata().GetUuid()
	other := mustGenerate(t, identity.RoleClient).Fingerprint()
	_, _ = rig.catalog.Update(uuid, workspacecatalog.MutateStatus, func(w *workspacecatalog.Workspace) error {
		w.Status.Lock = workspacecatalog.Lock{OwnerFingerprint: other, ExpiresAt: time.Now().Add(-time.Hour)}
		return nil
	})
	if _, err := rig.client.Acquire(t.Context(), connect.NewRequest(
		&workspacev1.AcquireRequest{Handle: uuidHandle(uuid), Ttl: durationpb.New(10 * time.Second)},
	)); err != nil {
		t.Errorf("Acquire over expired lock: %v, want nil", err)
	}
}

// TestConditionStatusToProtoCoversAllValues pins the wire mapping
// for every ConditionStatus value, including the
// fall-through (Unspecified / out-of-range → UNSPECIFIED).
func TestConditionStatusToProtoCoversAllValues(t *testing.T) {
	cases := []struct {
		in   workspacecatalog.ConditionStatus
		want sandboxv1.ConditionStatus
	}{
		{workspacecatalog.ConditionStatusTrue, sandboxv1.ConditionStatus_CONDITION_STATUS_TRUE},
		{workspacecatalog.ConditionStatusFalse, sandboxv1.ConditionStatus_CONDITION_STATUS_FALSE},
		{workspacecatalog.ConditionStatusUnknown, sandboxv1.ConditionStatus_CONDITION_STATUS_UNKNOWN},
		{workspacecatalog.ConditionStatusUnspecified, sandboxv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED},
		{workspacecatalog.ConditionStatus(99), sandboxv1.ConditionStatus_CONDITION_STATUS_UNSPECIFIED},
	}
	for _, tc := range cases {
		if got := conditionStatusToProto(tc.in); got != tc.want {
			t.Errorf("conditionStatusToProto(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestGpuRequestToProtoEmitsWhenAnyFieldNonZero pins both branches
// of the early-return: GpuCount + GpuMinVRAM both zero ⇒ nil
// (omits the wire field); any non-zero ⇒ populated.
func TestGpuRequestToProtoEmitsWhenAnyFieldNonZero(t *testing.T) {
	if got := gpuRequestToProto(workspacecatalog.Spec{}); got != nil {
		t.Errorf("zero spec → %+v, want nil", got)
	}
	if got := gpuRequestToProto(workspacecatalog.Spec{GpuCount: 2}); got == nil || got.GetCount() != 2 {
		t.Errorf("count=2 → %+v, want non-nil with Count=2", got)
	}
	if got := gpuRequestToProto(workspacecatalog.Spec{GpuMinVRAM: 1024}); got == nil || got.GetMinVramBytes() != 1024 {
		t.Errorf("min_vram=1024 → %+v, want non-nil with MinVramBytes=1024", got)
	}
}

// TestFingerprintStringOrEmptyZeroBecomesEmpty pins the helper's
// zero-fingerprint branch (covered by the projection but worth a
// direct pin so the contract is explicit).
func TestFingerprintStringOrEmptyZeroBecomesEmpty(t *testing.T) {
	var zero identity.Fingerprint
	if got := fingerprintStringOrEmpty(zero); got != "" {
		t.Errorf("zero fp → %q, want empty", got)
	}
}
