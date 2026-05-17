// Package sandboxattach is the sandbox-host's attach client. It
// dials the in-VM daft-agent's attach listener over a fresh
// AF_VSOCK connection, sends the AgentAttachHello, and adapts the
// length-prefixed bidi frame stream to the
// sandboxgateway.AttachSession surface so the orchestrator-driven
// AttachStream RPC can splice through transparently.
//
// The companion package is internal/agent — it ships the
// AgentAttachHello / AgentAttachFrame protos and the matching
// framing helpers; this package reuses them rather than
// duplicating the wire encoding.
package sandboxattach

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/richiejp/darkaifactory/internal/agent"
	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
)

// OpenSession sends the AgentAttachHello derived from req onto
// conn, then returns an AttachSession that frames Data over the
// same conn and proxies Resize via the Resize side-channel.
//
// On any error the conn is closed before the function returns —
// the caller can treat OpenSession atomically (success ⇒ keep
// conn alive via the returned Session; failure ⇒ nothing to clean
// up).
func OpenSession(_ context.Context, conn net.Conn, req sandboxgateway.AttachRequest) (sandboxgateway.AttachSession, error) {
	hello := &agentv1.AgentAttachHello{
		Command: req.Command,
		Tty:     req.TTY,
		Cols:    req.Cols,
		Rows:    req.Rows,
	}
	if err := agent.WriteAttachHello(conn, hello); err != nil {
		oops.Skip("sandboxattach: close conn on hello err", conn.Close(), net.ErrClosed)
		return nil, fmt.Errorf("sandboxattach: send AgentAttachHello: %w", err)
	}
	return newAgentSession(conn), nil
}

// agentSession wraps a framed vsock conn as an AttachSession.
//
//   - Read: returns Data-frame payload bytes, buffering any
//     unread tail of the last frame for subsequent calls.
//   - Write: wraps b in an AgentAttachFrame{Data} and writes it.
//   - Resize: wraps cols/rows in an AgentAttachFrame{Resize} and
//     writes it.
//   - Close: closes the underlying conn (idempotent).
//
// Read serialises with itself; Write/Resize serialise via a
// separate write mutex so a Resize never interleaves a partial
// Data frame.
type agentSession struct {
	conn net.Conn

	readBuf []byte

	writeMu sync.Mutex

	closeOnce sync.Once
	closeErr  error
}

func newAgentSession(conn net.Conn) *agentSession {
	return &agentSession{conn: conn}
}

// Read returns Data-frame bytes from the agent. A short read
// (fewer bytes than the frame carries) buffers the tail for the
// next Read call so callers using a small []byte don't lose data.
// Non-Data frames (e.g. the agent's terminal Exit) collapse the
// session: Exit translates to io.EOF.
func (s *agentSession) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if len(s.readBuf) > 0 {
		n := copy(b, s.readBuf)
		s.readBuf = s.readBuf[n:]
		return n, nil
	}
	// Loop because off-spec agent → host Resize frames are dropped
	// (a confused peer mustn't kill the session); keep reading
	// until we see Data, Exit, or an error.
	//daft:lint:disable=forloopbound reason="bounded by agent.ReadAttachFrame returning io.EOF / net.ErrClosed on close"
	for {
		frame, err := agent.ReadAttachFrame(s.conn)
		if err != nil {
			return 0, err
		}
		switch body := frame.GetBody().(type) {
		case *agentv1.AgentAttachFrame_Data:
			n := copy(b, body.Data)
			if n < len(body.Data) {
				s.readBuf = append([]byte(nil), body.Data[n:]...)
			}
			return n, nil
		case *agentv1.AgentAttachFrame_Exit:
			return 0, io.EOF
		case *agentv1.AgentAttachFrame_Resize:
			continue
		default:
			return 0, errors.New("sandboxattach: agent sent frame with no body")
		}
	}
}

// Write wraps b in a Data frame and sends it to the agent.
func (s *agentSession) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	frame := &agentv1.AgentAttachFrame{
		Body: &agentv1.AgentAttachFrame_Data{Data: b},
	}
	if err := agent.WriteAttachFrame(s.conn, frame); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Resize sends a Resize frame to the agent. Errors propagate so
// the caller can decide whether to tear the session down.
func (s *agentSession) Resize(cols, rows uint32) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	frame := &agentv1.AgentAttachFrame{
		Body: &agentv1.AgentAttachFrame_Resize{
			Resize: &agentv1.AgentAttachResize{Cols: cols, Rows: rows},
		},
	}
	if err := agent.WriteAttachFrame(s.conn, frame); err != nil {
		return fmt.Errorf("sandboxattach: send Resize: %w", err)
	}
	return nil
}

// Close releases the underlying conn. Idempotent.
func (s *agentSession) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.conn.Close()
	})
	return s.closeErr
}

// LocalAddr / RemoteAddr / SetDeadline delegate to the underlying
// conn so AttachSession satisfies net.Conn.
func (s *agentSession) LocalAddr() net.Addr                { return s.conn.LocalAddr() }
func (s *agentSession) RemoteAddr() net.Addr               { return s.conn.RemoteAddr() }
func (s *agentSession) SetDeadline(t time.Time) error      { return s.conn.SetDeadline(t) }
func (s *agentSession) SetReadDeadline(t time.Time) error  { return s.conn.SetReadDeadline(t) }
func (s *agentSession) SetWriteDeadline(t time.Time) error { return s.conn.SetWriteDeadline(t) }
