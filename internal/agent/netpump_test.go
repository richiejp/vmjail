package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// writeFramed writes a StdioProtocol frame (u16-le size + body) to w.
func writeFramed(t *testing.T, w io.Writer, body []byte) {
	t.Helper()
	hdr := make([]byte, 2)
	binary.LittleEndian.PutUint16(hdr, uint16(len(body))) //nolint:gosec // test payloads bounded by maxNetFrameSize.
	if _, err := w.Write(append(hdr, body...)); err != nil {
		t.Fatalf("write framed: %v", err)
	}
}

// readFramed reads one StdioProtocol frame from r within timeout.
func readFramed(t *testing.T, r net.Conn, timeout time.Duration) []byte {
	t.Helper()
	if err := r.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(r, hdr); err != nil {
		t.Fatalf("read header: %v", err)
	}
	size := binary.LittleEndian.Uint16(hdr)
	if size == 0 || size > maxNetFrameSize {
		t.Fatalf("bad size %d", size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read body: %v", err)
	}
	return buf
}

func TestPumpFramesConnToTap(t *testing.T) {
	connHost, connAgent := net.Pipe()
	tapHost, tapAgent := net.Pipe()
	t.Cleanup(func() {
		_ = connHost.Close()
		_ = tapHost.Close()
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() {
		pumpFrames(ctx, connAgent, tapAgent)
		close(done)
	}()

	frame := []byte("hello-ethernet-frame")
	writeFramed(t, connHost, frame)

	if err := tapHost.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	got := make([]byte, len(frame))
	if _, err := io.ReadFull(tapHost, got); err != nil {
		t.Fatalf("read tap: %v", err)
	}
	if !bytes.Equal(got, frame) {
		t.Fatalf("tap saw %q, want %q", got, frame)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpFrames did not exit after cancel")
	}
}

func TestPumpFramesTapToConn(t *testing.T) {
	connHost, connAgent := net.Pipe()
	tapHost, tapAgent := net.Pipe()
	t.Cleanup(func() {
		_ = connHost.Close()
		_ = tapHost.Close()
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan struct{})
	go func() {
		pumpFrames(ctx, connAgent, tapAgent)
		close(done)
	}()

	raw := []byte("\x00\x11\x22\x33\x44\x55raw-eth-payload")
	if _, err := tapHost.Write(raw); err != nil {
		t.Fatalf("tap host write: %v", err)
	}
	got := readFramed(t, connHost, 2*time.Second)
	if !bytes.Equal(got, raw) {
		t.Fatalf("framed = %q, want %q", got, raw)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpFrames did not exit after cancel")
	}
}

func TestPumpFramesExitsOnConnClose(t *testing.T) {
	connHost, connAgent := net.Pipe()
	tapHost, tapAgent := net.Pipe()
	t.Cleanup(func() {
		_ = tapHost.Close()
	})

	done := make(chan struct{})
	go func() {
		pumpFrames(t.Context(), connAgent, tapAgent)
		close(done)
	}()

	if err := connHost.Close(); err != nil {
		t.Fatalf("close conn: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpFrames did not exit after conn close")
	}
}

func TestPumpFramesRejectsOversizedFrame(t *testing.T) {
	connHost, connAgent := net.Pipe()
	tapHost, tapAgent := net.Pipe()
	t.Cleanup(func() {
		_ = connHost.Close()
		_ = tapHost.Close()
	})

	done := make(chan struct{})
	go func() {
		pumpFrames(t.Context(), connAgent, tapAgent)
		close(done)
	}()

	// Oversize header that would allocate gigabytes if honored.
	hdr := make([]byte, 2)
	binary.LittleEndian.PutUint16(hdr, maxNetFrameSize+1)
	// Pump may have exited mid-write — that's also a valid response.
	_, _ = connHost.Write(hdr)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pumpFrames did not exit on oversized header")
	}
}

func TestPumpConnToTapStopsOnShortHeader(t *testing.T) {
	conn := newCloseReader(bytes.NewReader([]byte{0x05}))
	var tap closeBuffer
	pumpConnToTap(conn, &tap)
	if tap.Len() != 0 {
		t.Fatalf("tap got %d bytes, want 0", tap.Len())
	}
}

func TestPumpConnToTapStopsOnZeroSize(t *testing.T) {
	conn := newCloseReader(bytes.NewReader([]byte{0x00, 0x00, 0xff}))
	var tap closeBuffer
	pumpConnToTap(conn, &tap)
	if tap.Len() != 0 {
		t.Fatalf("tap got %d bytes, want 0", tap.Len())
	}
}

func TestPumpConnToTapStopsOnTapWriteError(t *testing.T) {
	buf := make([]byte, 0, 6)
	buf = append(buf, 0x04, 0x00, 0x01, 0x02, 0x03, 0x04)
	conn := newCloseReader(bytes.NewReader(buf))
	tap := &errorWriter{err: io.ErrShortWrite}
	pumpConnToTap(conn, tap)
}

func TestPumpTapToConnStopsOnConnWriteError(t *testing.T) {
	tap := newCloseReader(bytes.NewReader([]byte{0xde, 0xad}))
	conn := &errorWriter{err: io.ErrShortWrite}
	pumpTapToConn(tap, conn)
}

// closeBuffer is a bytes.Buffer that also satisfies io.Closer so
// the pump's defer-close path has something it can call into.
type closeBuffer struct {
	bytes.Buffer
	closed bool
	mu     sync.Mutex
}

func (c *closeBuffer) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// closeReader wraps a bytes.Reader so it satisfies the writer
// surface in tests (writes are silently dropped) without standing
// up a full net.Pipe pair when the test only exercises one direction.
type closeReader struct {
	r io.Reader
}

func newCloseReader(r io.Reader) *closeReader { return &closeReader{r: r} }

func (c *closeReader) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *closeReader) Write(p []byte) (int, error) { return len(p), nil }
func (c *closeReader) Close() error                { return nil }

// errorWriter forces the pump's Write call to fail so the
// write-error exit path runs deterministically.
type errorWriter struct{ err error }

func (e *errorWriter) Read(p []byte) (int, error)  { return 0, io.EOF }
func (e *errorWriter) Write(p []byte) (int, error) { return 0, e.err }
func (e *errorWriter) Close() error                { return nil }
