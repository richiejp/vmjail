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

// ObserverRegistry is the sandbox-host's per-sandbox observer-log
// table: sandbox_uid → absolute path of that sandbox's host-side
// observer log. The executor populates it on dispatch (only for
// sandboxes whose ObserverLogPath is set) and the orchestrator-driven
// ObserverStream handler resolves a uid to a path to tail.
//
// Unlike AttachRegistry there is no dial closure — the "session" is a
// read-only file the handler opens itself (see internal/observertail),
// so the registry stores just the path.
type ObserverRegistry struct {
	mu    sync.RWMutex
	paths map[string]string
}

// NewObserverRegistry returns an empty registry. One instance per
// sandboxd process, shared between the executor (which registers) and
// the nodectrl client (which resolves).
func NewObserverRegistry() *ObserverRegistry {
	return &ObserverRegistry{paths: map[string]string{}}
}

// Register adds a sandbox_uid → log-path mapping. Returns
// ErrObserverRouteExists if the uid was already registered.
func (r *ObserverRegistry) Register(sandboxUID, logPath string) error {
	if sandboxUID == "" {
		return errors.New("sandboxgateway: ObserverRegistry.Register: empty sandboxUID")
	}
	if logPath == "" {
		return errors.New("sandboxgateway: ObserverRegistry.Register: empty logPath")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.paths[sandboxUID]; ok {
		return fmt.Errorf("%w: sandbox=%s", ErrObserverRouteExists, sandboxUID)
	}
	r.paths[sandboxUID] = logPath
	return nil
}

// Unregister removes the sandbox_uid mapping. Idempotent so the
// executor's terminal-phase cleanup runs without a presence check.
func (r *ObserverRegistry) Unregister(sandboxUID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.paths, sandboxUID)
}

// Resolve returns the observer-log path for sandbox_uid, or
// ErrObserverNoRoute when none is registered.
func (r *ObserverRegistry) Resolve(sandboxUID string) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	path, ok := r.paths[sandboxUID]
	if !ok {
		return "", fmt.Errorf("%w: sandbox=%s", ErrObserverNoRoute, sandboxUID)
	}
	return path, nil
}
