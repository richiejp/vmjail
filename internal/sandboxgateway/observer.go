package sandboxgateway

import (
	"errors"
	"fmt"
	"sync"
)

// ErrObserverNoRoute is returned when no observer log is registered
// for the requested sandbox_uid (the sandbox isn't running on this
// Node, or never had an observer). Same shape as ErrAttachNoRoute.
var ErrObserverNoRoute = errors.New("sandboxgateway: no observer log for sandbox")

// ErrObserverRouteExists is returned by ObserverRegistry.Register
// when a route is already registered for the sandbox_uid — an
// executor-invariant violation, not a transient condition.
var ErrObserverRouteExists = errors.New("sandboxgateway: observer route already registered for sandbox")

// ErrSummaryNoRoute / ErrSummaryRouteExists are the build-progress
// summary analogues of the observer-log route errors above.
var (
	ErrSummaryNoRoute     = errors.New("sandboxgateway: no build-progress summary for sandbox")
	ErrSummaryRouteExists = errors.New("sandboxgateway: summary route already registered for sandbox")
)

// pathRoutes is a thread-safe sandbox_uid → absolute host-file-path
// table. The executor populates it on dispatch and the orchestrator-
// driven stream handlers resolve a uid to a path to read. It backs both
// the observer-log and build-progress-summary registries, which differ
// only in their error wording.
type pathRoutes struct {
	mu      sync.RWMutex
	paths   map[string]string
	noRoute error
	exists  error
}

func newPathRoutes(noRoute, exists error) *pathRoutes {
	return &pathRoutes{paths: map[string]string{}, noRoute: noRoute, exists: exists}
}

// Register adds a sandbox_uid → path mapping. Returns the registry's
// "exists" error if the uid was already registered.
func (r *pathRoutes) Register(sandboxUID, path string) error {
	if sandboxUID == "" {
		return errors.New("sandboxgateway: Register: empty sandboxUID")
	}
	if path == "" {
		return errors.New("sandboxgateway: Register: empty path")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.paths[sandboxUID]; ok {
		return fmt.Errorf("%w: sandbox=%s", r.exists, sandboxUID)
	}
	r.paths[sandboxUID] = path
	return nil
}

// Unregister removes the sandbox_uid mapping. Idempotent so the
// executor's terminal-phase cleanup runs without a presence check.
func (r *pathRoutes) Unregister(sandboxUID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.paths, sandboxUID)
}

// Resolve returns the path for sandbox_uid, or the registry's "no
// route" error when none is registered.
func (r *pathRoutes) Resolve(sandboxUID string) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	path, ok := r.paths[sandboxUID]
	if !ok {
		return "", fmt.Errorf("%w: sandbox=%s", r.noRoute, sandboxUID)
	}
	return path, nil
}

// ObserverRegistry is the sandbox-host's per-sandbox observer-log
// table: sandbox_uid → absolute path of that sandbox's host-side
// observer log. The executor populates it on dispatch (only for
// sandboxes whose ObserverLogPath is set) and the orchestrator-driven
// ObserverStream handler resolves a uid to a path to tail.
//
// Unlike AttachRegistry there is no dial closure — the "session" is a
// read-only file the handler opens itself (see internal/observertail),
// so the registry stores just the path.
type ObserverRegistry struct{ *pathRoutes }

// NewObserverRegistry returns an empty registry. One instance per
// sandboxd process, shared between the executor (which registers) and
// the nodectrl client (which resolves).
func NewObserverRegistry() *ObserverRegistry {
	return &ObserverRegistry{newPathRoutes(ErrObserverNoRoute, ErrObserverRouteExists)}
}

// SummaryRegistry is the per-sandbox build-progress-summary analogue of
// ObserverRegistry: sandbox_uid → absolute host path of that sandbox's
// summary JSON (the guest writes it onto the observe virtiofs share,
// whose host backing dir this path points into). The executor registers
// it on dispatch and the ObserveSummary handler resolves it to read.
type SummaryRegistry struct{ *pathRoutes }

// NewSummaryRegistry returns an empty build-progress-summary registry.
func NewSummaryRegistry() *SummaryRegistry {
	return &SummaryRegistry{newPathRoutes(ErrSummaryNoRoute, ErrSummaryRouteExists)}
}
