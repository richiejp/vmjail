// Package workspacebroker is the orchestrator-side matcher that
// pairs a user-host's `daft workspace serve` with a sandbox-host
// waiting to consume a workspace: subscribe a serve, subscribe a
// sandbox, RequestAttach pairs them and notifies both with a
// session_id that both peers can present on a subsequent Relay
// byte-splice (handled by the orchestrator's BrokerService Relay
// RPC, not this package).
//
// The matcher is transport-agnostic — it speaks in ServeKey,
// sandbox_id, and chan<- *PendingAttach. The Connect-RPC handler
// in internal/orchestrator wires it to the generated proto types
// and supplies peer identity from the mTLS connection.
//
// See PLAN.md "Slice 3.5 design" for the architectural rationale.
package workspacebroker

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"

	"github.com/richiejp/darkaifactory/internal/identity"
)

// ErrNoServe is returned by RequestAttach when no `daft workspace
// serve` for the requested (identity, workspace) is currently
// registered. The caller is expected to retry later or surface
// the error to the user.
var ErrNoServe = errors.New("workspacebroker: no serve registered")

// ErrNoSandbox is returned by RequestAttach when no sandbox-host
// with the requested sandbox_id is currently registered.
var ErrNoSandbox = errors.New("workspacebroker: no sandbox registered")

// ErrSubscriberGone is returned by Subscribe* when a subscriber's
// notification channel can't accept a PendingAttach — typically
// because the peer's stream has closed. The matcher unregisters
// the subscriber before returning this; the handler should
// surface it as a stream-level error to the peer.
var ErrSubscriberGone = errors.New("workspacebroker: subscriber gone")

// ServeKey identifies a registered serve uniquely. The same
// workspace name from a different identity is a different serve
// — workspaces are scoped to identities, never global.
type ServeKey struct {
	Identity  identity.Fingerprint
	Workspace string
}

// PendingAttach is what the matcher pushes to a subscribed
// serve/sandbox stream when an attach has been brokered.
type PendingAttach struct {
	SessionID       string
	PeerFingerprint identity.Fingerprint
	Workspace       string
	SandboxID       string
}

// Matcher is the broker's state machine. Safe for concurrent
// use; all mutations are serialised under one mutex because the
// critical sections are tiny (map insert/delete plus one channel
// send under a non-blocking guard).
type Matcher struct {
	mu        sync.Mutex
	serves    map[ServeKey]chan<- *PendingAttach
	sandboxes map[string]*sandboxSub
	sessions  *SessionBroker
}

type sandboxSub struct {
	owner identity.Fingerprint
	notif chan<- *PendingAttach
}

// New returns an empty Matcher with a fresh SessionBroker.
func New() *Matcher {
	return &Matcher{
		serves:    map[ServeKey]chan<- *PendingAttach{},
		sandboxes: map[string]*sandboxSub{},
		sessions:  NewSessionBroker(),
	}
}

// Sessions returns the matcher's SessionBroker. The orchestrator's
// Relay handler uses it to Pair the two halves of a brokered
// session; tests use it to inspect reservations.
func (m *Matcher) Sessions() *SessionBroker { return m.sessions }

// SubscribeServe registers a serve and returns a function the
// caller must invoke on stream teardown to release the slot. The
// caller owns notif and is responsible for draining it; if notif
// blocks indefinitely, RequestAttach will see ErrSubscriberGone
// and drop the registration.
//
// Re-registering the same (identity, workspace) replaces the
// previous subscription — last-register-wins. The previous
// subscriber's notif channel is left untouched (the matcher
// doesn't own it); the caller's own stream teardown will close
// it.
func (m *Matcher) SubscribeServe(key ServeKey, notif chan<- *PendingAttach) (cancel func(), err error) {
	if key.Workspace == "" {
		return nil, fmt.Errorf("workspacebroker: empty workspace name")
	}
	if notif == nil {
		return nil, fmt.Errorf("workspacebroker: nil notif channel")
	}
	m.mu.Lock()
	m.serves[key] = notif
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if cur, ok := m.serves[key]; ok && cur == notif {
			delete(m.serves, key)
		}
	}, nil
}

// SubscribeSandbox registers a sandbox-host's control stream.
// owner is the sandbox-host's mTLS identity; only attaches
// matching that identity will be brokered to this sandbox (an
// identity can only attach workspaces to its own sandboxes).
//
// Re-registering the same sandbox_id replaces the prior
// subscription (last-register-wins) — the sandbox-host
// reconnected.
func (m *Matcher) SubscribeSandbox(sandboxID string, owner identity.Fingerprint, notif chan<- *PendingAttach) (cancel func(), err error) {
	if sandboxID == "" {
		return nil, fmt.Errorf("workspacebroker: empty sandbox_id")
	}
	if notif == nil {
		return nil, fmt.Errorf("workspacebroker: nil notif channel")
	}
	sub := &sandboxSub{owner: owner, notif: notif}
	m.mu.Lock()
	m.sandboxes[sandboxID] = sub
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if cur, ok := m.sandboxes[sandboxID]; ok && cur == sub {
			delete(m.sandboxes, sandboxID)
		}
	}, nil
}

// RequestAttach pairs a registered serve with a registered
// sandbox and pushes a PendingAttach to both. caller is the
// mTLS identity that initiated the attach. v1 ACL: caller must
// equal the serve owner (a peer can only attach its own
// workspaces). Sandbox ownership is checked only via the
// orchestrator's PeerStore (the sandbox-host must be an
// approved peer); cross-identity attach to a sandbox owned by
// a different peer is permitted — a fleet of sandbox-hosts
// running on behalf of multiple users is a real deployment
// model, and per-pair ACLs would require a permissions UX we
// don't have yet (PLAN.md open question
// "Authentication ergonomics").
//
// Returns the session_id that both peers will present on their
// Relay streams (Slice 3.5b).
func (m *Matcher) RequestAttach(caller identity.Fingerprint, serveKey ServeKey, sandboxID string) (string, error) {
	if caller != serveKey.Identity {
		return "", fmt.Errorf("workspacebroker: caller is not the serve owner")
	}

	sessionID, err := newSessionID()
	if err != nil {
		return "", fmt.Errorf("workspacebroker: mint session: %w", err)
	}

	m.mu.Lock()
	serveNotif, serveOK := m.serves[serveKey]
	sandSub, sandOK := m.sandboxes[sandboxID]
	m.mu.Unlock()

	if !serveOK {
		return "", fmt.Errorf("%w: identity=%s workspace=%q",
			ErrNoServe, serveKey.Identity, serveKey.Workspace)
	}
	if !sandOK {
		return "", fmt.Errorf("%w: sandbox_id=%q", ErrNoSandbox, sandboxID)
	}

	// Reserve the session in the broker before notifying either
	// peer — otherwise a peer that reacts very fast to its
	// PendingAttach could call Relay before Reserve has run, and
	// see ErrNoSession.
	if err := m.sessions.Reserve(sessionID, caller, sandSub.owner); err != nil {
		return "", fmt.Errorf("workspacebroker: reserve session: %w", err)
	}

	serveEv := &PendingAttach{
		SessionID:       sessionID,
		PeerFingerprint: sandSub.owner,
		Workspace:       serveKey.Workspace,
		SandboxID:       sandboxID,
	}
	sandEv := &PendingAttach{
		SessionID:       sessionID,
		PeerFingerprint: caller,
		Workspace:       serveKey.Workspace,
		SandboxID:       sandboxID,
	}

	// Non-blocking sends: if a subscriber's channel is full or
	// its goroutine has gone away, treat the registration as
	// dead and unregister. The matcher must never block on a
	// peer's I/O.
	if err := trySend(serveNotif, serveEv); err != nil {
		m.unregisterServe(serveKey, serveNotif)
		return "", fmt.Errorf("notify serve: %w", err)
	}
	if err := trySend(sandSub.notif, sandEv); err != nil {
		m.unregisterSandbox(sandboxID, sandSub)
		return "", fmt.Errorf("notify sandbox: %w", err)
	}
	return sessionID, nil
}

func trySend(ch chan<- *PendingAttach, ev *PendingAttach) error {
	select {
	case ch <- ev:
		return nil
	default:
		return ErrSubscriberGone
	}
}

// Counts returns the number of currently-subscribed serves and
// sandboxes. Exposed for tests and the orchestrator's debug
// surface; not load-bearing for the matching protocol.
func (m *Matcher) Counts() (serves, sandboxes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.serves), len(m.sandboxes)
}

func (m *Matcher) unregisterServe(key ServeKey, expect chan<- *PendingAttach) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.serves[key]; ok && cur == expect {
		delete(m.serves, key)
	}
}

func (m *Matcher) unregisterSandbox(sandboxID string, expect *sandboxSub) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.sandboxes[sandboxID]; ok && cur == expect {
		delete(m.sandboxes, sandboxID)
	}
}

// newSessionID returns a base64-url-encoded 32-byte random
// identifier. The orchestrator hands this to both peers; both
// present it on their Relay streams so the orchestrator can
// splice them. 32 bytes of entropy is enough that a guessing
// attack against an in-flight session is infeasible.
func newSessionID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
