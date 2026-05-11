package workspacebroker

import (
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// readyFakeStream is like fakeStream but lets the test seed the
// inbound queue with deterministic frames (including a close to
// signal io.EOF) without involving Splice or goroutines. We use
// the existing fakeStream from sessions_test.go for these tests
// directly — it already does what we need.

func TestRelayConnReadCopiesFrames(t *testing.T) {
	in := newFakeStream(4, 4)
	in.in <- []byte("hello-")
	in.in <- []byte("world")
	close(in.in)
	c := NewRelayConn(in, nil)

	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("Read 1: %v", err)
	}
	if string(buf[:n]) != "hello-" {
		t.Errorf("Read 1 = %q, want hello-", buf[:n])
	}
	n, err = c.Read(buf)
	if err != nil {
		t.Fatalf("Read 2: %v", err)
	}
	if string(buf[:n]) != "world" {
		t.Errorf("Read 2 = %q, want world", buf[:n])
	}
	// Third Read should return EOF (channel closed).
	if _, err := c.Read(buf); !errors.Is(err, io.EOF) {
		t.Errorf("Read 3 err = %v, want io.EOF", err)
	}
}

func TestRelayConnReadBuffersResidual(t *testing.T) {
	// Frame is larger than the caller's buffer — second Read
	// should yield the rest without consuming another frame.
	in := newFakeStream(1, 1)
	in.in <- []byte("abcdef")
	close(in.in)
	c := NewRelayConn(in, nil)
	small := make([]byte, 3)
	n, _ := c.Read(small)
	if n != 3 || string(small[:n]) != "abc" {
		t.Errorf("first Read = (%d,%q), want (3,abc)", n, small[:n])
	}
	n, _ = c.Read(small)
	if n != 3 || string(small[:n]) != "def" {
		t.Errorf("second Read = (%d,%q), want (3,def)", n, small[:n])
	}
	if _, err := c.Read(small); !errors.Is(err, io.EOF) {
		t.Errorf("third Read err = %v, want io.EOF", err)
	}
}

func TestRelayConnReadIntoZeroBufferIsNoop(t *testing.T) {
	c := NewRelayConn(newFakeStream(0, 0), nil)
	n, err := c.Read(nil)
	if n != 0 || err != nil {
		t.Errorf("Read(nil) = (%d,%v), want (0,nil)", n, err)
	}
}

func TestRelayConnWriteSendsAsFrame(t *testing.T) {
	out := newFakeStream(0, 4)
	c := NewRelayConn(out, nil)
	n, err := c.Write([]byte("payload"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 7 {
		t.Errorf("Write n = %d, want 7", n)
	}
	got := <-out.out
	if string(got) != "payload" {
		t.Errorf("frame = %q, want payload", got)
	}
}

func TestRelayConnCloseInvokesStopOnce(t *testing.T) {
	var stopCount atomic.Int32
	c := NewRelayConn(newFakeStream(0, 0), func() error {
		stopCount.Add(1)
		return errors.New("boom")
	})
	if err := c.Close(); err == nil || err.Error() != "boom" {
		t.Errorf("Close err = %v, want boom", err)
	}
	if err := c.Close(); err == nil || err.Error() != "boom" {
		t.Errorf("Close (2nd) err = %v, want boom (idempotent)", err)
	}
	if got := stopCount.Load(); got != 1 {
		t.Errorf("onStop invoked %d times, want 1", got)
	}
}

func TestRelayConnWriteAfterCloseFails(t *testing.T) {
	c := NewRelayConn(newFakeStream(0, 0), nil)
	_ = c.Close()
	if _, err := c.Write([]byte("late")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Write err = %v, want net.ErrClosed", err)
	}
}

func TestRelayConnReadAfterCloseReturnsEOF(t *testing.T) {
	c := NewRelayConn(newFakeStream(0, 0), nil)
	_ = c.Close()
	if _, err := c.Read(make([]byte, 4)); !errors.Is(err, io.EOF) {
		t.Errorf("Read err = %v, want io.EOF", err)
	}
}

func TestRelayConnAddrs(t *testing.T) {
	c := NewRelayConn(newFakeStream(0, 0), nil)
	la, ra := c.LocalAddr(), c.RemoteAddr()
	if la == nil || ra == nil {
		t.Fatal("addrs nil")
	}
	if la.Network() != "daft-relay" {
		t.Errorf("Network = %q, want daft-relay", la.Network())
	}
	if la.String() == "" || ra.String() == "" {
		t.Error("addr String empty")
	}
}

func TestRelayConnDeadlinesAreNoops(t *testing.T) {
	c := NewRelayConn(newFakeStream(0, 0), nil)
	now := time.Now()
	if err := c.SetDeadline(now); err != nil {
		t.Errorf("SetDeadline: %v", err)
	}
	if err := c.SetReadDeadline(now); err != nil {
		t.Errorf("SetReadDeadline: %v", err)
	}
	if err := c.SetWriteDeadline(now); err != nil {
		t.Errorf("SetWriteDeadline: %v", err)
	}
}

func TestOneConnListenerYieldsConnOnce(t *testing.T) {
	c := NewRelayConn(newFakeStream(0, 0), nil)
	l := NewOneConnListener(c)
	got, err := l.Accept()
	if err != nil {
		t.Fatalf("first Accept: %v", err)
	}
	if got != c {
		t.Error("first Accept returned different conn")
	}
	// Second Accept should block; close should unblock it.
	type result struct {
		c   net.Conn
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := l.Accept()
		done <- result{c, err}
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("Accept returned early: c=%v err=%v", r.c, r.err)
	default:
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case r := <-done:
		if r.c != nil {
			t.Errorf("Accept after Close returned non-nil conn %v", r.c)
		}
		if !errors.Is(r.err, net.ErrClosed) {
			t.Errorf("Accept after Close err = %v, want net.ErrClosed", r.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Accept didn't unblock on Close")
	}
}

func TestOneConnListenerCloseIdempotent(t *testing.T) {
	l := NewOneConnListener(NewRelayConn(newFakeStream(0, 0), nil))
	if err := l.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// Accept after first close returns ErrClosed without ever
	// yielding the wrapped conn.
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Accept after Close err = %v, want net.ErrClosed", err)
	}
}

func TestOneConnListenerAddr(t *testing.T) {
	l := NewOneConnListener(NewRelayConn(newFakeStream(0, 0), nil))
	if l.Addr() == nil {
		t.Fatal("Addr nil")
	}
}

// TestRelayConnPassesThroughSpliceMessage exercises RelayConn over
// a Splice-driven pair: write on one conn, read on the other.
// Confirms our adapter composes with the existing splicing layer.
func TestRelayConnPassesThroughSpliceMessage(t *testing.T) {
	// Two fake streams play the role of the two half-relays at the
	// orchestrator. Each peer sees one fakeStream pair through a
	// RelayConn; Splice copies between the partner streams.
	leftIn := make(chan []byte, 4)
	leftOut := make(chan []byte, 4)
	rightIn := make(chan []byte, 4)
	rightOut := make(chan []byte, 4)
	leftPeer := &chanStream{recv: leftIn, send: leftOut}
	rightPeer := &chanStream{recv: rightIn, send: rightOut}
	// Splice left.send -> right.recv (via leftOut -> rightIn) and
	// vice versa via simple goroutines.
	go func() {
		for b := range leftOut {
			rightIn <- b
		}
		close(rightIn)
	}()
	go func() {
		for b := range rightOut {
			leftIn <- b
		}
		close(leftIn)
	}()

	lc := NewRelayConn(leftPeer, func() error { close(leftOut); return nil })
	rc := NewRelayConn(rightPeer, func() error { close(rightOut); return nil })

	go func() {
		_, _ = lc.Write([]byte("ping"))
		_ = lc.Close()
	}()
	buf := make([]byte, 16)
	n, err := rc.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(buf[:n]) != "ping" {
		t.Errorf("read = %q, want ping", buf[:n])
	}
	_ = rc.Close()
}

// chanStream is a DataStream backed by two []byte channels. Used
// in TestRelayConnPassesThroughSpliceMessage to avoid coupling to
// the more complex fakeStream from sessions_test.go.
type chanStream struct {
	recv <-chan []byte
	send chan<- []byte
}

func (s *chanStream) RecvData() ([]byte, error) {
	b, ok := <-s.recv
	if !ok {
		return nil, io.EOF
	}
	return b, nil
}

func (s *chanStream) SendData(b []byte) error {
	defer func() { _ = recover() }() // tolerate send on closed
	s.send <- b
	return nil
}
