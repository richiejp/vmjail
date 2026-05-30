package sandboxnet

import (
	"net"
	"sync/atomic"
)

// Counters is a snapshot of a Network's wire-level byte traffic at
// the moment of capture. Returned by (*Network).Counters as a
// value-copy of the live atomics so callers can compare two
// snapshots without racing the live counters.
//
// Direction is from the host's point of view, i.e. from sandboxd
// looking at the vsock conn:
//
//   - RxBytes counts bytes the guest sent to sandboxd (frames going
//     INTO the netstack switch from the wire).
//   - TxBytes counts bytes sandboxd sent to the guest (frames the
//     switch wrote out to the wire).
//
// Both numbers include the 2-byte StdioProtocol size prefix on
// every frame — the overhead is bounded (≤0.13% at MTU 1500) and
// keeping it makes the counters mechanical and cheap. Callers who
// want app-layer bytes can subtract `2 * frame_count` once we add
// frame counters.
type Counters struct {
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
}

// counters holds the live atomic byte counters embedded in a
// Network. Methods are pointer-receiver so the atomics stay
// stable across copies.
type counters struct {
	rx atomic.Uint64
	tx atomic.Uint64
}

func (c *counters) snapshot() Counters {
	return Counters{RxBytes: c.rx.Load(), TxBytes: c.tx.Load()}
}

// countingConn wraps a net.Conn and atomically accumulates bytes
// read and written into the parent Network's counters. All other
// methods delegate verbatim to the wrapped conn, so timeouts and
// addresses still reflect the underlying socket.
//
// The wrapper is opaque to gvisor-tap-vsock's switch: it sees an
// ordinary net.Conn, which is what tap.Switch.Accept asks for.
type countingConn struct {
	net.Conn
	c *counters
}

func (cc *countingConn) Read(p []byte) (int, error) {
	n, err := cc.Conn.Read(p)
	if n > 0 {
		cc.c.rx.Add(uint64(n)) //nolint:gosec // G115: n is non-negative per io.Reader contract.
	}
	return n, err
}

func (cc *countingConn) Write(p []byte) (int, error) {
	n, err := cc.Conn.Write(p)
	if n > 0 {
		cc.c.tx.Add(uint64(n)) //nolint:gosec // G115: n is non-negative per io.Writer contract.
	}
	return n, err
}
