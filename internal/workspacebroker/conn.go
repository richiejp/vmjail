package workspacebroker

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// RelayConn adapts a DataStream as a net.Conn so callers can run
// arbitrary stream-oriented protocols (mTLS, HTTP/2, …) over a
// brokered Relay channel. Deadlines are no-ops in v1 — the
// orchestrator's session TTL and the caller's context handle most
// timeouts. Read/Write are independently safe to call from one
// goroutine each.
//
// The zero value is not useful; build with NewRelayConn.
type RelayConn struct {
	ds     DataStream
	onStop func() error

	readMu  sync.Mutex
	readBuf []byte

	writeMu sync.Mutex

	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool
}

// NewRelayConn wraps ds as a net.Conn. onStop is invoked on the
// first Close; both half-closes of the underlying bidi stream
// should happen there. nil onStop is allowed (server-side Connect
// bidi streams close when the handler returns).
func NewRelayConn(ds DataStream, onStop func() error) *RelayConn {
	return &RelayConn{ds: ds, onStop: onStop}
}

// Read returns up to len(p) bytes. A single DataStream frame may
// be larger than p; the residual is buffered and served on
// subsequent Reads. Returns io.EOF when the underlying RecvData
// signals end-of-stream.
func (c *RelayConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if len(c.readBuf) == 0 {
		if c.closed.Load() {
			return 0, io.EOF
		}
		data, err := c.ds.RecvData()
		if err != nil {
			return 0, err
		}
		c.readBuf = data
	}
	n := copy(p, c.readBuf)
	c.readBuf = c.readBuf[n:]
	return n, nil
}

// Write sends p as a single DataStream frame. Returns
// net.ErrClosed if the conn has been closed.
func (c *RelayConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ds.SendData(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Close marks the conn as closed and invokes the onStop hook
// exactly once.
func (c *RelayConn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if c.onStop != nil {
			c.closeErr = c.onStop()
		}
	})
	return c.closeErr
}

// LocalAddr / RemoteAddr return a placeholder relayAddr. The
// underlying transport is a brokered session, not a TCP socket;
// upstream consumers (tls.Server, http2.Server) only need
// non-nil addrs.
func (c *RelayConn) LocalAddr() net.Addr  { return relayAddr("local") }
func (c *RelayConn) RemoteAddr() net.Addr { return relayAddr("remote") }

// SetDeadline / SetReadDeadline / SetWriteDeadline are no-ops in
// v1. The session TTL on the orchestrator and the caller's ctx
// govern lifetime; finer-grained per-Read/Write deadlines would
// require an interruptible RecvData, which Connect bidi streams
// don't natively support.
func (c *RelayConn) SetDeadline(_ time.Time) error      { return nil }
func (c *RelayConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *RelayConn) SetWriteDeadline(_ time.Time) error { return nil }

type relayAddr string

func (relayAddr) Network() string  { return "daft-relay" }
func (a relayAddr) String() string { return "daft-relay:" + string(a) }

// OneConnListener is a net.Listener that yields exactly one
// pre-built net.Conn and then blocks Accept forever (until Close).
// http.Server.Serve(l) over a single dialed connection needs
// this: the server side is the user-host serving its workspace
// on a connection it itself opened to the sandbox-host (through
// the broker). Close unblocks pending Accepts with net.ErrClosed.
type OneConnListener struct {
	mu     sync.Mutex
	conn   net.Conn
	done   chan struct{}
	closed bool
}

// NewOneConnListener returns a listener that will yield conn on
// its first Accept. Subsequent Accepts block until Close.
func NewOneConnListener(conn net.Conn) *OneConnListener {
	return &OneConnListener{conn: conn, done: make(chan struct{})}
}

// Accept returns the wrapped conn the first time, then blocks
// until Close.
func (l *OneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, net.ErrClosed
	}
	c := l.conn
	l.conn = nil
	l.mu.Unlock()
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

// Close unblocks pending Accepts. It does NOT close the conn the
// listener yielded — that's the http.Server's responsibility.
func (l *OneConnListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	close(l.done)
	return nil
}

// Addr returns a placeholder; upstream callers only check for nil.
func (l *OneConnListener) Addr() net.Addr { return relayAddr("listener") }
