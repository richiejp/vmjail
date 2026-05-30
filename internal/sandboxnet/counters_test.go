package sandboxnet

import (
	"io"
	"net"
	"testing"
)

func TestCountingConn_AccumulatesBytes(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()

	var ctrs counters
	cc := &countingConn{Conn: a, c: &ctrs}

	// Read side: peer writes 5 bytes; we read them through cc.
	go func() {
		_, _ = b.Write([]byte("hello"))
	}()
	buf := make([]byte, 5)
	n, err := io.ReadFull(cc, buf)
	if err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if n != 5 {
		t.Fatalf("n: got %d want 5", n)
	}
	if got := ctrs.snapshot(); got.RxBytes != 5 || got.TxBytes != 0 {
		t.Fatalf("snapshot after read: %+v", got)
	}

	// Write side: 7 bytes outbound through cc.
	done := make(chan struct{})
	go func() {
		defer close(done)
		sink := make([]byte, 7)
		_, _ = io.ReadFull(b, sink)
	}()
	wn, err := cc.Write([]byte("ABCDEFG"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if wn != 7 {
		t.Fatalf("Write n: got %d want 7", wn)
	}
	<-done
	if got := ctrs.snapshot(); got.RxBytes != 5 || got.TxBytes != 7 {
		t.Fatalf("snapshot after write: %+v", got)
	}
}

func TestCountingConn_ZeroReadDoesNotAdd(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()

	var ctrs counters
	cc := &countingConn{Conn: a, c: &ctrs}

	// Close the peer; the Read will return (0, EOF).
	if err := b.Close(); err != nil {
		t.Fatalf("close peer: %v", err)
	}
	buf := make([]byte, 4)
	n, err := cc.Read(buf)
	if err == nil {
		t.Fatalf("expected error on closed peer")
	}
	if n != 0 {
		t.Fatalf("n: got %d want 0", n)
	}
	if got := ctrs.snapshot(); got.RxBytes != 0 || got.TxBytes != 0 {
		t.Fatalf("snapshot: %+v", got)
	}
}

func TestNetworkCounters_StartsZero(t *testing.T) {
	n, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := n.Counters(); got.RxBytes != 0 || got.TxBytes != 0 {
		t.Fatalf("fresh Network Counters: %+v", got)
	}
}

func TestNetworkActive_FalseUntilAccept(t *testing.T) {
	n, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if n.Active() {
		t.Fatal("Active() true before any Accept")
	}

	// Drive one Accept with a conn that closes immediately. The
	// switch will error out of rxStream, but Active must already
	// have flipped to true before any I/O happens.
	a, b := net.Pipe()
	if err := b.Close(); err != nil {
		t.Fatalf("close peer: %v", err)
	}
	_ = n.Accept(t.Context(), a) // returns rx error, that's fine
	if !n.Active() {
		t.Fatal("Active() false after Accept")
	}
}
