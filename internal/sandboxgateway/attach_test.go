package sandboxgateway

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

const (
	testSandboxUID = "sb-abcd"
	noDialLabel    = "nil dial"
)

// fakeAttachSession satisfies AttachSession for the tests that
// only care about the registry routing — not the actual byte
// pump.
type fakeAttachSession struct{}

func (*fakeAttachSession) Read([]byte) (int, error)         { return 0, nil }
func (*fakeAttachSession) Write([]byte) (int, error)        { return 0, nil }
func (*fakeAttachSession) Close() error                     { return nil }
func (*fakeAttachSession) Resize(uint32, uint32) error      { return nil }
func (*fakeAttachSession) LocalAddr() net.Addr              { return nil }
func (*fakeAttachSession) RemoteAddr() net.Addr             { return nil }
func (*fakeAttachSession) SetDeadline(time.Time) error      { return nil }
func (*fakeAttachSession) SetReadDeadline(time.Time) error  { return nil }
func (*fakeAttachSession) SetWriteDeadline(time.Time) error { return nil }

// TestAttachRegistryRoutesByUID pins the happy path: a registered
// dialer is invoked exactly once on Attach for its uid.
func TestAttachRegistryRoutesByUID(t *testing.T) {
	r := NewAttachRegistry()
	calls := 0
	var gotReq AttachRequest
	dial := func(_ context.Context, req AttachRequest) (AttachSession, error) {
		calls++
		gotReq = req
		return &fakeAttachSession{}, nil
	}
	if err := r.Register(testSandboxUID, dial); err != nil {
		t.Fatalf("Register: %v", err)
	}
	sess, err := r.Attach(context.Background(), AttachRequest{
		SandboxUID: testSandboxUID,
		SessionID:  "s1",
		Command:    []string{"/bin/sh"},
		TTY:        true,
		Cols:       80,
		Rows:       24,
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	_ = sess.Close()
	if calls != 1 {
		t.Errorf("dial called %d times, want 1", calls)
	}
	if gotReq.SessionID != "s1" || gotReq.Cols != 80 {
		t.Errorf("dial received wrong req: %+v", gotReq)
	}
}

// TestAttachRegistryReturnsErrAttachNoRouteForUnknownUID pins the
// missing-route branch — the orchestrator-side handler maps this
// onto an HTTP error so the client sees a clear "sandbox not
// reachable" failure rather than a hung connection.
func TestAttachRegistryReturnsErrAttachNoRouteForUnknownUID(t *testing.T) {
	r := NewAttachRegistry()
	_, err := r.Attach(context.Background(), AttachRequest{SandboxUID: "nope"})
	if !errors.Is(err, ErrAttachNoRoute) {
		t.Errorf("err = %v, want ErrAttachNoRoute", err)
	}
}

// TestAttachRegistryRegisterRejectsDuplicate pins the bug-
// detection branch.
func TestAttachRegistryRegisterRejectsDuplicate(t *testing.T) {
	r := NewAttachRegistry()
	dial := func(context.Context, AttachRequest) (AttachSession, error) { return nil, errors.New("unreachable") }
	if err := r.Register(testSandboxUID, dial); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := r.Register(testSandboxUID, dial)
	if !errors.Is(err, ErrAttachRouteExists) {
		t.Errorf("err = %v, want ErrAttachRouteExists", err)
	}
}

// TestAttachRegistryRegisterRejectsBadInput covers the input-
// validation branches: empty uid, nil dial.
func TestAttachRegistryRegisterRejectsBadInput(t *testing.T) {
	r := NewAttachRegistry()
	dial := func(context.Context, AttachRequest) (AttachSession, error) { return nil, errors.New("unreachable") }
	for _, tc := range []struct {
		name  string
		uid   string
		dial  AttachDialer
		match string
	}{
		{"empty uid", "", dial, "empty sandboxUID"},
		{noDialLabel, testSandboxUID, nil, "nil dial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.Register(tc.uid, tc.dial); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// TestAttachRegistryUnregisterIsIdempotent pins the cleanup
// invariant: Unregister of a never-registered uid is a no-op.
func TestAttachRegistryUnregisterIsIdempotent(t *testing.T) {
	r := NewAttachRegistry()
	r.Unregister(testSandboxUID)
	r.Unregister(testSandboxUID)
}

// TestAttachRegistryUnregisterRemovesRoute pins that Attach after
// Unregister yields ErrAttachNoRoute.
func TestAttachRegistryUnregisterRemovesRoute(t *testing.T) {
	r := NewAttachRegistry()
	dial := func(context.Context, AttachRequest) (AttachSession, error) { return &fakeAttachSession{}, nil }
	if err := r.Register(testSandboxUID, dial); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r.Unregister(testSandboxUID)
	if _, err := r.Attach(context.Background(), AttachRequest{SandboxUID: testSandboxUID}); !errors.Is(err, ErrAttachNoRoute) {
		t.Errorf("err after Unregister = %v, want ErrAttachNoRoute", err)
	}
}
