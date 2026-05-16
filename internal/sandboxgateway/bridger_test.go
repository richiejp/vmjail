package sandboxgateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

const (
	testWorkspace = "ws-aaaa-bbbb"
	testPort      = uint32(8080)
)

func dialAlways(conn net.Conn) DialFunc {
	return func(context.Context) (net.Conn, error) { return conn, nil }
}

// TestRegisterAndBridgeInvokesDialOnce pins the basic Register/Bridge
// loop: the dial fn is invoked exactly once per Bridge call.
func TestRegisterAndBridgeInvokesDialOnce(t *testing.T) {
	b := New()
	calls := 0
	dsLocal, dsRemote := net.Pipe()
	t.Cleanup(func() { _ = dsLocal.Close() })
	dial := func(context.Context) (net.Conn, error) {
		calls++
		return dsRemote, nil
	}
	if err := b.Register(testWorkspace, testPort, dial); err != nil {
		t.Fatalf("Register: %v", err)
	}
	upLocal, upRemote := net.Pipe()
	t.Cleanup(func() { _ = upLocal.Close() })
	bridgeDone := make(chan error, 1)
	go func() {
		bridgeDone <- b.Bridge(context.Background(), testWorkspace, testPort, upRemote)
	}()
	// Close upLocal so the up→ds direction sees EOF; close dsLocal
	// so the ds→up direction sees EOF; Bridge then returns.
	_ = upLocal.Close()
	_ = dsLocal.Close()
	select {
	case err := <-bridgeDone:
		if err != nil {
			t.Fatalf("Bridge: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Bridge didn't return within 2s")
	}
	if calls != 1 {
		t.Errorf("dial called %d times, want 1", calls)
	}
}

// TestBridgePumpsBothDirections drives upstream → downstream and
// downstream → upstream end-to-end through Bridge.
func TestBridgePumpsBothDirections(t *testing.T) {
	b := New()
	dsLocal, dsRemote := net.Pipe()
	if err := b.Register(testWorkspace, testPort, dialAlways(dsRemote)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	upLocal, upRemote := net.Pipe()
	t.Cleanup(func() { _ = upLocal.Close() })
	t.Cleanup(func() { _ = dsLocal.Close() })

	bridgeDone := make(chan struct{})
	go func() {
		defer close(bridgeDone)
		_ = b.Bridge(context.Background(), testWorkspace, testPort, upRemote)
	}()

	// up → ds
	want := []byte("hello downstream")
	go func() { _, _ = upLocal.Write(want) }()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(dsLocal, got); err != nil {
		t.Fatalf("read ds: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("ds got %q, want %q", got, want)
	}

	// ds → up
	reply := []byte("hello upstream")
	go func() { _, _ = dsLocal.Write(reply) }()
	gotReply := make([]byte, len(reply))
	if _, err := io.ReadFull(upLocal, gotReply); err != nil {
		t.Fatalf("read up: %v", err)
	}
	if !bytes.Equal(gotReply, reply) {
		t.Errorf("up got %q, want %q", gotReply, reply)
	}

	// Drain. Closing one side triggers EOF on the pump.
	_ = dsLocal.Close()
	_ = upLocal.Close()
	select {
	case <-bridgeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Bridge didn't return within 2s after closes")
	}
}

// TestBridgeReturnsErrNoRoute pins the missing-route branch.
func TestBridgeReturnsErrNoRoute(t *testing.T) {
	b := New()
	upA, upB := net.Pipe()
	t.Cleanup(func() { _ = upA.Close() })
	t.Cleanup(func() { _ = upB.Close() })
	err := b.Bridge(context.Background(), "missing", 12345, upA)
	if !errors.Is(err, ErrNoRoute) {
		t.Errorf("err = %v, want ErrNoRoute", err)
	}
}

// TestRegisterRejectsDuplicate pins the bug-detection branch.
func TestRegisterRejectsDuplicate(t *testing.T) {
	b := New()
	dial := func(context.Context) (net.Conn, error) { return nil, errors.New("unreachable") }
	if err := b.Register(testWorkspace, testPort, dial); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := b.Register(testWorkspace, testPort, dial)
	if !errors.Is(err, ErrRouteExists) {
		t.Errorf("err = %v, want ErrRouteExists", err)
	}
}

// TestRegisterRejectsBadInput covers the input-validation branches:
// empty workspace, zero port, nil dial.
func TestRegisterRejectsBadInput(t *testing.T) {
	b := New()
	dial := func(context.Context) (net.Conn, error) { return nil, errors.New("unreachable") }
	for _, tc := range []struct {
		name  string
		ws    string
		port  uint32
		dial  DialFunc
		match string
	}{
		{"empty workspace", "", testPort, dial, "empty workspaceUUID"},
		{"zero port", testWorkspace, 0, dial, "zero port"},
		{"nil dial", testWorkspace, testPort, nil, "nil dial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := b.Register(tc.ws, tc.port, tc.dial)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// TestUnregisterIsIdempotent pins the cleanup-path expectation: the
// executor's terminal-phase Unregister must succeed even if Register
// was never called (a Sandbox that failed to dispatch).
func TestUnregisterIsIdempotent(t *testing.T) {
	b := New()
	// Should not panic / not error.
	b.Unregister(testWorkspace, testPort)
	b.Unregister(testWorkspace, testPort)
}

// TestUnregisterRemovesRoute pins that a subsequent Bridge after
// Unregister returns ErrNoRoute.
func TestUnregisterRemovesRoute(t *testing.T) {
	b := New()
	dial := func(context.Context) (net.Conn, error) { return nil, errors.New("unreachable") }
	if err := b.Register(testWorkspace, testPort, dial); err != nil {
		t.Fatalf("Register: %v", err)
	}
	b.Unregister(testWorkspace, testPort)
	upA, _ := net.Pipe()
	t.Cleanup(func() { _ = upA.Close() })
	err := b.Bridge(context.Background(), testWorkspace, testPort, upA)
	if !errors.Is(err, ErrNoRoute) {
		t.Errorf("err = %v, want ErrNoRoute after Unregister", err)
	}
}

// TestBridgePropagatesDialError pins the failed-dial branch: the
// DialFunc's error surfaces wrapped in the Bridge return.
func TestBridgePropagatesDialError(t *testing.T) {
	b := New()
	want := errors.New("synthetic dial failure")
	dial := func(context.Context) (net.Conn, error) { return nil, want }
	if err := b.Register(testWorkspace, testPort, dial); err != nil {
		t.Fatalf("Register: %v", err)
	}
	upA, upB := net.Pipe()
	t.Cleanup(func() { _ = upA.Close() })
	t.Cleanup(func() { _ = upB.Close() })
	err := b.Bridge(context.Background(), testWorkspace, testPort, upA)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestBridgeCancelTearsDownPumps pins the ctx-driven teardown: a
// cancel mid-stream closes both ends and the pump returns. Without
// this guard a hung downstream socket would outlive the orchestrator
// request that opened it.
func TestBridgeCancelTearsDownPumps(t *testing.T) {
	b := New()
	dsLocal, dsRemote := net.Pipe()
	if err := b.Register(testWorkspace, testPort, dialAlways(dsRemote)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { _ = dsLocal.Close() })
	upLocal, upRemote := net.Pipe()
	t.Cleanup(func() { _ = upLocal.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.Bridge(ctx, testWorkspace, testPort, upRemote)
	}()
	// Give the pump goroutines a moment to settle into io.Copy.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Bridge didn't return within 2s of cancel")
	}
}

// TestConcurrentRegisterAndBridge sanity-checks the RWMutex: many
// concurrent Bridges on registered routes plus a concurrent
// Register on a fresh route don't deadlock or race.
func TestConcurrentRegisterAndBridge(t *testing.T) {
	b := New()
	dsLocal, dsRemote := net.Pipe()
	t.Cleanup(func() { _ = dsLocal.Close() })
	go func() { _, _ = io.Copy(io.Discard, dsLocal) }()
	if err := b.Register(testWorkspace, testPort, dialAlways(dsRemote)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	var wg sync.WaitGroup
	const N = 8
	for range N {
		wg.Go(func() {
			upA, upB := net.Pipe()
			defer func() { _ = upA.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			go func() { _ = upA.Close() }()
			_ = b.Bridge(ctx, testWorkspace, testPort, upB)
		})
	}
	// Concurrent Register on a different key — must not block.
	dial := func(context.Context) (net.Conn, error) { return nil, errors.New("unreachable") }
	if err := b.Register("other-ws", 9999, dial); err != nil {
		t.Errorf("concurrent Register on disjoint key: %v", err)
	}
	wg.Wait()
}
