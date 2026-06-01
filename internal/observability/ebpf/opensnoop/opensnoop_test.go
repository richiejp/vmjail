//go:build linux

package opensnoop

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
)

func arr16(s string) [16]uint8 {
	var a [16]uint8
	copy(a[:], s)
	return a
}

func arr255(s string) [255]uint8 {
	var a [255]uint8
	copy(a[:], s)
	return a
}

func recordBytes(t *testing.T, e opensnoopEvent) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, &e); err != nil {
		t.Fatalf("encode event: %v", err)
	}
	return buf.Bytes()
}

func TestParseEvent(t *testing.T) {
	t.Run("decodes a full record", func(t *testing.T) {
		raw := recordBytes(t, opensnoopEvent{
			Pid: 4321, Uid: 1000, Dfd: -100, Flags: 0x241,
			Comm: arr16("nix"), Filename: arr255("/nix/store/x"),
		})
		ev, err := parseEvent(raw)
		if err != nil {
			t.Fatalf("parseEvent: %v", err)
		}
		want := Event{PID: 4321, UID: 1000, DFD: -100, Flags: 0x241, Comm: "nix", Filename: "/nix/store/x"}
		if ev != want {
			t.Errorf("parseEvent = %+v, want %+v", ev, want)
		}
	})

	t.Run("rejects a short record", func(t *testing.T) {
		if _, err := parseEvent([]byte{1, 2, 3}); err == nil {
			t.Fatal("expected an error for a short record")
		}
	})

	t.Run("unterminated comm/filename keeps full width", func(t *testing.T) {
		full16 := strings.Repeat("a", 16)
		full255 := strings.Repeat("b", 255)
		raw := recordBytes(t, opensnoopEvent{Comm: arr16(full16), Filename: arr255(full255)})
		ev, err := parseEvent(raw)
		if err != nil {
			t.Fatalf("parseEvent: %v", err)
		}
		if ev.Comm != full16 || ev.Filename != full255 {
			t.Errorf("full-width fields truncated: comm=%q filename(len)=%d", ev.Comm, len(ev.Filename))
		}
	})
}

func TestOpenFlag(t *testing.T) {
	cases := []struct {
		flags int32
		want  string
	}{
		{0x0, "O_RDONLY"},
		{0x1, "O_WRONLY"},
		{0x2, "O_RDWR"},
		{0x41, "O_WRONLY|O_CREAT"},
		{0x241, "O_WRONLY|O_CREAT|O_TRUNC"},
		{0x80000, "O_RDONLY|O_CLOEXEC"},
		{0x80, "O_RDONLY|O_EXCL"},
		{0x400, "O_RDONLY|O_APPEND"},
		{0x800, "O_RDONLY|O_NONBLOCK"},
		{0x10000, "O_RDONLY|O_DIRECTORY"},
		// Every option bit at once, read-write.
		{
			0x2 | 0x40 | 0x80 | 0x200 | 0x400 | 0x800 | 0x10000 | 0x80000,
			"O_RDWR|O_CREAT|O_EXCL|O_TRUNC|O_APPEND|O_NONBLOCK|O_DIRECTORY|O_CLOEXEC",
		},
	}
	for _, c := range cases {
		if got := openFlag(c.flags); got != c.want {
			t.Errorf("openFlag(%#x) = %q, want %q", c.flags, got, c.want)
		}
	}
}

func TestEventString(t *testing.T) {
	e := Event{PID: 7, UID: 0, DFD: -100, Flags: 0x41, Comm: "crun", Filename: "/etc/hosts"}
	got := e.String()
	for _, sub := range []string{"pid=7", "uid=0", `comm="crun"`, "dfd=-100", "O_WRONLY|O_CREAT", "0x41", `path="/etc/hosts"`} {
		if !strings.Contains(got, sub) {
			t.Errorf("String()=%q missing %q", got, sub)
		}
	}
}

// fakeReader feeds canned records, then blocks on Read until Close, then
// reports ringbuf.ErrClosed — mimicking *ringbuf.Reader's cancel
// semantics so run's ctx-cancel path is exercised.
type fakeReader struct {
	recs    [][]byte
	i       int
	readErr error
	closed  chan struct{}
}

func newFakeReader(recs ...[]byte) *fakeReader {
	return &fakeReader{recs: recs, closed: make(chan struct{})}
}

func (f *fakeReader) Read() (ringbuf.Record, error) {
	if f.i < len(f.recs) {
		r := ringbuf.Record{RawSample: f.recs[f.i]}
		f.i++
		return r, nil
	}
	if f.readErr != nil {
		return ringbuf.Record{}, f.readErr
	}
	<-f.closed
	return ringbuf.Record{}, ringbuf.ErrClosed
}

func (f *fakeReader) Close() error {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
	return nil
}

func fakeOpener(r recordReader) opener {
	return func() (recordReader, func() error, error) {
		return r, func() error { return nil }, nil
	}
}

func TestRunWritesEventsThenStopsOnCancel(t *testing.T) {
	rec := recordBytes(t, opensnoopEvent{Pid: 1, Flags: 0x0, Comm: arr16("sh"), Filename: arr255("/lib")})
	fr := newFakeReader(rec)
	var out bytes.Buffer

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, &out, nil, fakeOpener(fr)) }()

	// Wait for the one event to be drained and written.
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), "/lib") {
		if time.Now().After(deadline) {
			t.Fatal("event never written")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("run after cancel = %v, want nil", err)
	}
	if !strings.Contains(out.String(), `path="/lib"`) {
		t.Errorf("output missing event: %q", out.String())
	}
}

func TestRunSkipsShortRecord(t *testing.T) {
	good := recordBytes(t, opensnoopEvent{Pid: 9, Comm: arr16("go"), Filename: arr255("/ok")})
	fr := newFakeReader([]byte{0, 1, 2}, good) // short then valid
	var out bytes.Buffer

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, &out, nil, fakeOpener(fr)) }()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), "/ok") {
		if time.Now().After(deadline) {
			t.Fatal("valid event after a short one never written")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("run = %v", err)
	}
}

func TestRunReturnsReadError(t *testing.T) {
	fr := newFakeReader()
	fr.readErr = errors.New("boom")
	if err := run(context.Background(), io.Discard, nil, fakeOpener(fr)); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("run = %v, want a wrapped read error", err)
	}
}

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestRunReturnsWriteError(t *testing.T) {
	rec := recordBytes(t, opensnoopEvent{Pid: 2, Comm: arr16("x"), Filename: arr255("/y")})
	fr := newFakeReader(rec)
	if err := run(context.Background(), errWriter{}, nil, fakeOpener(fr)); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("run = %v, want a wrapped write error", err)
	}
}

func TestRunReturnsOpenError(t *testing.T) {
	want := errors.New("no caps")
	open := func() (recordReader, func() error, error) { return nil, nil, want }
	if err := run(context.Background(), io.Discard, nil, open); !errors.Is(err, want) {
		t.Fatalf("run = %v, want %v", err, want)
	}
}

// TestRunAggregatingFeedsAggregator proves RunAggregating folds each decoded
// event into the aggregator while still writing the raw trace to w.
func TestRunAggregatingFeedsAggregator(t *testing.T) {
	rec := recordBytes(t, opensnoopEvent{Pid: 7, Comm: arr16("cc1"), Filename: arr255("/nix/store/abc/x.h")})
	fr := newFakeReader(rec)
	var out bytes.Buffer
	agg := NewAggregator()

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, &out, agg, fakeOpener(fr)) }()

	deadline := time.Now().Add(2 * time.Second)
	for agg.Snapshot().Total == 0 {
		if time.Now().After(deadline) {
			t.Fatal("aggregator never observed the event")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("run = %v", err)
	}
	s := agg.Snapshot()
	if s.Total == 0 || len(s.TopPaths) == 0 || s.TopPaths[0].Key != "/nix/store/abc/x.h" {
		t.Fatalf("aggregator not fed correctly: %+v", s)
	}
	if !strings.Contains(out.String(), `path="/nix/store/abc/x.h"`) {
		t.Fatalf("raw trace not written alongside aggregation: %q", out.String())
	}
}

func baseDeps() bpfDeps {
	return bpfDeps{
		memlock:   func() error { return nil },
		load:      func(*opensnoopObjects) error { return nil },
		attach:    func(*ebpf.Program) (func() error, error) { return func() error { return nil }, nil },
		newReader: func(*ebpf.Map) (recordReader, error) { return newFakeReader(), nil },
	}
}

func TestOpenSourceWith(t *testing.T) {
	t.Run("success returns reader and a working cleanup", func(t *testing.T) {
		rd, cleanup, err := openSourceWith(baseDeps())
		if err != nil {
			t.Fatalf("openSourceWith: %v", err)
		}
		if rd == nil || cleanup == nil {
			t.Fatal("nil reader or cleanup on success")
		}
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	t.Run("memlock failure", func(t *testing.T) {
		d := baseDeps()
		d.memlock = func() error { return errors.New("memlock denied") }
		if _, _, err := openSourceWith(d); err == nil || !strings.Contains(err.Error(), "memlock denied") {
			t.Fatalf("err = %v, want a memlock failure", err)
		}
	})

	t.Run("load failure", func(t *testing.T) {
		d := baseDeps()
		d.load = func(*opensnoopObjects) error { return errors.New("verifier rejected") }
		if _, _, err := openSourceWith(d); err == nil || !strings.Contains(err.Error(), "verifier rejected") {
			t.Fatalf("err = %v, want a load failure", err)
		}
	})

	t.Run("attach failure", func(t *testing.T) {
		d := baseDeps()
		d.attach = func(*ebpf.Program) (func() error, error) { return nil, errors.New("no tracepoint") }
		if _, _, err := openSourceWith(d); err == nil || !strings.Contains(err.Error(), "no tracepoint") {
			t.Fatalf("err = %v, want an attach failure", err)
		}
	})

	t.Run("ringbuf failure detaches", func(t *testing.T) {
		detached := false
		d := baseDeps()
		d.attach = func(*ebpf.Program) (func() error, error) {
			return func() error { detached = true; return nil }, nil
		}
		d.newReader = func(*ebpf.Map) (recordReader, error) { return nil, errors.New("ringbuf gone") }
		if _, _, err := openSourceWith(d); err == nil || !strings.Contains(err.Error(), "ringbuf gone") {
			t.Fatalf("err = %v, want a ringbuf failure", err)
		}
		if !detached {
			t.Error("tracepoint not detached after a ring-buffer open failure")
		}
	})
}

func TestRecordSizeMatchesGeneratedStruct(t *testing.T) {
	if got := binary.Size(opensnoopEvent{}); got != recordSize {
		t.Fatalf("recordSize=%d but the generated opensnoopEvent is %d bytes — the C struct and decoder drifted; rerun `make gen-bpf` and update recordSize",
			recordSize, got)
	}
}

func TestDefaultBPFDeps(t *testing.T) {
	d := defaultBPFDeps()
	if d.memlock == nil || d.load == nil || d.attach == nil || d.newReader == nil {
		t.Fatal("defaultBPFDeps left a nil primitive")
	}

	// Exercise the real privileged primitives' error paths. make check
	// has no CAP_BPF (and unprivileged_bpf_disabled is set), so each call
	// fails — covering the closure bodies the way tap_linux.go covers
	// realTunSetIff. The success paths run in the e2e gadget test.
	// newReader is left out: ringbuf.NewReader nil-derefs a nil *ebpf.Map
	// (panics rather than erroring), and a real map needs BPF privileges.
	_ = d.memlock() // may succeed (memcg-accounted kernels) or fail; either is fine.
	if err := d.load(&opensnoopObjects{}); err == nil {
		t.Skip("BPF load unexpectedly succeeded; attach/read paths are covered by the VM e2e test")
	}
	if _, err := d.attach(nil); err == nil {
		t.Error("attach with a nil program should fail")
	}
}

// TestRunWithRealOpenSourceFailsUnprivileged drives the production entry
// point. make check runs without CAP_BPF on a kernel with
// unprivileged_bpf_disabled set, so the real load must fail cleanly
// rather than panic — and this covers openSource's load + error path.
// The success path is exercised by the e2e gadget test inside a VM.
func TestRunWithRealOpenSourceFailsUnprivileged(t *testing.T) {
	err := Run(context.Background(), io.Discard)
	if err == nil {
		t.Skip("environment unexpectedly allowed BPF load; success path is covered by the VM e2e test")
	}
	if !strings.Contains(err.Error(), "opensnoop:") {
		t.Errorf("Run error = %v, want an opensnoop-prefixed failure", err)
	}
}
