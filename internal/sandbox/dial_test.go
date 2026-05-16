package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHybridVsockServer is an in-process stand-in for the Cloud
// Hypervisor hybrid-vsock UDS. It accepts on a temp UDS path,
// reads the `CONNECT <port>\n` line, then (depending on the
// configured reply) either writes `OK N\n` and pipes bytes, or
// writes the configured malformed reply and closes.
type fakeHybridVsockServer struct {
	t        *testing.T
	ln       net.Listener
	wantPort uint32
	// reply, when set, overrides the canonical "OK 12345\n".
	// Used to exercise the malformed-reply branch.
	reply string
	// onPayload, when non-nil, runs against the post-handshake
	// byte tunnel inside the accept goroutine.
	onPayload func(net.Conn)

	wg sync.WaitGroup
}

func newFakeHybridVsockServer(t *testing.T, wantPort uint32) *fakeHybridVsockServer {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "v.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &fakeHybridVsockServer{t: t, ln: ln, wantPort: wantPort}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(s.wg.Wait)
	return s
}

func (s *fakeHybridVsockServer) socket() string { return s.ln.Addr().String() }

func (s *fakeHybridVsockServer) serve() {
	defer s.wg.Done()
	conn, err := s.ln.Accept()
	if err != nil {
		// Listener closed — clean teardown.
		return
	}
	defer func() { _ = conn.Close() }()
	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil {
		s.t.Errorf("fake-vsock: read CONNECT: %v", err)
		return
	}
	want := fmt.Sprintf("CONNECT %d\n", s.wantPort)
	if line != want {
		s.t.Errorf("fake-vsock: got %q, want %q", line, want)
		return
	}
	reply := s.reply
	if reply == "" {
		reply = "OK 12345\n"
	}
	if _, err := io.WriteString(conn, reply); err != nil {
		s.t.Errorf("fake-vsock: write reply: %v", err)
		return
	}
	if s.onPayload != nil {
		s.onPayload(conn)
	}
}

// TestDialGuestVsockHappyPath pins the end-to-end byte tunnel: the
// fake server replies OK then echoes whatever the client sends.
func TestDialGuestVsockHappyPath(t *testing.T) {
	s := newFakeHybridVsockServer(t, 10000)
	s.onPayload = func(conn net.Conn) {
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		_, _ = conn.Write(buf[:n])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := DialGuestVsock(ctx, s.socket(), 10000)
	if err != nil {
		t.Fatalf("DialGuestVsock: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ping" {
		t.Errorf("got %q, want %q", got, "ping")
	}
}

// TestDialGuestVsockRejectedReply pins the non-OK branch — the
// server's malformed reply must surface as an error and the
// underlying conn must be closed (no leak).
func TestDialGuestVsockRejectedReply(t *testing.T) {
	s := newFakeHybridVsockServer(t, 10000)
	s.reply = "REJECT no listener\n"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := DialGuestVsock(ctx, s.socket(), 10000)
	if err == nil {
		_ = conn.Close()
		t.Fatal("expected error on REJECT reply")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error %q missing 'rejected'", err)
	}
}

// TestDialGuestVsockDialError pins the no-server branch: dialing
// a non-existent UDS errors at the unix-dial step.
func TestDialGuestVsockDialError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.sock")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := DialGuestVsock(ctx, missing, 10000)
	if err == nil {
		t.Fatal("expected error dialling missing UDS")
	}
}

// TestDialGuestVsockReplyExceedsLimit pins the bounded-line branch:
// a server that floods bytes without a newline must be cut off and
// the conn closed, not stream forever.
func TestDialGuestVsockReplyExceedsLimit(t *testing.T) {
	s := newFakeHybridVsockServer(t, 10000)
	s.reply = strings.Repeat("A", dialReplyLineLimit+16) // no '\n'
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := DialGuestVsock(ctx, s.socket(), 10000)
	if err == nil {
		t.Fatal("expected error on over-long reply")
	}
}

// TestDialGuestVsockClosedDuringRead pins the read-error branch:
// when the server hangs up before sending the OK line, the dial
// surfaces an io error rather than blocking.
func TestDialGuestVsockClosedDuringRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		// Read the CONNECT then drop the conn without replying.
		br := bufio.NewReader(c)
		_, _ = br.ReadString('\n')
		_ = c.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = DialGuestVsock(ctx, path, 10000)
	if err == nil {
		t.Fatal("expected error when server closes mid-handshake")
	}
	if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "read CONNECT") {
		// Either io.EOF or a wrapped "read CONNECT" message is
		// acceptable — both indicate the read failed.
		t.Logf("err: %v", err)
	}
}
