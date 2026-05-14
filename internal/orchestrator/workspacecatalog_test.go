package orchestrator

import (
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	sandboxctrlv1 "github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

const (
	// testImageRef is the image ref used by every test that exercises
	// SetImage / image-missing paths.
	testImageRef = "alpine:3"
	// testMountTarget is the canonical in-VM mount target used
	// across the Add/Remove mount tests.
	testMountTarget = "/workspace"
)

// TestServerExposesWorkspaceCatalog mirrors
// TestServerExposesSandboxRegistry — a configured catalog flows
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
	if _, err := NewWorkspaceCatalogHandler(nil); err == nil {
		t.Error("expected error for nil store")
	}
}

// TestWorkspaceCatalogHandlerRejectsMissingPeerIdentity covers
// every RPC's auth check.
func TestWorkspaceCatalogHandlerRejectsMissingPeerIdentity(t *testing.T) {
	h, err := NewWorkspaceCatalogHandler(workspacecatalog.NewStore(nil))
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
					ContainerPort: 80, Protocol: sandboxctrlv1.Protocol_PROTOCOL_TCP,
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
}

func newWorkspaceCatalogRig(t *testing.T) *workspaceCatalogRig {
	t.Helper()
	cat := workspacecatalog.NewStore(nil)
	env := newOrchTestEnv(t, identity.RoleClient, Config{WorkspaceCatalog: cat})
	client := workspacev1connect.NewWorkspaceCatalogServiceClient(env.httpc, env.baseURL)
	return &workspaceCatalogRig{
		server:   env.server,
		catalog:  cat,
		callerID: env.callerID,
		client:   client,
	}
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
	if ws.GetUuid() == "" {
		t.Error("Workspace.uuid is empty")
	}
	if ws.GetName() != "myapp" {
		t.Errorf("Workspace.name = %q, want %q", ws.GetName(), "myapp")
	}
	if ws.GetOwnerFingerprint() != rig.callerID.Fingerprint().String() {
		t.Errorf("Workspace.owner_fingerprint = %q, want caller's", ws.GetOwnerFingerprint())
	}
	if ws.GetStatus() != workspacev1.WorkspaceStatus_WORKSPACE_STATUS_DRAFT {
		t.Errorf("Workspace.status = %s, want DRAFT", ws.GetStatus())
	}
	if len(ws.GetMissing()) == 0 {
		t.Error("Workspace.missing is empty; expected at least mount/image/target")
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
	uuid := created.Msg.GetWorkspace().GetUuid()

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
	if byUUID.Msg.GetWorkspace().GetUuid() != byName.Msg.GetWorkspace().GetUuid() {
		t.Errorf("uuid mismatch: byUUID=%q byName=%q",
			byUUID.Msg.GetWorkspace().GetUuid(),
			byName.Msg.GetWorkspace().GetUuid())
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
	if mine.Msg.GetWorkspaces()[0].GetName() != "mine" {
		t.Errorf("List mine[0].name = %q, want mine", mine.Msg.GetWorkspaces()[0].GetName())
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
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: created.Msg.GetWorkspace().GetUuid()},
	}

	afterMount, err := rig.client.AddMount(t.Context(), connect.NewRequest(
		&workspacev1.AddMountRequest{
			Handle: handle,
			Mount: &sandboxctrlv1.Mount{
				Target: testMountTarget,
				Source: "user-host-cwd",
			},
		},
	))
	if err != nil {
		t.Fatalf("AddMount: %v", err)
	}
	if hasMissing(afterMount.Msg.GetWorkspace().GetMissing(), "mount") {
		t.Error("missing still contains 'mount' after AddMount")
	}

	afterImg, err := rig.client.SetImage(t.Context(), connect.NewRequest(
		&workspacev1.SetImageRequest{Handle: handle, ImageRef: testImageRef},
	))
	if err != nil {
		t.Fatalf("SetImage: %v", err)
	}
	if hasMissing(afterImg.Msg.GetWorkspace().GetMissing(), "image") {
		t.Error("missing still contains 'image' after SetImage")
	}

	final, err := rig.client.SetTarget(t.Context(), connect.NewRequest(
		&workspacev1.SetTargetRequest{Handle: handle, SandboxFingerprint: "sha256:abc"},
	))
	if err != nil {
		t.Fatalf("SetTarget: %v", err)
	}
	if got := len(final.Msg.GetWorkspace().GetMissing()); got != 0 {
		t.Errorf("missing = %v, want empty", final.Msg.GetWorkspace().GetMissing())
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
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: created.Msg.GetWorkspace().GetUuid()},
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
					Mount:  &sandboxctrlv1.Mount{Target: tc.target},
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
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: created.Msg.GetWorkspace().GetUuid()},
	}
	add := &workspacev1.AddMountRequest{
		Handle: handle,
		Mount:  &sandboxctrlv1.Mount{Target: testMountTarget, Source: "a"},
	}
	if _, err := rig.client.AddMount(t.Context(), connect.NewRequest(add)); err != nil {
		t.Fatalf("first AddMount: %v", err)
	}
	_, err = rig.client.AddMount(t.Context(), connect.NewRequest(add))
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
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: created.Msg.GetWorkspace().GetUuid()},
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
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: created.Msg.GetWorkspace().GetUuid()},
	}
	if _, err := rig.client.AddEnv(t.Context(), connect.NewRequest(
		&workspacev1.AddEnvRequest{Handle: handle, Env: &sandboxctrlv1.EnvVar{}},
	)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("empty name: code = %s, want InvalidArgument", connect.CodeOf(err))
	}
	if _, err := rig.client.AddEnv(t.Context(), connect.NewRequest(
		&workspacev1.AddEnvRequest{Handle: handle, Env: &sandboxctrlv1.EnvVar{Name: "K", Value: "v"}},
	)); err != nil {
		t.Fatalf("first AddEnv: %v", err)
	}
	if _, err := rig.client.AddEnv(t.Context(), connect.NewRequest(
		&workspacev1.AddEnvRequest{Handle: handle, Env: &sandboxctrlv1.EnvVar{Name: "K", Value: "v2"}},
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
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: created.Msg.GetWorkspace().GetUuid()},
	}
	cases := []struct {
		name string
		port *sandboxctrlv1.PortMapping
		want connect.Code
	}{
		{"nil port", nil, connect.CodeInvalidArgument},
		{
			"unspecified proto",
			&sandboxctrlv1.PortMapping{ContainerPort: 3000},
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
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: created.Msg.GetWorkspace().GetUuid()},
	}
	if _, err := rig.client.AddPort(t.Context(), connect.NewRequest(
		&workspacev1.AddPortRequest{
			Handle: handle,
			Port:   &sandboxctrlv1.PortMapping{ContainerPort: 3000, Protocol: sandboxctrlv1.Protocol_PROTOCOL_TCP},
		},
	)); err != nil {
		t.Fatalf("first AddPort: %v", err)
	}
	if _, err := rig.client.AddPort(t.Context(), connect.NewRequest(
		&workspacev1.AddPortRequest{
			Handle: handle,
			Port:   &sandboxctrlv1.PortMapping{ContainerPort: 3000, Protocol: sandboxctrlv1.Protocol_PROTOCOL_UDP},
		},
	)); err != nil {
		t.Errorf("same-port-different-proto: %v, want nil", err)
	}
	if _, err := rig.client.AddPort(t.Context(), connect.NewRequest(
		&workspacev1.AddPortRequest{
			Handle: handle,
			Port:   &sandboxctrlv1.PortMapping{ContainerPort: 3000, Protocol: sandboxctrlv1.Protocol_PROTOCOL_TCP},
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
		Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: created.Msg.GetWorkspace().GetUuid()},
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
	if cleared.Msg.GetWorkspace().GetImageRef() != "" {
		t.Errorf("image_ref = %q, want empty after clear", cleared.Msg.GetWorkspace().GetImageRef())
	}
	if !hasMissing(cleared.Msg.GetWorkspace().GetMissing(), "image") {
		t.Error("missing should re-include 'image' after clear")
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
	handle := uuidHandle(created.Msg.GetWorkspace().GetUuid())
	if _, err := rig.client.AddMount(t.Context(), connect.NewRequest(
		&workspacev1.AddMountRequest{
			Handle: handle,
			Mount:  &sandboxctrlv1.Mount{Target: testMountTarget, Source: "src"},
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
	if len(got.Msg.GetWorkspace().GetMounts()) != 0 {
		t.Errorf("Mounts = %d, want 0", len(got.Msg.GetWorkspace().GetMounts()))
	}
	if !hasMissing(got.Msg.GetWorkspace().GetMissing(), "mount") {
		t.Error("missing should include 'mount' after RemoveMount drops the last")
	}
}

//nolint:dupl // Structurally parallel to the RemoveEnv / RemovePort unknown-key tests by design — each pins one RPC's missing-key error mapping.
func TestRemoveMountUnknownTargetMapsToNotFound(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "missing-mount"},
	))
	handle := uuidHandle(created.Msg.GetWorkspace().GetUuid())
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
	handle := uuidHandle(created.Msg.GetWorkspace().GetUuid())
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
	handle := uuidHandle(created.Msg.GetWorkspace().GetUuid())
	for _, target := range []string{"/a", "/b", "/c"} {
		if _, err := rig.client.AddMount(t.Context(), connect.NewRequest(
			&workspacev1.AddMountRequest{
				Handle: handle,
				Mount:  &sandboxctrlv1.Mount{Target: target, Source: "src"},
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
	mounts := got.Msg.GetWorkspace().GetMounts()
	if len(mounts) != 2 || mounts[0].GetTarget() != "/a" || mounts[1].GetTarget() != "/c" {
		t.Errorf("mounts = %v, want [/a /c]", mountTargets(mounts))
	}
}

func TestRemoveEnvDropsByName(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "rm-env"},
	))
	handle := uuidHandle(created.Msg.GetWorkspace().GetUuid())
	for _, name := range []string{"A", "B"} {
		if _, err := rig.client.AddEnv(t.Context(), connect.NewRequest(
			&workspacev1.AddEnvRequest{
				Handle: handle,
				Env:    &sandboxctrlv1.EnvVar{Name: name, Value: "v"},
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
	envs := got.Msg.GetWorkspace().GetEnv()
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
	handle := uuidHandle(created.Msg.GetWorkspace().GetUuid())
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
	handle := uuidHandle(created.Msg.GetWorkspace().GetUuid())
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
	handle := uuidHandle(created.Msg.GetWorkspace().GetUuid())
	if _, err := rig.client.AddPort(t.Context(), connect.NewRequest(
		&workspacev1.AddPortRequest{
			Handle: handle,
			Port: &sandboxctrlv1.PortMapping{
				ContainerPort: 3000, Protocol: sandboxctrlv1.Protocol_PROTOCOL_TCP,
			},
		},
	)); err != nil {
		t.Fatalf("AddPort TCP: %v", err)
	}
	if _, err := rig.client.AddPort(t.Context(), connect.NewRequest(
		&workspacev1.AddPortRequest{
			Handle: handle,
			Port: &sandboxctrlv1.PortMapping{
				ContainerPort: 3000, Protocol: sandboxctrlv1.Protocol_PROTOCOL_UDP,
			},
		},
	)); err != nil {
		t.Fatalf("AddPort UDP: %v", err)
	}
	got, err := rig.client.RemovePort(t.Context(), connect.NewRequest(
		&workspacev1.RemovePortRequest{
			Handle: handle, ContainerPort: 3000,
			Protocol: sandboxctrlv1.Protocol_PROTOCOL_TCP,
		},
	))
	if err != nil {
		t.Fatalf("RemovePort: %v", err)
	}
	ports := got.Msg.GetWorkspace().GetPorts()
	if len(ports) != 1 || ports[0].GetProtocol() != sandboxctrlv1.Protocol_PROTOCOL_UDP {
		t.Errorf("ports = %v, want [3000/UDP]", ports)
	}
}

func TestRemovePortUnknownMapsToNotFound(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	created, _ := rig.client.Create(t.Context(), connect.NewRequest(
		&workspacev1.CreateWorkspaceRequest{Name: "nope-port"},
	))
	handle := uuidHandle(created.Msg.GetWorkspace().GetUuid())
	_, err := rig.client.RemovePort(t.Context(), connect.NewRequest(
		&workspacev1.RemovePortRequest{
			Handle: handle, ContainerPort: 9999,
			Protocol: sandboxctrlv1.Protocol_PROTOCOL_TCP,
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
	handle := uuidHandle(created.Msg.GetWorkspace().GetUuid())
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

func mountTargets(mounts []*sandboxctrlv1.Mount) []string {
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

// TestStatusToProtoCoversAllValues exercises every catalog Status →
// proto enum mapping. The wire surface only hits a subset of these
// today (Create produces DRAFT, mutators don't transition), so a
// direct test keeps the projection honest for B4 when the state
// machine starts firing transitions in earnest.
func TestStatusToProtoCoversAllValues(t *testing.T) {
	cases := []struct {
		in   workspacecatalog.Status
		want workspacev1.WorkspaceStatus
	}{
		{workspacecatalog.StatusUnspecified, workspacev1.WorkspaceStatus_WORKSPACE_STATUS_UNSPECIFIED},
		{workspacecatalog.StatusDraft, workspacev1.WorkspaceStatus_WORKSPACE_STATUS_DRAFT},
		{workspacecatalog.StatusReady, workspacev1.WorkspaceStatus_WORKSPACE_STATUS_READY},
		{workspacecatalog.StatusRunning, workspacev1.WorkspaceStatus_WORKSPACE_STATUS_RUNNING},
		{workspacecatalog.StatusFrozen, workspacev1.WorkspaceStatus_WORKSPACE_STATUS_FROZEN},
		{workspacecatalog.StatusStopped, workspacev1.WorkspaceStatus_WORKSPACE_STATUS_STOPPED},
		{workspacecatalog.StatusArchived, workspacev1.WorkspaceStatus_WORKSPACE_STATUS_ARCHIVED},
		// Out-of-range value falls through to UNSPECIFIED.
		{workspacecatalog.Status(99), workspacev1.WorkspaceStatus_WORKSPACE_STATUS_UNSPECIFIED},
	}
	for _, tc := range cases {
		if got := statusToProto(tc.in); got != tc.want {
			t.Errorf("statusToProto(%s) = %s, want %s", tc.in, got, tc.want)
		}
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

// TestWorkspaceToProtoCarriesLockExpiry pins the lock-expiry
// projection branch — workspaceToProto only stamps LockExpiresAt
// when the catalog value is non-zero, so we have to construct a
// catalog Workspace with a lock manually since the B2a wire
// surface can't set one yet.
func TestWorkspaceToProtoCarriesLockExpiry(t *testing.T) {
	rig := newWorkspaceCatalogRig(t)
	cat := rig.catalog
	ws, err := cat.Create(workspacecatalog.CreateInput{
		UUID: "fffffff0-1111-4111-8111-aaaaaaaaaaaa",
		Name: "locked",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	now := ws.CreatedAt.Add(60_000_000_000) // +60s
	_, err = cat.Update(ws.UUID, func(w *workspacecatalog.Workspace) error {
		w.LockExpiresAt = now
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	updated, _ := cat.Get(ws.UUID)
	proto := workspaceToProto(updated)
	if proto.GetLockExpiresAt() == nil || !proto.GetLockExpiresAt().IsValid() {
		t.Error("LockExpiresAt should be populated")
	}
}
