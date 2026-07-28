package sandboxexec

import (
	"bytes"
	"context"
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/sandbox"
	"github.com/richiejp/darkaifactory/internal/workspacesnapshot"
)

const (
	testDiskAllowance   = 1000
	testGrownAllowance  = 2000
	testControlDiskPath = "/state/workspace.qcow2"
	testControlVsock    = "/run/daft/test.vsock"
	testControlAPI      = "/run/daft/test.api.sock"
	testQCOWGrowStep    = "qcow2"
)

type fakeWorkspaceGrowStore struct {
	sequence *[]string
	err      error
}

func (*fakeWorkspaceGrowStore) Prepare(string, uint64) (path string, created bool, err error) {
	return testControlDiskPath, false, nil
}

func (s *fakeWorkspaceGrowStore) Grow(string, uint64) error {
	*s.sequence = append(*s.sequence, testQCOWGrowStep)
	return s.err
}

type fakeWorkspaceVM struct {
	sequence                *[]string
	pauseErr, resumeErr     error
	resizeErr               error
	pauseCalls, resumeCalls int
}

func (v *fakeWorkspaceVM) Pause(context.Context) error {
	v.pauseCalls++
	*v.sequence = append(*v.sequence, "pause")
	return v.pauseErr
}

func (v *fakeWorkspaceVM) Resume(context.Context) error {
	v.resumeCalls++
	*v.sequence = append(*v.sequence, "resume")
	return v.resumeErr
}

func (v *fakeWorkspaceVM) ResizeDisk(context.Context, string, uint64) error {
	*v.sequence = append(*v.sequence, "vmm")
	return v.resizeErr
}

func newWorkspaceControlExecutor(store WorkspaceDiskPreparer, vm workspaceVMController) *Executor {
	return &Executor{
		stdout:                    &bytes.Buffer{},
		workspaceDisks:            store,
		workspaceDiskPollInterval: time.Hour,
		newWorkspaceVMController:  func(string) (workspaceVMController, error) { return vm, nil },
		dialWorkspaceGuest:        func(context.Context, string, uint32) (net.Conn, error) { return nil, nil },
		workspaceDiskControls:     map[string]*workspaceDiskRuntime{},
	}
}

func TestPollWorkspaceDiskFreezesAtNinetyPercent(t *testing.T) {
	sequence := []string{}
	vm := &fakeWorkspaceVM{sequence: &sequence}
	e := newWorkspaceControlExecutor(nil, vm)
	runtime := &workspaceDiskRuntime{workspaceID: testWSUUID, controller: vm, allowance: testDiskAllowance}
	e.getWorkspaceUsage = func(context.Context, func(context.Context) (net.Conn, error)) (workspacesnapshot.Usage, error) {
		return workspacesnapshot.Usage{Total: testDiskAllowance, Used: 899}, nil
	}
	if err := e.pollWorkspaceDisk(t.Context(), runtime); err != nil {
		t.Fatal(err)
	}
	if runtime.frozen || vm.pauseCalls != 0 {
		t.Fatal("disk froze below threshold")
	}
	e.getWorkspaceUsage = func(context.Context, func(context.Context) (net.Conn, error)) (workspacesnapshot.Usage, error) {
		return workspacesnapshot.Usage{Total: testDiskAllowance, Used: 900}, nil
	}
	if err := e.pollWorkspaceDisk(t.Context(), runtime); err != nil {
		t.Fatal(err)
	}
	if !runtime.frozen || vm.pauseCalls != 1 {
		t.Fatalf("frozen=%v pause calls=%d", runtime.frozen, vm.pauseCalls)
	}
	if err := e.pollWorkspaceDisk(t.Context(), runtime); err != nil || vm.pauseCalls != 1 {
		t.Fatalf("repeat poll error=%v pause calls=%d", err, vm.pauseCalls)
	}
}

func TestWorkspaceDiskResizeOrdering(t *testing.T) {
	sequence := []string{}
	store := &fakeWorkspaceGrowStore{sequence: &sequence}
	vm := &fakeWorkspaceVM{sequence: &sequence}
	e := newWorkspaceControlExecutor(store, vm)
	e.growWorkspaceGuest = func(context.Context, func(context.Context) (net.Conn, error)) error {
		sequence = append(sequence, "xfs")
		return nil
	}
	runtime := &workspaceDiskRuntime{
		workspaceID: testWSUUID, sandboxUID: testSandboxID, hostSocket: testControlVsock,
		controller: vm, allowance: testDiskAllowance,
	}
	e.workspaceDiskControls[testWSUUID] = runtime
	if err := e.ResizeWorkspaceDisk(t.Context(), testWSUUID, testGrownAllowance); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sequence, []string{testQCOWGrowStep, "vmm", "xfs"}) {
		t.Fatalf("resize sequence = %v", sequence)
	}
	status, err := e.WorkspaceDiskStatus(testWSUUID)
	if err != nil || status.Allowance != testGrownAllowance || status.Frozen {
		t.Fatalf("status = %#v, %v", status, err)
	}
}

func TestFrozenWorkspaceResizeRequiresExplicitUnfreeze(t *testing.T) {
	sequence := []string{}
	store := &fakeWorkspaceGrowStore{sequence: &sequence}
	vm := &fakeWorkspaceVM{sequence: &sequence}
	e := newWorkspaceControlExecutor(store, vm)
	e.growWorkspaceGuest = func(context.Context, func(context.Context) (net.Conn, error)) error {
		sequence = append(sequence, "xfs")
		return nil
	}
	runtime := &workspaceDiskRuntime{
		workspaceID: testWSUUID, controller: vm, allowance: testDiskAllowance,
		usage: workspacesnapshot.Usage{Used: 950}, frozen: true,
	}
	e.workspaceDiskControls[testWSUUID] = runtime
	if err := e.UnfreezeWorkspaceDisk(t.Context(), testWSUUID); !errors.Is(err, ErrWorkspaceDiskStillFull) {
		t.Fatalf("premature unfreeze error = %v", err)
	}
	if err := e.ResizeWorkspaceDisk(t.Context(), testWSUUID, testGrownAllowance); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sequence, []string{testQCOWGrowStep, "vmm", "resume", "xfs", "pause"}) {
		t.Fatalf("frozen resize sequence = %v", sequence)
	}
	if !runtime.frozen {
		t.Fatal("resize implicitly unfroze workspace")
	}
	if err := e.UnfreezeWorkspaceDisk(t.Context(), testWSUUID); err != nil {
		t.Fatal(err)
	}
	if runtime.frozen || vm.resumeCalls != 2 {
		t.Fatalf("frozen=%v resume calls=%d", runtime.frozen, vm.resumeCalls)
	}
}

func TestWorkspaceDiskControlRegistrationLifecycle(t *testing.T) {
	sequence := []string{}
	vm := &fakeWorkspaceVM{sequence: &sequence}
	e := newWorkspaceControlExecutor(nil, vm)
	cancelled, cancel := context.WithCancel(t.Context())
	unregister := e.registerWorkspaceDiskControl(cancelled, sandbox.Config{
		APISocket:     testControlAPI,
		Vsock:         &sandbox.VsockConfig{HostSocket: testControlVsock},
		WorkspaceDisk: &sandbox.DiskConfig{Path: testControlDiskPath, ID: workspaceDiskID},
	}, testWSUUID, testSandboxID, testDiskAllowance)
	status, err := e.WorkspaceDiskStatus(testWSUUID)
	if err != nil || status.SandboxUID != testSandboxID || status.Allowance != testDiskAllowance {
		t.Fatalf("registered status = %#v, %v", status, err)
	}
	cancel()
	unregister()
	if _, err := e.WorkspaceDiskStatus(testWSUUID); !errors.Is(err, ErrWorkspaceDiskNotRunning) {
		t.Fatalf("unregistered status error = %v", err)
	}
	if cleanup := e.registerWorkspaceDiskControl(t.Context(), sandbox.Config{}, testWSUUID, testSandboxID, testDiskAllowance); cleanup == nil {
		t.Fatal("disabled registration returned nil cleanup")
	}
}

func TestWorkspaceDiskControlFailures(t *testing.T) {
	sequence := []string{}
	vm := &fakeWorkspaceVM{sequence: &sequence}
	e := newWorkspaceControlExecutor(&fakeWorkspaceGrowStore{sequence: &sequence}, vm)
	runtime := &workspaceDiskRuntime{workspaceID: testWSUUID, controller: vm, allowance: testDiskAllowance}
	e.workspaceDiskControls[testWSUUID] = runtime
	e.getWorkspaceUsage = func(context.Context, func(context.Context) (net.Conn, error)) (workspacesnapshot.Usage, error) {
		return workspacesnapshot.Usage{}, errors.New("usage failed")
	}
	if err := e.pollWorkspaceDisk(t.Context(), runtime); err == nil {
		t.Fatal("usage failure ignored")
	}
	vm.pauseErr = errors.New("pause failed")
	e.getWorkspaceUsage = func(context.Context, func(context.Context) (net.Conn, error)) (workspacesnapshot.Usage, error) {
		return workspacesnapshot.Usage{Used: testDiskAllowance}, nil
	}
	if err := e.pollWorkspaceDisk(t.Context(), runtime); err == nil || runtime.frozen {
		t.Fatalf("pause failure = %v frozen=%v", err, runtime.frozen)
	}
	if err := e.ResizeWorkspaceDisk(t.Context(), "missing", testGrownAllowance); !errors.Is(err, ErrWorkspaceDiskNotRunning) {
		t.Fatalf("missing resize error = %v", err)
	}
	if err := e.ResizeWorkspaceDisk(t.Context(), testWSUUID, testDiskAllowance); err == nil {
		t.Fatal("non-growth resize accepted")
	}
	if err := e.UnfreezeWorkspaceDisk(t.Context(), testWSUUID); !errors.Is(err, ErrWorkspaceDiskNotFrozen) {
		t.Fatalf("unfrozen error = %v", err)
	}
	e.workspaceDisks = struct{ WorkspaceDiskPreparer }{}
	if err := e.ResizeWorkspaceDisk(t.Context(), testWSUUID, testGrownAllowance); !errors.Is(err, ErrWorkspaceDiskNoGrower) {
		t.Fatalf("missing grower error = %v", err)
	}
}

func TestWorkspaceDiskResizeFailures(t *testing.T) {
	want := errors.New("injected failure")
	for _, tc := range []struct {
		name   string
		frozen bool
		mutate func(*fakeWorkspaceGrowStore, *fakeWorkspaceVM, *Executor)
	}{
		{name: "qcow2", mutate: func(store *fakeWorkspaceGrowStore, _ *fakeWorkspaceVM, _ *Executor) { store.err = want }},
		{name: "VMM", mutate: func(_ *fakeWorkspaceGrowStore, vm *fakeWorkspaceVM, _ *Executor) { vm.resizeErr = want }},
		{name: "resume", frozen: true, mutate: func(_ *fakeWorkspaceGrowStore, vm *fakeWorkspaceVM, _ *Executor) { vm.resumeErr = want }},
		{name: "XFS", mutate: func(_ *fakeWorkspaceGrowStore, _ *fakeWorkspaceVM, e *Executor) {
			e.growWorkspaceGuest = func(context.Context, func(context.Context) (net.Conn, error)) error { return want }
		}},
		{name: "re-freeze", frozen: true, mutate: func(_ *fakeWorkspaceGrowStore, vm *fakeWorkspaceVM, _ *Executor) { vm.pauseErr = want }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sequence := []string{}
			store := &fakeWorkspaceGrowStore{sequence: &sequence}
			vm := &fakeWorkspaceVM{sequence: &sequence}
			e := newWorkspaceControlExecutor(store, vm)
			e.growWorkspaceGuest = func(context.Context, func(context.Context) (net.Conn, error)) error { return nil }
			e.workspaceDiskControls[testWSUUID] = &workspaceDiskRuntime{
				workspaceID: testWSUUID, controller: vm, allowance: testDiskAllowance, frozen: tc.frozen,
			}
			tc.mutate(store, vm, e)
			if err := e.ResizeWorkspaceDisk(t.Context(), testWSUUID, testGrownAllowance); err == nil {
				t.Fatal("resize failure ignored")
			}
		})
	}
}

func TestWorkspaceDiskControllerSetupFailuresAndDialer(t *testing.T) {
	e := newWorkspaceControlExecutor(nil, nil)
	e.newWorkspaceVMController = func(string) (workspaceVMController, error) { return nil, errors.New("factory failed") }
	cleanup := e.registerWorkspaceDiskControl(t.Context(), sandbox.Config{
		APISocket: testControlAPI, Vsock: &sandbox.VsockConfig{HostSocket: testControlVsock},
		WorkspaceDisk: &sandbox.DiskConfig{Path: testControlDiskPath, ID: workspaceDiskID},
	}, testWSUUID, testSandboxID, testDiskAllowance)
	cleanup()
	if _, err := e.WorkspaceDiskStatus(testWSUUID); !errors.Is(err, ErrWorkspaceDiskNotRunning) {
		t.Fatalf("failed controller was registered: %v", err)
	}

	called := false
	e.dialWorkspaceGuest = func(_ context.Context, socket string, port uint32) (net.Conn, error) {
		called = socket == testControlVsock && port == workspacesnapshot.DefaultVsockPort
		return nil, errors.New("dial stopped")
	}
	_, _ = e.workspaceDialer(testControlVsock)(t.Context())
	if !called {
		t.Fatal("workspace dialer used the wrong route")
	}
	if _, err := newWorkspaceVMController("relative"); err == nil {
		t.Fatal("relative API socket accepted")
	}
}

func TestWorkspaceDiskUnfreezeResumeFailure(t *testing.T) {
	sequence := []string{}
	vm := &fakeWorkspaceVM{sequence: &sequence, resumeErr: errors.New("resume failed")}
	e := newWorkspaceControlExecutor(nil, vm)
	e.workspaceDiskControls[testWSUUID] = &workspaceDiskRuntime{
		workspaceID: testWSUUID, controller: vm, allowance: testDiskAllowance, frozen: true, resizeDone: true,
	}
	if err := e.UnfreezeWorkspaceDisk(t.Context(), testWSUUID); err == nil {
		t.Fatal("resume failure ignored")
	}
}
