package workspacebroker

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/identity"
)

const testWorkspaceName = "src"

// fp returns a deterministic test fingerprint. Tests use small
// integer seeds; this gives us readable identities that are still
// distinct uint8-prefixes from each other.
func fp(seed byte) identity.Fingerprint {
	var f identity.Fingerprint
	for i := range f {
		f[i] = seed
	}
	return f
}

func TestMatcherHappyPath(t *testing.T) {
	m := New()
	owner := fp(1)
	key := ServeKey{Identity: owner, Workspace: testWorkspaceName}

	serveCh := make(chan *PendingAttach, 1)
	cancelServe, err := m.SubscribeServe(key, serveCh)
	if err != nil {
		t.Fatalf("SubscribeServe: %v", err)
	}
	t.Cleanup(cancelServe)

	sandCh := make(chan *PendingAttach, 1)
	cancelSand, err := m.SubscribeSandbox("sb-1", owner, sandCh)
	if err != nil {
		t.Fatalf("SubscribeSandbox: %v", err)
	}
	t.Cleanup(cancelSand)

	sid, err := m.RequestAttach(owner, key, "sb-1")
	if err != nil {
		t.Fatalf("RequestAttach: %v", err)
	}
	if sid == "" {
		t.Fatal("empty session id")
	}

	got1 := recv(t, serveCh)
	got2 := recv(t, sandCh)
	if got1.SessionID != sid || got2.SessionID != sid {
		t.Errorf("session ids = %q/%q, want %q", got1.SessionID, got2.SessionID, sid)
	}
	if got1.PeerFingerprint != owner {
		t.Errorf("serve event peer = %s, want %s", got1.PeerFingerprint, owner)
	}
	if got2.PeerFingerprint != owner {
		t.Errorf("sandbox event peer = %s, want %s", got2.PeerFingerprint, owner)
	}
	if got1.Workspace != testWorkspaceName || got2.Workspace != testWorkspaceName {
		t.Errorf("workspace = %q/%q, want %q", got1.Workspace, got2.Workspace, testWorkspaceName)
	}
	if got1.SandboxID != "sb-1" || got2.SandboxID != "sb-1" {
		t.Errorf("sandbox_id = %q/%q, want sb-1", got1.SandboxID, got2.SandboxID)
	}
}

func TestSessionIDIsUnique(t *testing.T) {
	m := New()
	owner := fp(1)
	key := ServeKey{Identity: owner, Workspace: "w"}
	serveCh := make(chan *PendingAttach, 32)
	sandCh := make(chan *PendingAttach, 32)
	cancel1, _ := m.SubscribeServe(key, serveCh)
	t.Cleanup(cancel1)
	cancel2, _ := m.SubscribeSandbox("sb", owner, sandCh)
	t.Cleanup(cancel2)

	seen := map[string]struct{}{}
	for range 8 {
		sid, err := m.RequestAttach(owner, key, "sb")
		if err != nil {
			t.Fatalf("RequestAttach: %v", err)
		}
		if _, dup := seen[sid]; dup {
			t.Errorf("duplicate session id %q", sid)
		}
		seen[sid] = struct{}{}
		// Drain so trySend's default branch doesn't fire.
		<-serveCh
		<-sandCh
	}
}

func TestRequestAttachRejectsCrossIdentityAttach(t *testing.T) {
	m := New()
	alice, bob := fp(1), fp(2)
	key := ServeKey{Identity: alice, Workspace: testWorkspaceName}
	_, _ = m.SubscribeServe(key, make(chan *PendingAttach, 1))
	_, _ = m.SubscribeSandbox("sb-bob", bob, make(chan *PendingAttach, 1))

	if _, err := m.RequestAttach(bob, key, "sb-bob"); err == nil {
		t.Fatal("expected error: bob requesting attach for alice's workspace")
	}
}

// TestRequestAttachAllowsCrossOwnerSandbox: alice's workspace
// can attach to a sandbox-host running on bob's identity. The
// orchestrator's PeerStore already filtered everyone for trust;
// per-pair ACLs are out of scope for v1.
func TestRequestAttachAllowsCrossOwnerSandbox(t *testing.T) {
	m := New()
	alice, bob := fp(1), fp(2)
	key := ServeKey{Identity: alice, Workspace: testWorkspaceName}
	serveCh := make(chan *PendingAttach, 1)
	sandCh := make(chan *PendingAttach, 1)
	_, _ = m.SubscribeServe(key, serveCh)
	_, _ = m.SubscribeSandbox("sb-bob", bob, sandCh)

	sid, err := m.RequestAttach(alice, key, "sb-bob")
	if err != nil {
		t.Fatalf("RequestAttach: %v", err)
	}
	if sid == "" {
		t.Fatal("empty session id")
	}
	if got := recv(t, serveCh); got.PeerFingerprint != bob {
		t.Errorf("serve event peer = %s, want bob %s", got.PeerFingerprint, bob)
	}
	if got := recv(t, sandCh); got.PeerFingerprint != alice {
		t.Errorf("sandbox event peer = %s, want alice %s", got.PeerFingerprint, alice)
	}
}

func TestRequestAttachWithoutServe(t *testing.T) {
	m := New()
	alice := fp(1)
	_, _ = m.SubscribeSandbox("sb", alice, make(chan *PendingAttach, 1))

	_, err := m.RequestAttach(alice, ServeKey{Identity: alice, Workspace: "absent"}, "sb")
	if !errors.Is(err, ErrNoServe) {
		t.Fatalf("err = %v, want ErrNoServe", err)
	}
}

func TestRequestAttachWithoutSandbox(t *testing.T) {
	m := New()
	alice := fp(1)
	key := ServeKey{Identity: alice, Workspace: testWorkspaceName}
	_, _ = m.SubscribeServe(key, make(chan *PendingAttach, 1))

	_, err := m.RequestAttach(alice, key, "missing-sandbox")
	if !errors.Is(err, ErrNoSandbox) {
		t.Fatalf("err = %v, want ErrNoSandbox", err)
	}
}

// TestSubscribeServeReplacesPrior pins last-register-wins: a
// reconnecting serve replaces the previous registration.
func TestSubscribeServeReplacesPrior(t *testing.T) {
	m := New()
	alice := fp(1)
	key := ServeKey{Identity: alice, Workspace: testWorkspaceName}

	first := make(chan *PendingAttach, 1)
	_, _ = m.SubscribeServe(key, first)
	second := make(chan *PendingAttach, 1)
	_, _ = m.SubscribeServe(key, second)

	_, _ = m.SubscribeSandbox("sb", alice, make(chan *PendingAttach, 1))
	if _, err := m.RequestAttach(alice, key, "sb"); err != nil {
		t.Fatalf("RequestAttach: %v", err)
	}

	select {
	case <-first:
		t.Error("first subscriber received an attach despite being superseded")
	case <-second:
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Error("second subscriber did not receive an attach")
	}
}

// TestSubscribeServeRejectsBadInput pins the matcher's input
// guards. The Connect handler will already reject empty fields,
// but the matcher must be defensive too.
func TestSubscribeServeRejectsBadInput(t *testing.T) {
	m := New()
	if _, err := m.SubscribeServe(ServeKey{Workspace: ""}, make(chan *PendingAttach)); err == nil {
		t.Error("expected error for empty workspace")
	}
	if _, err := m.SubscribeServe(ServeKey{Workspace: "w"}, nil); err == nil {
		t.Error("expected error for nil notif")
	}
	if _, err := m.SubscribeSandbox("", fp(1), make(chan *PendingAttach)); err == nil {
		t.Error("expected error for empty sandbox_id")
	}
	if _, err := m.SubscribeSandbox("sb", fp(1), nil); err == nil {
		t.Error("expected error for nil notif")
	}
}

// TestCancelUnregisters verifies the cancel func returned by
// Subscribe* tears down the registration.
func TestCancelUnregisters(t *testing.T) {
	m := New()
	alice := fp(1)
	key := ServeKey{Identity: alice, Workspace: testWorkspaceName}

	cancelServe, _ := m.SubscribeServe(key, make(chan *PendingAttach, 1))
	cancelServe()

	_, _ = m.SubscribeSandbox("sb", alice, make(chan *PendingAttach, 1))
	if _, err := m.RequestAttach(alice, key, "sb"); !errors.Is(err, ErrNoServe) {
		t.Errorf("after cancel: err = %v, want ErrNoServe", err)
	}
}

// TestSlowSubscriberIsDropped — a subscriber whose channel can't
// accept must be unregistered so subsequent RequestAttach calls
// see "no serve" rather than blocking on the dead subscriber.
func TestSlowSubscriberIsDropped(t *testing.T) {
	m := New()
	alice := fp(1)
	key := ServeKey{Identity: alice, Workspace: testWorkspaceName}

	// Unbuffered channel that nothing is reading — trySend will
	// hit the default branch immediately.
	deadServe := make(chan *PendingAttach)
	_, _ = m.SubscribeServe(key, deadServe)
	_, _ = m.SubscribeSandbox("sb", alice, make(chan *PendingAttach, 1))

	_, err := m.RequestAttach(alice, key, "sb")
	if !errors.Is(err, ErrSubscriberGone) {
		t.Fatalf("first call: err = %v, want ErrSubscriberGone", err)
	}
	// Second call: serve has been auto-unregistered.
	_, err = m.RequestAttach(alice, key, "sb")
	if !errors.Is(err, ErrNoServe) {
		t.Fatalf("second call: err = %v, want ErrNoServe", err)
	}
}

// TestSlowSandboxIsDropped — same for the sandbox side. Serve
// has already been notified (and stays registered for next call),
// but the sandbox subscriber is dropped.
func TestSlowSandboxIsDropped(t *testing.T) {
	m := New()
	alice := fp(1)
	key := ServeKey{Identity: alice, Workspace: testWorkspaceName}

	serveCh := make(chan *PendingAttach, 4)
	_, _ = m.SubscribeServe(key, serveCh)
	deadSand := make(chan *PendingAttach)
	_, _ = m.SubscribeSandbox("sb", alice, deadSand)

	_, err := m.RequestAttach(alice, key, "sb")
	if !errors.Is(err, ErrSubscriberGone) {
		t.Fatalf("err = %v, want ErrSubscriberGone", err)
	}
	_, err = m.RequestAttach(alice, key, "sb")
	if !errors.Is(err, ErrNoSandbox) {
		t.Fatalf("err = %v, want ErrNoSandbox", err)
	}
}

// TestCounts pins what Matcher.Counts reports as subscribers
// register and cancel.
func TestCounts(t *testing.T) {
	m := New()
	if s, sb := m.Counts(); s != 0 || sb != 0 {
		t.Errorf("empty matcher counts = %d/%d, want 0/0", s, sb)
	}
	alice := fp(1)
	c1, _ := m.SubscribeServe(ServeKey{Identity: alice, Workspace: testWorkspaceName}, make(chan *PendingAttach, 1))
	c2, _ := m.SubscribeSandbox("sb", alice, make(chan *PendingAttach, 1))
	if s, sb := m.Counts(); s != 1 || sb != 1 {
		t.Errorf("after subscribe counts = %d/%d, want 1/1", s, sb)
	}
	c1()
	c2()
	if s, sb := m.Counts(); s != 0 || sb != 0 {
		t.Errorf("after cancel counts = %d/%d, want 0/0", s, sb)
	}
}

// TestNewSessionIDFormat — base64-url, 32 bytes of entropy → 43
// chars unpadded.
func TestNewSessionIDFormat(t *testing.T) {
	sid, err := newSessionID()
	if err != nil {
		t.Fatalf("newSessionID: %v", err)
	}
	if len(sid) != 43 {
		t.Errorf("session id len = %d, want 43", len(sid))
	}
	if strings.ContainsAny(sid, "+/=") {
		t.Errorf("session id %q has padding or non-url chars", sid)
	}
}

// TestMatcherSessionsIsLive verifies that Sessions() returns the
// embedded broker that RequestAttach has been reserving against —
// not a fresh instance — so that the orchestrator's Relay handler
// observes the reservations RequestAttach creates.
func TestMatcherSessionsIsLive(t *testing.T) {
	m := New()
	s := m.Sessions()
	if s == nil {
		t.Fatal("Sessions() nil")
	}
	if m.Sessions() != s {
		t.Error("Sessions() not stable across calls")
	}
	alice := fp(1)
	key := ServeKey{Identity: alice, Workspace: testWorkspaceName}
	_, _ = m.SubscribeServe(key, make(chan *PendingAttach, 1))
	_, _ = m.SubscribeSandbox("sb", alice, make(chan *PendingAttach, 1))
	if _, err := m.RequestAttach(alice, key, "sb"); err != nil {
		t.Fatalf("RequestAttach: %v", err)
	}
	if got := m.Sessions().Count(); got != 1 {
		t.Errorf("Sessions().Count() = %d, want 1", got)
	}
}

// recv pulls one event off ch or fails the test if nothing
// arrives quickly.
func recv(t *testing.T, ch <-chan *PendingAttach) *PendingAttach {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for attach event")
		return nil
	}
}
