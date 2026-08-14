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
	workspaceGuestAttemptTimeout     = 5 * time.Second
	workspaceGuestAttempts           = 3
	workspaceCapacityAttempts        = 20
	workspaceCapacityAttemptTimeout  = time.Second
	workspaceGuestRetryInterval      = 100 * time.Millisecond
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

type workspaceDiskPressureSource interface {
	UnderPressure() bool
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
	lastPoll   time.Time
	lastUsed   uint64
	growthRate float64
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
	now := time.Now()
	if !runtime.lastPoll.IsZero() && now.After(runtime.lastPoll) && usage.Used > runtime.lastUsed {
		runtime.growthRate = float64(usage.Used-runtime.lastUsed) / now.Sub(runtime.lastPoll).Seconds()
	} else {
		runtime.growthRate = 0
	}
	runtime.lastPoll, runtime.lastUsed = now, usage.Used
	runtime.usage = usage
	shouldFreeze := !runtime.frozen && atWorkspaceFreezeThreshold(usage.Used, runtime.allowance)
	runtime.mu.Unlock()
	if pressure, ok := e.workspaceDisks.(workspaceDiskPressureSource); ok && pressure.UnderPressure() {
		if err := e.freezeGrowingWorkspaceDisks(ctx); err != nil {
			return err
		}
	}
	if !shouldFreeze {
		return nil
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.frozen {
		return nil
	}
	if err := runtime.controller.Pause(ctx); err != nil {
		return fmt.Errorf("pause at storage threshold: %w", err)
	}
	runtime.frozen = true
	return nil
}

func (e *Executor) freezeGrowingWorkspaceDisks(ctx context.Context) error {
	e.diskMu.RLock()
	type candidate struct {
		runtime *workspaceDiskRuntime
		rate    float64
		used    uint64
	}
	candidates := make([]candidate, 0, len(e.workspaceDiskControls))
	for _, runtime := range e.workspaceDiskControls {
		runtime.mu.Lock()
		candidates = append(candidates, candidate{runtime: runtime, rate: runtime.growthRate, used: runtime.usage.Used})
		runtime.mu.Unlock()
	}
	e.diskMu.RUnlock()
	slices.SortFunc(candidates, func(left, right candidate) int {
		if left.rate != right.rate {
			if left.rate > right.rate {
				return -1
			}
			return 1
		}
		if left.used != right.used {
			if left.used > right.used {
				return -1
			}
			return 1
		}
		return strings.Compare(left.runtime.workspaceID, right.runtime.workspaceID)
	})
	for _, candidate := range candidates {
		runtime := candidate.runtime
		runtime.mu.Lock()
		if runtime.frozen || runtime.growthRate <= 0 {
			runtime.mu.Unlock()
			continue
		}
		if err := runtime.controller.Pause(ctx); err != nil {
			runtime.mu.Unlock()
			return fmt.Errorf("pause %s at host storage pressure: %w", runtime.workspaceID, err)
		}
		runtime.frozen = true
		runtime.mu.Unlock()
	}
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
	_, _ = fmt.Fprintf(e.stdout, "workspace-disk: resize %s qcow2=%d\n", workspaceID, desiredSize)
	wasFrozen := runtime.frozen
	if wasFrozen {
		if err := runtime.controller.Resume(ctx); err != nil {
			return fmt.Errorf("sandboxexec: resume VM for XFS grow: %w", err)
		}
		_, _ = fmt.Fprintf(e.stdout, "workspace-disk: resize %s resumed\n", workspaceID)
		if err := e.waitWorkspaceGuestBounded(ctx, runtime.hostSocket); err != nil {
			return fmt.Errorf("sandboxexec: wait for resumed guest: %w", err)
		}
		_, _ = fmt.Fprintf(e.stdout, "workspace-disk: resize %s guest-ready\n", workspaceID)
	}
	resizeErr := runtime.controller.ResizeDisk(ctx, workspaceDiskID, desiredSize)
	if resizeErr != nil {
		resizeErr = fmt.Errorf("sandboxexec: resize VMM disk: %w", resizeErr)
	} else {
		_, _ = fmt.Fprintf(e.stdout, "workspace-disk: resize %s vmm=%d\n", workspaceID, desiredSize)
	}
	var capacityErr error
	if resizeErr == nil {
		capacityErr = e.waitWorkspaceCapacityBounded(ctx, runtime.hostSocket, desiredSize)
		if capacityErr != nil {
			capacityErr = fmt.Errorf("sandboxexec: wait for guest block capacity: %w", capacityErr)
		} else {
			_, _ = fmt.Fprintf(e.stdout, "workspace-disk: resize %s capacity=%d\n", workspaceID, desiredSize)
		}
	}
	var growErr error
	if resizeErr == nil && capacityErr == nil {
		// The capacity barrier proves the guest block layer has published the
		// new size. xfs_growfs must not be used as that probe: when invoked
		// during the transition it can block until its short guest deadline.
		growErr = e.growWorkspaceGuestBounded(ctx, runtime.hostSocket)
		if growErr != nil {
			growErr = fmt.Errorf("sandboxexec: grow guest XFS: %w", growErr)
		} else {
			_, _ = fmt.Fprintf(e.stdout, "workspace-disk: resize %s xfs=%d\n", workspaceID, desiredSize)
		}
	}
	if wasFrozen {
		if pauseErr := runtime.controller.Pause(ctx); pauseErr != nil {
			return errors.Join(resizeErr, capacityErr, growErr, fmt.Errorf("sandboxexec: re-freeze VM after XFS grow: %w", pauseErr))
		}
		_, _ = fmt.Fprintf(e.stdout, "workspace-disk: resize %s re-frozen\n", workspaceID)
	}
	if err := errors.Join(resizeErr, capacityErr, growErr); err != nil {
		return err
	}
	runtime.allowance = desiredSize
	runtime.resizeDone = true
	return nil
}

func (e *Executor) waitWorkspaceCapacityBounded(ctx context.Context, hostSocket string, desired uint64) error {
	var lastCapacity uint64
	var lastErr error
	for attempt := 0; attempt < workspaceCapacityAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, workspaceCapacityAttemptTimeout)
		lastCapacity, lastErr = e.getWorkspaceCapacity(attemptCtx, e.workspaceDialer(hostSocket))
		cancel()
		if lastErr == nil && lastCapacity >= desired {
			return e.waitWorkspaceGuestRetry(ctx)
		}
		if ctx.Err() != nil {
			return errors.Join(lastErr, ctx.Err())
		}
		if attempt+1 < workspaceCapacityAttempts {
			if err := e.waitWorkspaceGuestRetry(ctx); err != nil {
				return errors.Join(lastErr, err)
			}
		}
	}
	if lastErr != nil {
		return fmt.Errorf("guest capacity unavailable after %d attempts: %w", workspaceCapacityAttempts, lastErr)
	}
	return fmt.Errorf("guest capacity=%d did not reach requested=%d after %d attempts", lastCapacity, desired, workspaceCapacityAttempts)
}

// growWorkspaceGuestBounded tolerates a transient vsock interruption while
// Cloud Hypervisor publishes the resized block device. xfs_growfs is
// idempotent, so retrying the exact grow request is safe; each attempt and the
// total operation remain bounded by the caller's deadline.
func (e *Executor) growWorkspaceGuestBounded(ctx context.Context, hostSocket string) error {
	var lastErr error
	for attempt := 0; attempt < workspaceGuestAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, workspaceGuestAttemptTimeout)
		lastErr = e.growWorkspaceGuest(attemptCtx, e.workspaceDialer(hostSocket))
		cancel()
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return errors.Join(lastErr, ctx.Err())
		}
		if attempt+1 < workspaceGuestAttempts {
			if err := e.waitWorkspaceGuestRetry(ctx); err != nil {
				return errors.Join(lastErr, err)
			}
		}
	}
	return fmt.Errorf("failed after %d attempts: %w", workspaceGuestAttempts, lastErr)
}

func (e *Executor) waitWorkspaceGuestBounded(ctx context.Context, hostSocket string) error {
	var lastErr error
	for attempt := 0; attempt < workspaceGuestAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, workspaceGuestAttemptTimeout)
		_, lastErr = e.getWorkspaceUsage(attemptCtx, e.workspaceDialer(hostSocket))
		cancel()
		if lastErr == nil {
			// The hybrid-vsock transport tears each one-request stream down
			// asynchronously. Opening the grow stream in the same scheduling
			// instant can race that teardown and wedge every subsequent dial.
			// Give the bounded readiness stream time to close before its caller
			// starts the next control operation.
			return e.waitWorkspaceGuestRetry(ctx)
		}
		if ctx.Err() != nil {
			return errors.Join(lastErr, ctx.Err())
		}
		if attempt+1 < workspaceGuestAttempts {
			if err := e.waitWorkspaceGuestRetry(ctx); err != nil {
				return errors.Join(lastErr, err)
			}
		}
	}
	return fmt.Errorf("guest did not become responsive after %d attempts: %w", workspaceGuestAttempts, lastErr)
}

func waitWorkspaceGuestRetry(ctx context.Context) error {
	timer := time.NewTimer(workspaceGuestRetryInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
