// Package sandboxgateway is the sandbox-host's per-(workspace, port)
// port-forward registry. Each entry maps a (workspace UUID, container
// port) tuple to a DialFunc — typically a closure over
// sandbox.DialGuestVsock that targets the running VM's hybrid-vsock
// UDS at the per-sandbox HostSocket and the executor-allocated
// guest-side VsockPort.
//
// The Bridger.Bridge entry-point is what the orchestrator-driven
// ingress path (Slice C1.3c, enabling C2) calls when it has accepted
// an inbound byte stream tagged with a workspace + port label: it
// looks up the matching DialFunc, dials into the guest, and pumps
// bytes both directions until either side closes.
//
// Lifecycle is owned by the sandboxexec executor: Register on a
// successful dispatch, Unregister when the Sandbox transitions to
// a terminal phase (Slice C1.3b). Stateless DialFunc indirection
// keeps the package free of any vsock or CH knowledge so the
// orchestrator-side handler can unit-test against an in-process
// net.Pipe stub.
package sandboxgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/localai-org/darkaifactory/internal/oops"
)

// ErrNoRoute is returned by Bridge when no DialFunc has been
// registered for the (workspaceUUID, port) tuple. Callers (the
// orchestrator-side handler) typically map this onto an HTTP
// 503 / 404 — there's no sandbox to relay to.
var ErrNoRoute = errors.New("sandboxgateway: no route for (workspace, port)")

// ErrRouteExists is returned by Register when a route already
// exists for the (workspaceUUID, port) tuple. Indicates an
// executor/orchestrator bug — two dispatches racing on the same
// pair — rather than a transient condition.
var ErrRouteExists = errors.New("sandboxgateway: route already registered for (workspace, port)")

// DialFunc opens a fresh byte tunnel into the guest serving the
// (workspace, port) this DialFunc was registered under. Each call
// returns a new conn; callers own its lifetime.
type DialFunc func(ctx context.Context) (net.Conn, error)

// Bridger is the in-memory (workspaceUUID, port) → DialFunc table
// with a Bridge entry-point that pumps bytes between an upstream
// conn (the orchestrator-side ingress stream) and a freshly-dialed
// downstream guest conn.
//
// Safe for concurrent use; a single Bridger instance lives per
// sandboxd process.
type Bridger struct {
	mu     sync.RWMutex
	routes map[routeKey]DialFunc
}

// routeKey is the (workspaceUUID, port) tuple keyed by the table.
// Kept private — callers go through the Register / Unregister /
// Bridge methods rather than constructing keys themselves.
type routeKey struct {
	workspaceUUID string
	port          uint32
}

// New returns an empty Bridger. Single instance per sandboxd.
func New() *Bridger {
	return &Bridger{routes: map[routeKey]DialFunc{}}
}

// Register adds a (workspaceUUID, port) → dial mapping. Returns
// ErrRouteExists if the tuple was already registered: this is an
// invariant the executor enforces (one Sandbox per workspace+port
// pair at a time), so the duplicate is treated as a bug rather
// than a refresh.
func (b *Bridger) Register(workspaceUUID string, port uint32, dial DialFunc) error {
	if workspaceUUID == "" {
		return errors.New("sandboxgateway: Register: empty workspaceUUID")
	}
	if port == 0 {
		return errors.New("sandboxgateway: Register: zero port")
	}
	if dial == nil {
		return errors.New("sandboxgateway: Register: nil dial")
	}
	key := routeKey{workspaceUUID: workspaceUUID, port: port}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.routes[key]; ok {
		return fmt.Errorf("%w: workspace=%s port=%d", ErrRouteExists, workspaceUUID, port)
	}
	b.routes[key] = dial
	return nil
}

// Unregister removes the (workspaceUUID, port) mapping. Idempotent:
// removing a missing tuple is a no-op so the executor's terminal-
// phase cleanup can run without first checking presence.
func (b *Bridger) Unregister(workspaceUUID string, port uint32) {
	key := routeKey{workspaceUUID: workspaceUUID, port: port}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.routes, key)
}

// lookup returns the DialFunc for the tuple, or nil + ErrNoRoute.
// Split out so Bridge can hold the read lock for the minimum
// possible window — the dial + pump runs outside.
func (b *Bridger) lookup(workspaceUUID string, port uint32) (DialFunc, error) {
	key := routeKey{workspaceUUID: workspaceUUID, port: port}
	b.mu.RLock()
	defer b.mu.RUnlock()
	dial, ok := b.routes[key]
	if !ok {
		return nil, fmt.Errorf("%w: workspace=%s port=%d", ErrNoRoute, workspaceUUID, port)
	}
	return dial, nil
}

// Bridge wires upstream to a freshly-dialed downstream conn for
// the (workspaceUUID, port) tuple and copies bytes in both
// directions until either side closes.
//
// Returns ErrNoRoute if no mapping is registered, the dial error if
// the DialFunc fails, or the first non-EOF copy error (best-effort —
// the pump tears both sides down regardless). Closing upstream is
// the caller's responsibility.
func (b *Bridger) Bridge(
	ctx context.Context,
	workspaceUUID string,
	port uint32,
	upstream net.Conn,
) error {
	dial, err := b.lookup(workspaceUUID, port)
	if err != nil {
		return err
	}
	downstream, err := dial(ctx)
	if err != nil {
		return fmt.Errorf("sandboxgateway: dial workspace=%s port=%d: %w", workspaceUUID, port, err)
	}
	defer func() { oops.Skip("sandboxgateway: close downstream", downstream.Close(), net.ErrClosed) }()
	return pumpBidirectional(ctx, upstream, downstream)
}

// pumpBidirectional copies bytes between a and b until either
// direction returns. Half-close is honoured when the underlying
// conn supports it (net.Pipe + tls.Conn + tcp Conn all do) so an
// HTTP-style read-until-EOF on the far side actually sees the EOF.
func pumpBidirectional(ctx context.Context, a, b net.Conn) error {
	// Cancel-driven teardown: when ctx fires, close both ends so
	// the in-flight io.Copy returns. Without this, a hung
	// downstream socket would outlive the request.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			oops.Skip("sandboxgateway: ctx-close a", a.Close(), net.ErrClosed)
			oops.Skip("sandboxgateway: ctx-close b", b.Close(), net.ErrClosed)
		case <-done:
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	errCh := make(chan error, 2)
	go func() {
		defer wg.Done()
		_, err := io.Copy(b, a)
		if cw, ok := b.(interface{ CloseWrite() error }); ok {
			oops.Skip("sandboxgateway: half-close b", cw.CloseWrite(), net.ErrClosed)
		}
		errCh <- err
	}()
	go func() {
		defer wg.Done()
		_, err := io.Copy(a, b)
		if cw, ok := a.(interface{ CloseWrite() error }); ok {
			oops.Skip("sandboxgateway: half-close a", cw.CloseWrite(), net.ErrClosed)
		}
		errCh <- err
	}()
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err == nil {
			continue
		}
		if oops.IsExpectedPumpExit(err) {
			continue
		}
		return err
	}
	return nil
}
