package trace

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	tracev1 "github.com/richiejp/darkaifactory/internal/genpb/trace/v1"
)

// streamMaxMessage caps a single framed batch at 4 MiB. Generous
// enough for a normal Flush (default ring buffer is 1k spans @
// ~400 bytes each = 400 KiB) but bounded so a malformed length
// prefix can't make the receiver allocate gigabytes.
const streamMaxMessage = 4 * 1024 * 1024

// streamLenPrefix is the fixed 4-byte big-endian length prefix that
// precedes each serialised ReportSpansRequest on the wire. We don't
// use HTTP/2 here because the in-VM agent ships spans over a
// hybrid-vsock UDS and we don't want the connect-go + net/http stack
// in the static-musl agent binary just to send one message per
// Flush. Binary framing keeps the agent under 2 MiB.
const streamLenPrefix = 4

// streamReadTimeoutForTest caps how long the receiver waits for
// one framed batch on a connection. A parked producer (crashed
// mid-handshake, open but never writing) must not be able to pin
// a receiver goroutine indefinitely. Variable rather than constant
// so the parked-connection test can shrink it to keep CI fast.
//
//nolint:gochecknoglobals // test-hook timeout; mutated only by the parked-connection test.
var streamReadTimeoutForTest = 30 * time.Second

// StreamExporter is an Exporter that, on each Export call, opens a
// fresh net.Conn via Dial, writes a single length-prefixed
// ReportSpansRequest, and closes the conn. Stateless on purpose:
// a producer that crashes between batches doesn't leak a connection
// in the receiver's accept loop.
//
// Transport-agnostic: Dial can target AF_VSOCK (in-VM agent →
// sandbox-host), UDS (tests), or TCP (out-of-VM producers if we
// ever want one). The exporter only cares that the returned
// net.Conn carries a clean byte stream.
type StreamExporter struct {
	dial func(ctx context.Context) (net.Conn, error)
}

// NewStreamExporter wires an exporter that uses dial for each
// Export call. dial must be safe to invoke concurrently; the
// returned conn is owned by the exporter and closed when the
// write finishes.
func NewStreamExporter(dial func(ctx context.Context) (net.Conn, error)) *StreamExporter {
	return &StreamExporter{dial: dial}
}

// Export marshals spans into one ReportSpansRequest, writes the
// length prefix + payload over a fresh connection, and returns.
// No-op (and nil error) on an empty batch — same convention as
// ConnectExporter so Tracer.Flush doesn't dial just to send zero
// bytes.
func (e *StreamExporter) Export(ctx context.Context, spans []*tracev1.Span) error {
	if len(spans) == 0 {
		return nil
	}
	payload, err := proto.Marshal(&tracev1.ReportSpansRequest{Spans: spans})
	if err != nil {
		return fmt.Errorf("trace: marshal ReportSpansRequest: %w", err)
	}
	if len(payload) > streamMaxMessage {
		return fmt.Errorf("trace: span batch %d bytes exceeds %d cap", len(payload), streamMaxMessage)
	}
	conn, err := e.dial(ctx)
	if err != nil {
		return fmt.Errorf("trace: dial: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(deadline)
	}
	var hdr [streamLenPrefix]byte
	// len(payload) was just checked against streamMaxMessage above
	// (4 MiB), well within uint32 range — gosec's overflow check is
	// the conservative one.
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload))) //nolint:gosec // G115: bounded by streamMaxMessage above
	if _, err := conn.Write(hdr[:]); err != nil {
		return fmt.Errorf("trace: write length prefix: %w", err)
	}
	if _, err := conn.Write(payload); err != nil {
		return fmt.Errorf("trace: write payload: %w", err)
	}
	return nil
}

// StreamReceiver runs an accept loop on listener, reads one
// length-prefixed ReportSpansRequest per connection, and ingests
// the spans into tracer. The same protocol as StreamExporter
// emits.
//
// One framed batch per connection lets us treat partial reads /
// dead connections / malformed frames as "drop this conn and
// move on" — there is no in-band recovery channel.
type StreamReceiver struct {
	listener net.Listener
	tracer   *Tracer

	mu      sync.Mutex
	stopped bool
}

// NewStreamReceiver wraps listener + tracer. Caller owns the
// listener and may also close it; Run exits on either Close() or
// listener-close.
func NewStreamReceiver(listener net.Listener, tracer *Tracer) *StreamReceiver {
	return &StreamReceiver{listener: listener, tracer: tracer}
}

// Run blocks accepting connections until the listener is closed
// (returns nil) or ctx is cancelled (closes the listener and
// returns ctx.Err()).
func (r *StreamReceiver) Run(ctx context.Context) error {
	doneCtx := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = r.Close()
		case <-doneCtx:
		}
	}()
	defer close(doneCtx)

	//daft:lint:disable=forloopbound reason="exits when r.listener.Close drives Accept to return net.ErrClosed"
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			if r.isStopped() || errors.Is(err, net.ErrClosed) {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				return nil
			}
			return fmt.Errorf("trace: accept: %w", err)
		}
		go r.handle(conn)
	}
}

// Close stops the receiver. Idempotent.
func (r *StreamReceiver) Close() error {
	r.mu.Lock()
	already := r.stopped
	r.stopped = true
	r.mu.Unlock()
	if already {
		return nil
	}
	return r.listener.Close()
}

func (r *StreamReceiver) isStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

func (r *StreamReceiver) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(streamReadTimeoutForTest))
	spans, err := ReadFramedBatch(conn)
	if err != nil {
		// A misbehaving or crashed-mid-write producer must not take
		// the receiver down or fill logs.
		return
	}
	r.tracer.IngestProto(spans)
}

// ReadFramedBatch reads one length prefix + payload from r and
// returns the decoded spans. Production callers use StreamReceiver;
// this is the seam tests and out-of-tree consumers reach for when
// they only need the framing.
func ReadFramedBatch(r io.Reader) ([]*tracev1.Span, error) {
	var hdr [streamLenPrefix]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("trace: read length prefix: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return nil, nil
	}
	if n > streamMaxMessage {
		return nil, fmt.Errorf("trace: framed batch length %d > cap %d", n, streamMaxMessage)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("trace: read payload: %w", err)
	}
	var req tracev1.ReportSpansRequest
	if err := proto.Unmarshal(buf, &req); err != nil {
		return nil, fmt.Errorf("trace: unmarshal ReportSpansRequest: %w", err)
	}
	return req.GetSpans(), nil
}
