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
			Pid: 4321, Uid: 1000, Dfd: -100, Flags: 0x241, Ret: -2,
			Comm: arr16("nix"), Filename: arr255("/nix/store/x"),
		})
		ev, err := parseEvent(raw)
		if err != nil {
			t.Fatalf("parseEvent: %v", err)
		}
		want := Event{PID: 4321, UID: 1000, DFD: -100, Flags: 0x241, Ret: -2, Comm: "nix", Filename: "/nix/store/x"}
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
	e := Event{PID: 7, UID: 0, DFD: -100, Flags: 0x41, Ret: 5, Comm: "crun", Filename: "/etc/hosts"}
	got := e.String()
	for _, sub := range []string{"pid=7", "uid=0", `comm="crun"`, "dfd=-100", "O_WRONLY|O_CREAT", "0x41", "ret=fd 5", `path="/etc/hosts"`} {
		if !strings.Contains(got, sub) {
			t.Errorf("String()=%q missing %q", got, sub)
		}
	}
	// A failed open renders the errno by name — the cache-miss line.
	miss := Event{PID: 7, Ret: -2, Comm: "cc1", Filename: "/missing.h"}.String()
	if !strings.Contains(miss, "ret=ENOENT") {
		t.Errorf("failed-open String()=%q missing ret=ENOENT", miss)
	}
}

func TestRetString(t *testing.T) {
	cases := []struct {
		ret  int32
		want string
	}{
		{0, "fd 0"},
		{7, "fd 7"},
		// Every named errno, so the whole errnoName switch is exercised.
		{-1, "EPERM"},
		{-2, "ENOENT"},
		{-9, "EBADF"},
		{-13, "EACCES"},
		{-20, "ENOTDIR"},
		{-21, "EISDIR"},
		{-23, "ENFILE"},
		{-24, "EMFILE"},
		{-36, "ENAMETOOLONG"},
		{-40, "ELOOP"},
		{-9999, "errno 9999"}, // unnamed errno falls back to the number
	}
	for _, c := range cases {
		if got := retString(c.ret); got != c.want {
			t.Errorf("retString(%d) = %q, want %q", c.ret, got, c.want)
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
		load:      func(*opensnoopObjects, uint32) error { return nil },
		attach:    func(string, *ebpf.Program) (func() error, error) { return func() error { return nil }, nil },
		newReader: func(*ebpf.Map) (recordReader, error) { return newFakeReader(), nil },
	}
}

func TestOpenSourceWith(t *testing.T) {
	t.Run("success returns reader and a working cleanup", func(t *testing.T) {
		rd, cleanup, err := openSourceWith(baseDeps(), 0)
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
		if _, _, err := openSourceWith(d, 0); err == nil || !strings.Contains(err.Error(), "memlock denied") {
			t.Fatalf("err = %v, want a memlock failure", err)
		}
	})

	t.Run("load failure", func(t *testing.T) {
		d := baseDeps()
		d.load = func(*opensnoopObjects, uint32) error { return errors.New("verifier rejected") }
		if _, _, err := openSourceWith(d, 0); err == nil || !strings.Contains(err.Error(), "verifier rejected") {
			t.Fatalf("err = %v, want a load failure", err)
		}
	})

	t.Run("threads selfPidns into load", func(t *testing.T) {
		var got uint32
		d := baseDeps()
		d.load = func(_ *opensnoopObjects, selfPidns uint32) error { got = selfPidns; return nil }
		_, cleanup, err := openSourceWith(d, 4026531836)
		if err != nil {
			t.Fatalf("openSourceWith: %v", err)
		}
		_ = cleanup()
		if got != 4026531836 {
			t.Errorf("selfPidns threaded to load = %d, want 4026531836", got)
		}
	})
}

// TestOpenSourceWithUnwind covers the attach/ring-buffer failure ladder:
// each step that fails must unwind the steps already taken (both
// tracepoints detach, objects close), so a partial load never leaks.
func TestOpenSourceWithUnwind(t *testing.T) {
	t.Run("enter attach failure", func(t *testing.T) {
		d := baseDeps()
		d.attach = func(string, *ebpf.Program) (func() error, error) { return nil, errors.New("no tracepoint") }
		if _, _, err := openSourceWith(d, 0); err == nil || !strings.Contains(err.Error(), "no tracepoint") {
			t.Fatalf("err = %v, want an attach failure", err)
		}
	})

	t.Run("exit attach failure detaches enter", func(t *testing.T) {
		enterDetached := false
		d := baseDeps()
		// First call (enter) succeeds with a detach hook; second (exit) fails.
		d.attach = func(name string, _ *ebpf.Program) (func() error, error) {
			if name == "sys_exit_openat" {
				return nil, errors.New("no exit tracepoint")
			}
			return func() error { enterDetached = true; return nil }, nil
		}
		if _, _, err := openSourceWith(d, 0); err == nil || !strings.Contains(err.Error(), "no exit tracepoint") {
			t.Fatalf("err = %v, want an exit-attach failure", err)
		}
		if !enterDetached {
			t.Error("enter tracepoint not detached after the exit attach failed")
		}
	})

	t.Run("ringbuf failure detaches both", func(t *testing.T) {
		detaches := 0
		d := baseDeps()
		d.attach = func(string, *ebpf.Program) (func() error, error) {
			return func() error { detaches++; return nil }, nil
		}
		d.newReader = func(*ebpf.Map) (recordReader, error) { return nil, errors.New("ringbuf gone") }
		if _, _, err := openSourceWith(d, 0); err == nil || !strings.Contains(err.Error(), "ringbuf gone") {
			t.Fatalf("err = %v, want a ringbuf failure", err)
		}
		if detaches != 2 {
			t.Errorf("detaches = %d, want both tracepoints detached after a ring-buffer open failure", detaches)
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
	// A non-zero selfPidns also covers the .rodata Set branch (the spec
	// parse + variable write happen in userspace; only LoadAndAssign needs
	// CAP_BPF, so this fails at the load, not the Set).
	if err := d.load(&opensnoopObjects{}, 4026531836); err == nil {
		t.Skip("BPF load unexpectedly succeeded; attach/read paths are covered by the VM e2e test")
	}
	if _, err := d.attach("sys_enter_openat", nil); err == nil {
		t.Error("attach with a nil program should fail")
	}
}

// TestRunWithRealOpenSourceFailsUnprivileged drives the production entry
// point. make check runs without CAP_BPF on a kernel with
// unprivileged_bpf_disabled set, so the real load must fail cleanly
// rather than panic — and this covers openSource's load + error path.
// The success path is exercised by the e2e gadget test inside a VM.
func TestRunWithRealOpenSourceFailsUnprivileged(t *testing.T) {
	// Pre-cancelled context: a privileged env (e.g. the daft self-build
	// sandbox, CAP_SYS_ADMIN) lets the load succeed, and without a cancelled
	// ctx Run blocks forever on the ring-buffer read. The unprivileged path
	// returns the load error before the read loop.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run(ctx, io.Discard, 4026531836)
	if err == nil {
		t.Skip("environment unexpectedly allowed BPF load; success path is covered by the VM e2e test")
	}
	if !strings.Contains(err.Error(), "opensnoop:") {
		t.Errorf("Run error = %v, want an opensnoop-prefixed failure", err)
	}
}

// TestRunAggregatingFailsUnprivileged drives the aggregating production
// entry the agent actually calls: like Run, the real load must fail cleanly
// without CAP_BPF rather than panic. The success path is the VM e2e.
func TestRunAggregatingFailsUnprivileged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := RunAggregating(ctx, io.Discard, NewAggregator(), 4026531836)
	if err == nil {
		t.Skip("environment unexpectedly allowed BPF load; success path is covered by the VM e2e test")
	}
	if !strings.Contains(err.Error(), "opensnoop:") {
		t.Errorf("RunAggregating error = %v, want an opensnoop-prefixed failure", err)
	}
}
