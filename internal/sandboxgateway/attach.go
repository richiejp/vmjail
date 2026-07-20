package sandboxgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// ErrAttachNoRoute is returned by an AttachBridger when no
// session can be opened for the requested sandbox_uid. Same
// shape as ErrNoRoute on the port side.
var ErrAttachNoRoute = errors.New("sandboxgateway: no attach route for sandbox")

// AttachExitError is the terminal result reported by the in-VM agent. It
// unwraps to io.EOF so existing stream pumps still treat a completed command
// as an expected close while exit-aware relays can preserve Code and Reason.
type AttachExitError struct {
	Code   int32
	Reason string
}

func (e *AttachExitError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("sandbox command execution failed: %s", e.Reason)
	}
	return fmt.Sprintf("sandbox command exited with code %d", e.Code)
}

func (*AttachExitError) Unwrap() error { return io.EOF }

// AttachRequest is the dispatch payload carried from the
// orchestrator's AttachRequest event through to the sandboxd's
// AttachBridger.Attach call. Mirrors nodectrlv1.AttachRequest
// but is wire-free so this package stays transport-agnostic.
type AttachRequest struct {
	SessionID  string
	SandboxUID string
	Command    []string
	TTY        bool
	Cols, Rows uint32
}

// AttachSession is a live attach: a bidi byte stream into the
// running sandbox plus a side-channel for window-resize events.
// Bytes flow via net.Conn; resize is out-of-band so a partial
// data frame in flight is not corrupted by an interleaved
// window-size change.
type AttachSession interface {
	net.Conn
	// Resize forwards a window-resize control event to the
	// session's downstream PTY. A non-nil return tears the
	// session down (the caller must Close the net.Conn).
	Resize(cols, rows uint32) error
}

// AttachBridger opens an attach session into a running Sandbox
// on the local Node. Implementations live in the sandboxd
// (production: vsock to daft-agent) and tests (fake in-process
// PTY). The interface is transport-free so this package never
// depends on the agent or the wire format.
type AttachBridger interface {
	Attach(ctx context.Context, req AttachRequest) (AttachSession, error)
}

// AttachDialer opens a fresh AttachSession against the agent
// listener on the configured sandbox. Single-shot: every Attach
// call produces a new session; the caller owns its lifetime. The
// AttachRequest carries the Hello fields so the dialer can drive
// the host-side framing handshake before returning.
type AttachDialer func(ctx context.Context, req AttachRequest) (AttachSession, error)

// ErrAttachRouteExists is returned by AttachRegistry.Register
// when a route is already registered for the sandbox_uid. Same
// shape as ErrRouteExists on the port side — an executor/
// orchestrator bug rather than a transient condition.
var ErrAttachRouteExists = errors.New("sandboxgateway: attach route already registered for sandbox")

// AttachRegistry is the sandbox-host's per-sandbox attach-route
// table. The executor populates it on dispatch and the
// orchestrator-driven AttachStream handler dispatches through it
// (via the AttachBridger interface) to reach the right VM.
//
// Single-key (sandbox_uid) — each sandbox has at most one agent
// attach listener; nothing like the (workspace, port) tuple
// Bridger uses for port-forward.
type AttachRegistry struct {
	mu      sync.RWMutex
	dialers map[string]AttachDialer
}

// NewAttachRegistry returns an empty AttachRegistry. Single
// instance per sandboxd process.
func NewAttachRegistry() *AttachRegistry {
	return &AttachRegistry{dialers: map[string]AttachDialer{}}
}

// Register adds a sandbox_uid → dial mapping. Returns
// ErrAttachRouteExists if the uid was already registered — same
// executor-invariant rationale as Bridger.Register.
func (r *AttachRegistry) Register(sandboxUID string, dial AttachDialer) error {
	if sandboxUID == "" {
		return errors.New("sandboxgateway: AttachRegistry.Register: empty sandboxUID")
	}
	if dial == nil {
		return errors.New("sandboxgateway: AttachRegistry.Register: nil dial")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.dialers[sandboxUID]; ok {
		return fmt.Errorf("%w: sandbox=%s", ErrAttachRouteExists, sandboxUID)
	}
	r.dialers[sandboxUID] = dial
	return nil
}

// Unregister removes the sandbox_uid mapping. Idempotent —
// removing a missing uid is a no-op so the executor's terminal-
// phase cleanup runs without first checking presence.
func (r *AttachRegistry) Unregister(sandboxUID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.dialers, sandboxUID)
}

// Attach satisfies AttachBridger by looking the sandbox_uid up
// and delegating to the registered dialer. Returns
// ErrAttachNoRoute when no mapping exists. The session's
// lifetime is the caller's responsibility once Attach returns.
func (r *AttachRegistry) Attach(ctx context.Context, req AttachRequest) (AttachSession, error) {
	dial := r.lookup(req.SandboxUID)
	if dial == nil {
		return nil, fmt.Errorf("%w: sandbox=%s", ErrAttachNoRoute, req.SandboxUID)
	}
	return dial(ctx, req)
}

func (r *AttachRegistry) lookup(sandboxUID string) AttachDialer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.dialers[sandboxUID]
}
