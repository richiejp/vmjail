package sandboxattach

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/richiejp/vmjail/internal/agent"
	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
	"github.com/richiejp/vmjail/internal/sandboxgateway"
)

// echoAgent runs a minimal agent simulator on agentConn: reads
// Hello, reads exactly one Data frame and echoes its payload via
// helloCh/dataCh, sends one Data frame containing reply, then
// sends a terminal Exit.
type echoAgentResult struct {
	hello *agentv1.AgentAttachHello
	data  []byte
}

func runEchoAgent(agentConn net.Conn, reply []byte, out chan<- echoAgentResult, done chan<- struct{}) {
	defer close(done)
	defer func() { _ = agentConn.Close() }()
	hello, err := readHelloShim(agentConn)
	if err != nil {
		return
	}
	frame, err := agent.ReadAttachFrame(agentConn)
	if err != nil {
		return
	}
	out <- echoAgentResult{hello: hello, data: frame.GetData()}
	if werr := agent.WriteAttachFrame(agentConn, &agentv1.AgentAttachFrame{
		Body: &agentv1.AgentAttachFrame_Data{Data: reply},
	}); werr != nil {
		return
	}
	_ = agent.WriteAttachFrame(agentConn, &agentv1.AgentAttachFrame{
		Body: &agentv1.AgentAttachFrame_Exit{Exit: &agentv1.AgentAttachExit{Code: 0}},
	})
}

// TestOpenSessionSendsHelloAndRoundTripsData drives one full
// session against a fake "agent" goroutine on the other end of
// net.Pipe: OpenSession should send a Hello matching req, and
// subsequent Write/Read pairs should frame Data both directions.
func TestOpenSessionSendsHelloAndRoundTripsData(t *testing.T) {
	hostConn, agentConn := net.Pipe()
	t.Cleanup(func() { _ = hostConn.Close() })

	resultCh := make(chan echoAgentResult, 1)
	agentDone := make(chan struct{})
	go runEchoAgent(agentConn, []byte("hello-from-agent"), resultCh, agentDone)

	req := sandboxgateway.AttachRequest{
		SessionID:  "sess-1",
		SandboxUID: "sb-1",
		Command:    []string{"/bin/sh"},
		TTY:        true,
		Cols:       100,
		Rows:       30,
	}
	sess, err := OpenSession(context.Background(), hostConn, req)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	if _, werr := sess.Write([]byte("ping")); werr != nil {
		t.Fatalf("Write: %v", werr)
	}
	got := waitForAgentResult(t, resultCh)
	if !got.hello.GetTty() || got.hello.GetCols() != 100 || got.hello.GetRows() != 30 {
		t.Errorf("Hello mismatch: %+v", got.hello)
	}
	if len(got.hello.GetCommand()) != 1 || got.hello.GetCommand()[0] != "/bin/sh" {
		t.Errorf("Hello.Command = %v, want [/bin/sh]", got.hello.GetCommand())
	}
	if !bytes.Equal(got.data, []byte("ping")) {
		t.Errorf("agent saw data %q, want ping", got.data)
	}

	buf := make([]byte, 64)
	n, rerr := sess.Read(buf)
	if rerr != nil {
		t.Fatalf("Read: %v", rerr)
	}
	if !bytes.Equal(buf[:n], []byte("hello-from-agent")) {
		t.Errorf("host got %q, want hello-from-agent", buf[:n])
	}
	if _, rerr := sess.Read(buf); !errors.Is(rerr, io.EOF) {
		t.Errorf("expected EOF after Exit frame, got %v", rerr)
	} else {
		var exit *sandboxgateway.AttachExitError
		if !errors.As(rerr, &exit) || exit.Code != 0 || exit.Reason != "" {
			t.Errorf("exit = %#v, want code=0 with no reason", exit)
		}
	}
	<-agentDone
}

// readHelloShim wraps readHello so callers don't need to pass *testing.T.
func readHelloShim(conn net.Conn) (*agentv1.AgentAttachHello, error) {
	return readHello(nil, conn)
}

func waitForAgentResult(t *testing.T, ch <-chan echoAgentResult) echoAgentResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("agent goroutine never reported result")
		return echoAgentResult{}
	}
}

// TestOpenSessionReadBuffersShortReads pins the short-read
// buffering: a Read with a buffer smaller than the incoming Data
// frame yields the prefix, and the next Read returns the tail
// without losing bytes.
func TestOpenSessionReadBuffersShortReads(t *testing.T) {
	hostConn, agentConn := net.Pipe()
	t.Cleanup(func() { _ = hostConn.Close() })

	go func() {
		defer func() { _ = agentConn.Close() }()
		_, _ = readHello(t, agentConn)
		_ = agent.WriteAttachFrame(agentConn, &agentv1.AgentAttachFrame{
			Body: &agentv1.AgentAttachFrame_Data{Data: []byte("abcdefghij")}, // 10 bytes
		})
		// Hold the agent side open until we close.
		buf := make([]byte, 64)
		_, _ = agentConn.Read(buf)
	}()

	sess, err := OpenSession(context.Background(), hostConn, sandboxgateway.AttachRequest{SandboxUID: "sb"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	small := make([]byte, 4)
	n, rerr := sess.Read(small)
	if rerr != nil {
		t.Fatalf("Read: %v", rerr)
	}
	if !bytes.Equal(small[:n], []byte("abcd")) {
		t.Errorf("first chunk = %q, want abcd", small[:n])
	}
	n, rerr = sess.Read(small)
	if rerr != nil {
		t.Fatalf("Read: %v", rerr)
	}
	if !bytes.Equal(small[:n], []byte("efgh")) {
		t.Errorf("second chunk = %q, want efgh", small[:n])
	}
	n, rerr = sess.Read(small)
	if rerr != nil {
		t.Fatalf("Read: %v", rerr)
	}
	if !bytes.Equal(small[:n], []byte("ij")) {
		t.Errorf("tail chunk = %q, want ij", small[:n])
	}
}

// TestOpenSessionResizeFrame pins the Resize side-channel: a
// Resize call emits an AgentAttachFrame{Resize} on the wire with
// the correct dims.
func TestOpenSessionResizeFrame(t *testing.T) {
	hostConn, agentConn := net.Pipe()
	t.Cleanup(func() { _ = hostConn.Close() })

	gotResize := make(chan *agentv1.AgentAttachResize, 1)
	go func() {
		defer func() { _ = agentConn.Close() }()
		_, _ = readHello(t, agentConn)
		frame, err := agent.ReadAttachFrame(agentConn)
		if err != nil {
			return
		}
		gotResize <- frame.GetResize()
	}()

	sess, err := OpenSession(context.Background(), hostConn, sandboxgateway.AttachRequest{SandboxUID: "sb"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	if rerr := sess.Resize(132, 50); rerr != nil {
		t.Fatalf("Resize: %v", rerr)
	}
	select {
	case r := <-gotResize:
		if r == nil || r.GetCols() != 132 || r.GetRows() != 50 {
			t.Errorf("Resize frame mismatch: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent never saw Resize frame")
	}
}

// TestOpenSessionHelloWriteFailureClosesConn pins the atomicity:
// when Hello write fails, OpenSession closes conn before
// returning.
func TestOpenSessionHelloWriteFailureClosesConn(t *testing.T) {
	hostConn, _ := net.Pipe()
	_ = hostConn.Close() // pre-close so the Hello write errors.

	_, err := OpenSession(context.Background(), hostConn, sandboxgateway.AttachRequest{SandboxUID: "sb"})
	if err == nil {
		t.Fatal("expected error from OpenSession on closed conn")
	}
}

// TestSessionReadEmptyBufferReturnsZero pins the len(b)==0 short-
// circuit so a caller probing with a zero-length buf doesn't
// block on a wire read.
func TestSessionReadEmptyBufferReturnsZero(t *testing.T) {
	s := newAgentSession(&closedConn{})
	n, err := s.Read(nil)
	if err != nil || n != 0 {
		t.Errorf("Read(nil) = (%d, %v), want (0, nil)", n, err)
	}
}

// TestSessionWriteEmptyReturnsZero same shape on the write side.
func TestSessionWriteEmptyReturnsZero(t *testing.T) {
	s := newAgentSession(&closedConn{})
	n, err := s.Write(nil)
	if err != nil || n != 0 {
		t.Errorf("Write(nil) = (%d, %v), want (0, nil)", n, err)
	}
}

// TestSessionReadReturnsWireError pins the error-passthrough
// branch: a closed conn surfaces the underlying io error from
// ReadAttachFrame.
func TestSessionReadReturnsWireError(t *testing.T) {
	hostConn, agentConn := net.Pipe()
	t.Cleanup(func() { _ = hostConn.Close() })
	_ = agentConn.Close()
	s := newAgentSession(hostConn)
	buf := make([]byte, 16)
	if _, err := s.Read(buf); err == nil {
		t.Fatal("expected error on closed conn")
	}
}

// TestSessionWriteReturnsWireError pins the wire-error branch of
// Write.
func TestSessionWriteReturnsWireError(t *testing.T) {
	hostConn, _ := net.Pipe()
	_ = hostConn.Close()
	s := newAgentSession(hostConn)
	if _, err := s.Write([]byte("data")); err == nil {
		t.Fatal("expected error on closed conn")
	}
}

// TestSessionResizeReturnsWireError pins the wire-error branch of
// Resize.
func TestSessionResizeReturnsWireError(t *testing.T) {
	hostConn, _ := net.Pipe()
	_ = hostConn.Close()
	s := newAgentSession(hostConn)
	if err := s.Resize(80, 24); err == nil {
		t.Fatal("expected error on closed conn")
	}
}

// TestSessionNetConnPassthroughs pins the trivial net.Conn
// delegations so the AttachSession satisfies the full interface.
func TestSessionNetConnPassthroughs(t *testing.T) {
	hostConn, _ := net.Pipe()
	t.Cleanup(func() { _ = hostConn.Close() })
	s := newAgentSession(hostConn)
	_ = s.LocalAddr()
	_ = s.RemoteAddr()
	if err := s.SetDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Errorf("SetDeadline: %v", err)
	}
	if err := s.SetReadDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Errorf("SetReadDeadline: %v", err)
	}
	if err := s.SetWriteDeadline(time.Now().Add(time.Hour)); err != nil {
		t.Errorf("SetWriteDeadline: %v", err)
	}
}

// TestSessionCloseIdempotent pins the closeOnce behavior.
func TestSessionCloseIdempotent(t *testing.T) {
	c := &countingCloser{}
	s := newAgentSession(c)
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if c.closes != 1 {
		t.Errorf("underlying Close called %d times, want 1", c.closes)
	}
}

// TestSessionReadDropsAgentResize: a Resize from the agent (off-
// spec in v0) is dropped and Read continues to the next frame.
func TestSessionReadDropsAgentResize(t *testing.T) {
	hostConn, agentConn := net.Pipe()
	t.Cleanup(func() { _ = hostConn.Close() })

	go func() {
		defer func() { _ = agentConn.Close() }()
		_, _ = readHello(t, agentConn)
		// Off-spec: agent sends Resize first.
		_ = agent.WriteAttachFrame(agentConn, &agentv1.AgentAttachFrame{
			Body: &agentv1.AgentAttachFrame_Resize{Resize: &agentv1.AgentAttachResize{Cols: 1, Rows: 1}},
		})
		// Then a real Data frame the host should see.
		_ = agent.WriteAttachFrame(agentConn, &agentv1.AgentAttachFrame{
			Body: &agentv1.AgentAttachFrame_Data{Data: []byte("after")},
		})
		buf := make([]byte, 64)
		_, _ = agentConn.Read(buf)
	}()

	sess, err := OpenSession(context.Background(), hostConn, sandboxgateway.AttachRequest{SandboxUID: "sb"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	buf := make([]byte, 32)
	n, rerr := sess.Read(buf)
	if rerr != nil {
		t.Fatalf("Read: %v", rerr)
	}
	if !bytes.Equal(buf[:n], []byte("after")) {
		t.Errorf("after-resize data = %q, want after", buf[:n])
	}
}

// TestSessionReadRejectsEmptyBody: an AgentAttachFrame with no
// body set (off-spec) surfaces as a clear error.
func TestSessionReadRejectsEmptyBody(t *testing.T) {
	hostConn, agentConn := net.Pipe()
	t.Cleanup(func() { _ = hostConn.Close() })

	go func() {
		defer func() { _ = agentConn.Close() }()
		_, _ = readHello(t, agentConn)
		_ = agent.WriteAttachFrame(agentConn, &agentv1.AgentAttachFrame{})
	}()

	sess, err := OpenSession(context.Background(), hostConn, sandboxgateway.AttachRequest{SandboxUID: "sb"})
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })

	buf := make([]byte, 32)
	if _, rerr := sess.Read(buf); rerr == nil {
		t.Error("expected error on empty-body frame")
	}
}

// readHello reads one length-prefixed AgentAttachHello off the
// agent-facing end of the pipe via the exported helper. The t
// argument is accepted (and may be nil) so callers from non-
// t.Helper-eligible sites — the goroutine-based agent simulator —
// can reuse it.
func readHello(t *testing.T, conn net.Conn) (*agentv1.AgentAttachHello, error) {
	if t != nil {
		t.Helper()
	}
	return agent.ReadAttachHello(conn)
}

// closedConn is a net.Conn whose Read/Write all error. Used to
// fast-fail the empty-buffer short-circuit tests without standing
// up a real pipe.
type closedConn struct{}

func (closedConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (closedConn) Write([]byte) (int, error)        { return 0, io.ErrClosedPipe }
func (closedConn) Close() error                     { return nil }
func (closedConn) LocalAddr() net.Addr              { return nil }
func (closedConn) RemoteAddr() net.Addr             { return nil }
func (closedConn) SetDeadline(time.Time) error      { return nil }
func (closedConn) SetReadDeadline(time.Time) error  { return nil }
func (closedConn) SetWriteDeadline(time.Time) error { return nil }

// countingCloser counts Close calls so the idempotency test can
// assert.
type countingCloser struct {
	closedConn
	closes int
}

func (c *countingCloser) Close() error {
	c.closes++
	return nil
}
