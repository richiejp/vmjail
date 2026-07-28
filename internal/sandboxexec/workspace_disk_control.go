package sandboxexec

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/richiejp/darkaifactory/internal/cloudhypervisor"
	"github.com/richiejp/darkaifactory/internal/sandbox"
	"github.com/richiejp/darkaifactory/internal/workspacesnapshot"
)

const (
	defaultWorkspaceDiskPollInterval = 10 * time.Second
	workspaceFreezeNumerator         = 9
	workspaceFreezeDenominator       = 10
)

var (
	ErrWorkspaceDiskNotRunning = errors.New("sandboxexec: workspace disk is not running")
	ErrWorkspaceDiskNotFrozen  = errors.New("sandboxexec: workspace disk is not frozen")
	ErrWorkspaceDiskStillFull  = errors.New("sandboxexec: workspace disk remains above the freeze threshold")
	ErrWorkspaceDiskNoGrower   = errors.New("sandboxexec: workspace disk store cannot grow images")
)

type workspaceDiskGrower interface {
	Grow(workspaceID string, virtualSize uint64) error
}

type workspaceVMController interface {
	Pause(context.Context) error
	Resume(context.Context) error
	ResizeDisk(context.Context, string, uint64) error
}

// WorkspaceDiskStatus is the Node-local state used by heartbeats and public
// storage status projection. Values are a coherent snapshot.
type WorkspaceDiskStatus struct {
	WorkspaceID string
	SandboxUID  string
	Usage       workspacesnapshot.Usage
	Allowance   uint64
	Frozen      bool
}

type workspaceDiskRuntime struct {
	workspaceID string
	sandboxUID  string
	hostSocket  string
	controller  workspaceVMController

	mu         sync.Mutex
	usage      workspacesnapshot.Usage
	allowance  uint64
	frozen     bool
	resizeDone bool
}

func (e *Executor) registerWorkspaceDiskControl(ctx context.Context, cfg sandbox.Config, workspaceID, sandboxUID string, allowance uint64) func() {
	if cfg.WorkspaceDisk == nil || cfg.Vsock == nil || cfg.APISocket == "" || workspaceID == "" {
		return func() {}
	}
	controller, err := e.newWorkspaceVMController(cfg.APISocket)
	if err != nil {
		_, _ = fmt.Fprintf(e.stdout, "workspace-disk: controller unavailable for %s: %v\n", workspaceID, err)
		return func() {}
	}
	runtime := &workspaceDiskRuntime{
		workspaceID: workspaceID, sandboxUID: sandboxUID, hostSocket: cfg.Vsock.HostSocket,
		controller: controller, allowance: allowance,
	}
	e.diskMu.Lock()
	e.workspaceDiskControls[workspaceID] = runtime
	e.diskMu.Unlock()
	go e.monitorWorkspaceDisk(ctx, runtime)
	return func() {
		e.diskMu.Lock()
		if e.workspaceDiskControls[workspaceID] == runtime {
			delete(e.workspaceDiskControls, workspaceID)
		}
		e.diskMu.Unlock()
	}
}

func (e *Executor) monitorWorkspaceDisk(ctx context.Context, runtime *workspaceDiskRuntime) {
	ticker := time.NewTicker(e.workspaceDiskPollInterval)
	defer ticker.Stop()
	//daft:lint:disable=forloopbound reason="bounded by the per-Sandbox context cancellation signal"
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.pollWorkspaceDisk(ctx, runtime); err != nil && ctx.Err() == nil {
				_, _ = fmt.Fprintf(e.stdout, "workspace-disk: poll %s: %v\n", runtime.workspaceID, err)
			}
		}
	}
}

func (e *Executor) pollWorkspaceDisk(ctx context.Context, runtime *workspaceDiskRuntime) error {
	usage, err := e.getWorkspaceUsage(ctx, e.workspaceDialer(runtime.hostSocket))
	if err != nil {
		return err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.usage = usage
	if runtime.frozen || !atWorkspaceFreezeThreshold(usage.Used, runtime.allowance) {
		return nil
	}
	if err := runtime.controller.Pause(ctx); err != nil {
		return fmt.Errorf("pause at storage threshold: %w", err)
	}
	runtime.frozen = true
	return nil
}

func atWorkspaceFreezeThreshold(used, allowance uint64) bool {
	return allowance > 0 && used >= allowance/workspaceFreezeDenominator*workspaceFreezeNumerator
}

func (e *Executor) workspaceDialer(hostSocket string) func(context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		return e.dialWorkspaceGuest(ctx, hostSocket, workspacesnapshot.DefaultVsockPort)
	}
}

// WorkspaceDiskStatus returns the running disk state for workspaceID.
func (e *Executor) WorkspaceDiskStatus(workspaceID string) (WorkspaceDiskStatus, error) {
	runtime, err := e.workspaceDiskRuntime(workspaceID)
	if err != nil {
		return WorkspaceDiskStatus{}, err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return WorkspaceDiskStatus{
		WorkspaceID: runtime.workspaceID, SandboxUID: runtime.sandboxUID,
		Usage: runtime.usage, Allowance: runtime.allowance, Frozen: runtime.frozen,
	}, nil
}

// WorkspaceDiskStatuses returns a stable coherent snapshot of every running
// retained disk for heartbeat publication.
func (e *Executor) WorkspaceDiskStatuses() []WorkspaceDiskStatus {
	e.diskMu.RLock()
	runtimes := make([]*workspaceDiskRuntime, 0, len(e.workspaceDiskControls))
	for _, runtime := range e.workspaceDiskControls {
		runtimes = append(runtimes, runtime)
	}
	e.diskMu.RUnlock()
	statuses := make([]WorkspaceDiskStatus, 0, len(runtimes))
	for _, runtime := range runtimes {
		runtime.mu.Lock()
		statuses = append(statuses, WorkspaceDiskStatus{
			WorkspaceID: runtime.workspaceID, SandboxUID: runtime.sandboxUID,
			Usage: runtime.usage, Allowance: runtime.allowance, Frozen: runtime.frozen,
		})
		runtime.mu.Unlock()
	}
	slices.SortFunc(statuses, func(left, right WorkspaceDiskStatus) int {
		return strings.Compare(left.WorkspaceID, right.WorkspaceID)
	})
	return statuses
}

// ResizeWorkspaceDisk grows the retained qcow2 image, exposes the larger block
// device through Cloud Hypervisor, and finally grows XFS in the guest. A
// previously frozen VM is resumed only long enough to perform the guest step
// and is paused again; UnfreezeWorkspaceDisk remains explicit.
func (e *Executor) ResizeWorkspaceDisk(ctx context.Context, workspaceID string, desiredSize uint64) error {
	runtime, err := e.workspaceDiskRuntime(workspaceID)
	if err != nil {
		return err
	}
	grower, ok := e.workspaceDisks.(workspaceDiskGrower)
	if !ok {
		return ErrWorkspaceDiskNoGrower
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if desiredSize <= runtime.allowance {
		return fmt.Errorf("sandboxexec: workspace disk resize must increase allowance: current=%d requested=%d", runtime.allowance, desiredSize)
	}
	if err := grower.Grow(workspaceID, desiredSize); err != nil {
		return fmt.Errorf("sandboxexec: grow qcow2: %w", err)
	}
	if err := runtime.controller.ResizeDisk(ctx, workspaceDiskID, desiredSize); err != nil {
		return fmt.Errorf("sandboxexec: resize VMM disk: %w", err)
	}
	wasFrozen := runtime.frozen
	if wasFrozen {
		if err := runtime.controller.Resume(ctx); err != nil {
			return fmt.Errorf("sandboxexec: resume VM for XFS grow: %w", err)
		}
	}
	growErr := e.growWorkspaceGuest(ctx, e.workspaceDialer(runtime.hostSocket))
	if wasFrozen {
		if pauseErr := runtime.controller.Pause(ctx); pauseErr != nil {
			return errors.Join(growErr, fmt.Errorf("sandboxexec: re-freeze VM after XFS grow: %w", pauseErr))
		}
	}
	if growErr != nil {
		return fmt.Errorf("sandboxexec: grow guest XFS: %w", growErr)
	}
	runtime.allowance = desiredSize
	runtime.resizeDone = true
	return nil
}

// UnfreezeWorkspaceDisk resumes a VM after pressure has been resolved. A disk
// that is still above 90% is refused unless a complete resize has provided a
// larger allowance.
func (e *Executor) UnfreezeWorkspaceDisk(ctx context.Context, workspaceID string) error {
	runtime, err := e.workspaceDiskRuntime(workspaceID)
	if err != nil {
		return err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !runtime.frozen {
		return ErrWorkspaceDiskNotFrozen
	}
	if !runtime.resizeDone && atWorkspaceFreezeThreshold(runtime.usage.Used, runtime.allowance) {
		return ErrWorkspaceDiskStillFull
	}
	if err := runtime.controller.Resume(ctx); err != nil {
		return fmt.Errorf("sandboxexec: resume workspace VM: %w", err)
	}
	runtime.frozen = false
	runtime.resizeDone = false
	return nil
}

func (e *Executor) workspaceDiskRuntime(workspaceID string) (*workspaceDiskRuntime, error) {
	e.diskMu.RLock()
	runtime := e.workspaceDiskControls[workspaceID]
	e.diskMu.RUnlock()
	if runtime == nil {
		return nil, ErrWorkspaceDiskNotRunning
	}
	return runtime, nil
}

func newWorkspaceVMController(socket string) (workspaceVMController, error) {
	return cloudhypervisor.NewClient(socket)
}
