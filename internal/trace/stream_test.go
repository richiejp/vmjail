package trace

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	tracev1 "github.com/richiejp/darkaifactory/internal/genpb/trace/v1"
)

// TestStreamRoundTripOverUDS exercises StreamExporter and
// StreamReceiver against each other across a real Unix-domain
// socket. Same wire shape as the production vsock path: 4-byte
// length prefix, then ReportSpansRequest bytes, one batch per
// conn.
func TestStreamRoundTripOverUDS(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "trace.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	sinkTracer := New("sink")
	recv := NewStreamReceiver(listener, sinkTracer)
	runCtx, runCancel := context.WithCancel(context.Background())
	t.Cleanup(runCancel)
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- recv.Run(runCtx) }()

	exporter := NewStreamExporter(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	})

	traceID := strings.Repeat("a", 32)
	spans := []*tracev1.Span{
		{
			TraceId:       traceID,
			SpanId:        strings.Repeat("b", 16),
			Name:          "remote.op",
			Component:     "test-producer",
			StartUnixNano: time.Now().UnixNano(),
			EndUnixNano:   time.Now().Add(time.Millisecond).UnixNano(),
		},
	}
	if err := exporter.Export(context.Background(), spans); err != nil {
		t.Fatalf("Export: %v", err)
	}

	// Receiver writes asynchronously; poll until the span lands.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snap := sinkTracer.Snapshot(); len(snap) == 1 {
			if snap[0].TraceID() != traceID {
				t.Errorf("trace_id = %q, want %q", snap[0].TraceID(), traceID)
			}
			runCancel()
			<-runErrCh
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("span never landed in sink Tracer")
}

// TestStreamExporterEmptyBatchIsNoOp confirms Export skips the dial
// path on an empty span slice, matching ConnectExporter's behaviour.
// We pass a dial that explodes — if it ever runs, the test fails.
func TestStreamExporterEmptyBatchIsNoOp(t *testing.T) {
	exporter := NewStreamExporter(func(context.Context) (net.Conn, error) {
		t.Fatal("dial should not be called on empty batch")
		return nil, errors.New("unreachable")
	})
	if err := exporter.Export(context.Background(), nil); err != nil {
		t.Errorf("Export(nil) = %v, want nil", err)
	}
	if err := exporter.Export(context.Background(), []*tracev1.Span{}); err != nil {
		t.Errorf("Export(empty) = %v, want nil", err)
	}
}

// TestStreamExporterDialError surfaces the dial failure verbatim.
func TestStreamExporterDialError(t *testing.T) {
	wantErr := errors.New("synthetic dial error")
	exporter := NewStreamExporter(func(context.Context) (net.Conn, error) {
		return nil, wantErr
	})
	err := exporter.Export(context.Background(), []*tracev1.Span{{TraceId: "x"}})
	if !errors.Is(err, wantErr) {
		t.Errorf("Export error = %v, want wraps %v", err, wantErr)
	}
}

// TestReadFramedBatchRejectsOversize protects the receiver against
// a malicious / corrupt producer sending a length prefix larger
// than our cap. Without this, the receiver would allocate up to
// 4 GiB before discovering the producer is lying.
func TestReadFramedBatchRejectsOversize(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], streamMaxMessage+1)
	_, err := ReadFramedBatch(bytesReader(hdr[:]))
	if err == nil {
		t.Fatal("expected oversize rejection")
	}
	if !strings.Contains(err.Error(), "> cap") {
		t.Errorf("error = %q, want contains > cap", err.Error())
	}
}

// TestReadFramedBatchZeroLength accepts a zero-length frame as a
// no-op (returns nil spans, no error). Useful for producer-side
// keepalives if we ever add them.
func TestReadFramedBatchZeroLength(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 0)
	spans, err := ReadFramedBatch(bytesReader(hdr[:]))
	if err != nil {
		t.Errorf("zero-length frame: %v", err)
	}
	if spans != nil {
		t.Errorf("spans = %v, want nil", spans)
	}
}

// TestReadFramedBatchTruncated covers the case where the prefix
// promises N bytes but the stream ends early.
func TestReadFramedBatchTruncated(t *testing.T) {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 100)
	r := bytesReader(append(hdr[:], 1, 2, 3)) // 3 bytes < promised 100
	_, err := ReadFramedBatch(r)
	if err == nil {
		t.Fatal("expected truncation error")
	}
}

// TestReadFramedBatchBadProto exercises the proto.Unmarshal error
// branch — feed valid framing but garbage payload.
func TestReadFramedBatchBadProto(t *testing.T) {
	garbage := []byte{0xff, 0xff, 0xff, 0xff, 0xff} // not a valid ReportSpansRequest
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(garbage))) //nolint:gosec // test fixture, length is a known small literal
	r := bytesReader(append(hdr[:], garbage...))
	_, err := ReadFramedBatch(r)
	if err == nil {
		t.Fatal("expected unmarshal error")
	}
}

// TestStreamReceiverRunStopsOnContextCancel asserts Run exits when
// ctx is cancelled, returning ctx.Err().
func TestStreamReceiverRunStopsOnContextCancel(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "trace.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	recv := NewStreamReceiver(listener, New("sink"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- recv.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on ctx cancel")
	}
}

// failWriteConn wraps a net.Conn so the first Write succeeds but
// the second errors — exercises the payload-write error branch
// (length prefix goes through, payload doesn't).
type failWriteConn struct {
	net.Conn
	calls int
}

func (c *failWriteConn) Write(p []byte) (int, error) {
	c.calls++
	if c.calls == 1 {
		return c.Conn.Write(p)
	}
	return 0, errors.New("synthetic write failure")
}

// TestStreamExporterPropagatesPayloadWriteError feeds a conn whose
// second Write errors. The Export must surface the wrapped error.
func TestStreamExporterPropagatesPayloadWriteError(t *testing.T) {
	hostSide, agentSide := net.Pipe()
	t.Cleanup(func() { _ = hostSide.Close() })
	// Drain the prefix so the agent's first Write doesn't block.
	go func() {
		buf := make([]byte, 4)
		_, _ = io.ReadFull(hostSide, buf)
	}()

	exporter := NewStreamExporter(func(context.Context) (net.Conn, error) {
		return &failWriteConn{Conn: agentSide}, nil
	})
	err := exporter.Export(context.Background(), []*tracev1.Span{{TraceId: "x"}})
	if err == nil || !strings.Contains(err.Error(), "write payload") {
		t.Errorf("err = %v, want write-payload error", err)
	}
}

// TestStreamExporterRejectsOversizePayload feeds spans whose
// serialised size exceeds streamMaxMessage. The exporter must
// refuse before dialing — protects the receiver too, since they
// share the same cap.
func TestStreamExporterRejectsOversizePayload(t *testing.T) {
	huge := strings.Repeat("a", streamMaxMessage)
	spans := []*tracev1.Span{
		{TraceId: strings.Repeat("a", 32), SpanId: strings.Repeat("b", 16), Name: huge},
	}
	exporter := NewStreamExporter(func(context.Context) (net.Conn, error) {
		t.Fatal("dial should not be called when payload exceeds cap")
		return nil, errors.New("unreachable")
	})
	err := exporter.Export(context.Background(), spans)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want oversize rejection", err)
	}
}

// TestStreamReceiverDropsParkedConnection confirms that a producer
// who opens a connection and never writes doesn't pin a receiver
// goroutine indefinitely. The handle path must apply a read
// deadline so SetReadDeadline-bound producers get reaped within
// streamReadTimeout — we shrink the constant via a brief override
// to keep the test fast.
func TestStreamReceiverDropsParkedConnection(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "trace.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	recv := NewStreamReceiver(listener, New("sink"))
	go func() { _ = recv.Run(context.Background()) }()
	t.Cleanup(func() { _ = recv.Close() })

	// Override the read deadline so the test doesn't wait 30 s.
	savedTimeout := streamReadTimeoutForTest
	streamReadTimeoutForTest = 100 * time.Millisecond
	t.Cleanup(func() { streamReadTimeoutForTest = savedTimeout })

	parked, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = parked.Close() })

	// Wait until the receiver closes the conn (it sets a read
	// deadline and EOFs once it fires); Read returns io.EOF.
	buf := make([]byte, 1)
	if err := parked.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline on producer side: %v", err)
	}
	_, err = parked.Read(buf)
	if err == nil {
		t.Fatal("parked connection still open after read deadline")
	}
}

// TestStreamReceiverHandlesMalformedConnection makes sure a
// connection that closes after sending garbage doesn't take down
// the receiver. We send a single bad connection then a good one
// and confirm the good one's spans land.
func TestStreamReceiverHandlesMalformedConnection(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "trace.sock")
	listener, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	tracer := New("sink")
	recv := NewStreamReceiver(listener, tracer)
	go func() { _ = recv.Run(context.Background()) }()
	t.Cleanup(func() { _ = recv.Close() })

	// Garbage: half a length prefix then EOF.
	bad, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("Dial bad: %v", err)
	}
	if _, err := bad.Write([]byte{0x00, 0x01}); err != nil {
		t.Fatalf("Write bad: %v", err)
	}
	_ = bad.Close()

	// Good: framed proto.
	good, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("Dial good: %v", err)
	}
	payload, err := proto.Marshal(&tracev1.ReportSpansRequest{
		Spans: []*tracev1.Span{{TraceId: strings.Repeat("c", 32), SpanId: strings.Repeat("d", 16), Name: "ok"}},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload))) //nolint:gosec // test fixture, length is a known small literal
	if _, err := good.Write(hdr[:]); err != nil {
		t.Fatalf("Write hdr: %v", err)
	}
	if _, err := good.Write(payload); err != nil {
		t.Fatalf("Write payload: %v", err)
	}
	_ = good.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(tracer.Snapshot()) == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("good span never landed after bad connection")
}

// bytesReader returns an io.Reader over b — used in framing tests
// to feed exact byte sequences without spinning up a socket.
func bytesReader(b []byte) io.Reader {
	return &byteSliceReader{b: b}
}

type byteSliceReader struct{ b []byte }

func (r *byteSliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
