//go:build linux

package agent

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// TestDialHostVsockWrapsDialErrors swaps vsockDialer for a stub
// that errors and confirms the wrapper preserves the underlying
// message (so log-grepping for "vsock dial" remains stable).
func TestDialHostVsockWrapsDialErrors(t *testing.T) {
	saved := vsockDialer
	t.Cleanup(func() { vsockDialer = saved })

	wantErr := errors.New("synthetic dial fail")
	vsockDialer = func(port uint32) (net.Conn, error) {
		if port != 8000 {
			t.Errorf("port = %d, want 8000", port)
		}
		return nil, wantErr
	}

	_, err := DialHostVsock(context.Background(), 8000)
	if !errors.Is(err, wantErr) {
		t.Errorf("error = %v, want wraps %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "vsock dial") {
		t.Errorf("error message %q lost the prefix", err.Error())
	}
}

// TestDialHostVsockAppliesContextDeadline verifies that a ctx with
// a deadline propagates to the returned conn (SetDeadline is what
// causes Read/Write to fail after the deadline; we observe it by
// checking the conn's behaviour rather than introspecting internals).
func TestDialHostVsockAppliesContextDeadline(t *testing.T) {
	saved := vsockDialer
	t.Cleanup(func() { vsockDialer = saved })

	hostSide, agentSide := net.Pipe()
	t.Cleanup(func() { _ = hostSide.Close() })
	vsockDialer = func(uint32) (net.Conn, error) { return agentSide, nil }

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(50*time.Millisecond))
	defer cancel()
	conn, err := DialHostVsock(ctx, 8000)
	if err != nil {
		t.Fatalf("DialHostVsock: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// Wait past the deadline; Read should fail with an i/o
	// timeout (or similar) rather than blocking forever.
	time.Sleep(80 * time.Millisecond)
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err == nil {
		t.Error("Read past deadline returned nil err; deadline not applied")
	}
}

// TestDialHostVsockSetDeadlineErrorClosesConn covers the rare
// branch where the dialed conn rejects SetDeadline. The Dial
// wrapper must close the conn it just opened to avoid a leak.
func TestDialHostVsockSetDeadlineErrorClosesConn(t *testing.T) {
	saved := vsockDialer
	t.Cleanup(func() { vsockDialer = saved })

	closeFlag := false
	vsockDialer = func(uint32) (net.Conn, error) {
		return &failSetDeadlineConn{onClose: func() { closeFlag = true }}, nil
	}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Hour))
	defer cancel()
	_, err := DialHostVsock(ctx, 8000)
	if err == nil {
		t.Fatal("expected SetDeadline error to surface")
	}
	if !closeFlag {
		t.Error("conn not closed after SetDeadline error — leak")
	}
}

// failSetDeadlineConn is a net.Conn whose SetDeadline always
// errors. Used to exercise the cleanup branch of DialHostVsock.
type failSetDeadlineConn struct {
	onClose func()
}

func (c *failSetDeadlineConn) Read([]byte) (int, error)  { return 0, errors.New("nope") }
func (c *failSetDeadlineConn) Write([]byte) (int, error) { return 0, errors.New("nope") }
func (c *failSetDeadlineConn) LocalAddr() net.Addr       { return &net.UnixAddr{} }
func (c *failSetDeadlineConn) RemoteAddr() net.Addr      { return &net.UnixAddr{} }
func (c *failSetDeadlineConn) SetDeadline(time.Time) error {
	return errors.New("set-deadline forbidden")
}
func (c *failSetDeadlineConn) SetReadDeadline(time.Time) error  { return nil }
func (c *failSetDeadlineConn) SetWriteDeadline(time.Time) error { return nil }
func (c *failSetDeadlineConn) Close() error {
	if c.onClose != nil {
		c.onClose()
	}
	return nil
}
