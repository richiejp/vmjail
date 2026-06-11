//go:build linux

package execsnoop

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

// packArgs lays argv out the way bpf_probe_read_user_str does: each string
// written with a trailing NUL.
func packArgs(argv ...string) (buf [2560]uint8, count int32, size uint32) {
	off := 0
	for _, s := range argv {
		off += copy(buf[off:], s)
		if off < len(buf) {
			buf[off] = 0
			off++
		}
	}
	//nolint:gosec // G115: test buffer offset, always small.
	return buf, int32(len(argv)), uint32(off)
}

func recordBytes(t *testing.T, e execsnoopEvent) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, &e); err != nil {
		t.Fatalf("encode event: %v", err)
	}
	return buf.Bytes()
}

// makeRecord builds a wire record for an exec with the given argv.
func makeRecord(t *testing.T, pid, ppid, uid uint32, ret int32, comm string, argv ...string) []byte {
	t.Helper()
	args, count, size := packArgs(argv...)
	return recordBytes(t, execsnoopEvent{
		Pid: pid, Ppid: ppid, Uid: uid, Ret: ret,
		ArgsCount: count, ArgsSize: size, Comm: arr16(comm), Args: args,
	})
}

func TestParseEvent(t *testing.T) {
	t.Run("decodes a full record", func(t *testing.T) {
		raw := makeRecord(t, 4321, 4000, 1000, 0, "sh", "/bin/cc", "-c", "x.c")
		ev, err := parseEvent(raw)
		if err != nil {
			t.Fatalf("parseEvent: %v", err)
		}
		if ev.PID != 4321 || ev.PPID != 4000 || ev.UID != 1000 || ev.Ret != 0 || ev.Comm != "sh" {
			t.Errorf("scalar fields wrong: %+v", ev)
		}
		want := []string{"/bin/cc", "-c", "x.c"}
		if strings.Join(ev.Argv, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("Argv = %q, want %q", ev.Argv, want)
		}
		if ev.Truncated {
			t.Error("short argv should not be flagged truncated")
		}
	})

	t.Run("flags a capped argv as truncated", func(t *testing.T) {
		raw := makeRecord(t, 1, 1, 0, 0, "go", "/bin/x")
		// Force the count to the cap without needing 20 real args.
		binary.LittleEndian.PutUint32(raw[offArgsCount:offArgsSize], uint32(maxArgs))
		ev, err := parseEvent(raw)
		if err != nil {
			t.Fatalf("parseEvent: %v", err)
		}
		if !ev.Truncated {
			t.Error("args_count at the cap should flag truncated")
		}
	})

	t.Run("rejects a short record", func(t *testing.T) {
		if _, err := parseEvent([]byte{1, 2, 3}); err == nil {
			t.Fatal("expected an error for a short record")
		}
	})
}

func TestEventString(t *testing.T) {
	ok := Event{PID: 7, PPID: 4, UID: 0, Ret: 0, Comm: "sh", Argv: []string{"/bin/ls", "-l"}}.String()
	for _, sub := range []string{"pid=7", "ppid=4", "uid=0", `comm="sh"`, "ret=ok", "argv=[/bin/ls -l]"} {
		if !strings.Contains(ok, sub) {
			t.Errorf("String()=%q missing %q", ok, sub)
		}
	}
	miss := Event{PID: 9, Ret: -2, Comm: "make", Argv: []string{"/usr/bin/gcc-99"}}.String()
	if !strings.Contains(miss, "ret=ENOENT") {
		t.Errorf("failed-exec String()=%q missing ret=ENOENT", miss)
	}
	trunc := Event{Argv: []string{"/bin/x"}, Truncated: true}.String()
	if !strings.Contains(trunc, "(truncated)") {
		t.Errorf("truncated String()=%q missing the marker", trunc)
	}
}

func TestRetString(t *testing.T) {
	cases := []struct {
		ret  int32
		want string
	}{
		{0, "ok"},
		{-1, "EPERM"},
		{-2, "ENOENT"},
		{-7, "E2BIG"},
		{-8, "ENOEXEC"},
		{-13, "EACCES"},
		{-20, "ENOTDIR"},
		{-26, "ETXTBSY"},
		{-36, "ENAMETOOLONG"},
		{-40, "ELOOP"},
		{-9999, "errno 9999"},
	}
	for _, c := range cases {
		if got := retString(c.ret); got != c.want {
			t.Errorf("retString(%d) = %q, want %q", c.ret, got, c.want)
		}
	}
}

// fakeReader feeds canned records, then blocks on Read until Close, then
// reports ringbuf.ErrClosed — mimicking *ringbuf.Reader's cancel semantics.
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
	fr := newFakeReader(makeRecord(t, 1, 0, 0, 0, "sh", "/bin/sh", "-c", "true"))
	var out bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, &out, nil, fakeOpener(fr)) }()

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), "/bin/sh") {
		if time.Now().After(deadline) {
			t.Fatal("event never written")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("run after cancel = %v, want nil", err)
	}
	if !strings.Contains(out.String(), "argv=[/bin/sh -c true]") {
		t.Errorf("output missing argv: %q", out.String())
	}
}

func TestRunSkipsShortRecord(t *testing.T) {
	good := makeRecord(t, 9, 0, 0, 0, "go", "/ok")
	fr := newFakeReader([]byte{0, 1, 2}, good)
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
	fr := newFakeReader(makeRecord(t, 2, 0, 0, 0, "x", "/y"))
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

func TestRunAggregatingFeedsAggregator(t *testing.T) {
	fr := newFakeReader(makeRecord(t, 7, 1, 0, -2, "make", "/usr/bin/gcc-99", "-c"))
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
	if s.Total != 1 || s.Failed != 1 || len(s.TopFailed) == 0 || s.TopFailed[0].Key != "/usr/bin/gcc-99" {
		t.Fatalf("aggregator not fed correctly: %+v", s)
	}
}

func baseDeps() bpfDeps {
	return bpfDeps{
		memlock:   func() error { return nil },
		load:      func(*execsnoopObjects, uint32) error { return nil },
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
		d.load = func(*execsnoopObjects, uint32) error { return errors.New("verifier rejected") }
		if _, _, err := openSourceWith(d, 0); err == nil || !strings.Contains(err.Error(), "verifier rejected") {
			t.Fatalf("err = %v, want a load failure", err)
		}
	})

	t.Run("threads selfPidns into load", func(t *testing.T) {
		var got uint32
		d := baseDeps()
		d.load = func(_ *execsnoopObjects, selfPidns uint32) error { got = selfPidns; return nil }
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
// each step that fails unwinds the steps already taken.
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
		d.attach = func(name string, _ *ebpf.Program) (func() error, error) {
			if name == "sys_exit_execve" {
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
			t.Errorf("detaches = %d, want both tracepoints detached", detaches)
		}
	})
}

func TestRecordSizeMatchesGeneratedStruct(t *testing.T) {
	if got := binary.Size(execsnoopEvent{}); got != recordSize {
		t.Fatalf("recordSize=%d but the generated execsnoopEvent is %d bytes — the C struct and decoder drifted; rerun `make gen-bpf` and update recordSize",
			recordSize, got)
	}
}

func TestDefaultBPFDeps(t *testing.T) {
	d := defaultBPFDeps()
	if d.memlock == nil || d.load == nil || d.attach == nil || d.newReader == nil {
		t.Fatal("defaultBPFDeps left a nil primitive")
	}
	_ = d.memlock()
	if err := d.load(&execsnoopObjects{}, 4026531836); err == nil {
		t.Skip("BPF load unexpectedly succeeded; attach/read paths are covered by the VM e2e test")
	}
	if _, err := d.attach("sys_enter_execve", nil); err == nil {
		t.Error("attach with a nil program should fail")
	}
}

// TestRunFailsUnprivileged drives the production entry points: make check
// runs without CAP_BPF, so the real load must fail cleanly rather than panic.
// The success path is exercised by the e2e gadget test inside a VM.
func TestRunFailsUnprivileged(t *testing.T) {
	// Pre-cancelled context: if the BPF load is *allowed* (a privileged
	// environment such as the daft self-build sandbox, which holds
	// CAP_SYS_ADMIN), Run otherwise runs past the load into the blocking
	// ring-buffer read and the test hangs forever. With ctx already
	// cancelled it returns promptly and the err==nil skip below applies.
	// The unprivileged path still returns the load error before the read.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range []error{
		Run(ctx, io.Discard, 4026531836),
		RunAggregating(ctx, io.Discard, NewAggregator(), 4026531836, nil),
	} {
		if err == nil {
			t.Skip("environment unexpectedly allowed BPF load; success path is covered by the VM e2e test")
		}
		if !strings.Contains(err.Error(), "execsnoop:") {
			t.Errorf("error = %v, want an execsnoop-prefixed failure", err)
		}
	}
}

// TestAttachSignalling pins the readiness signal RunAggregating threads to the
// agent: onReady fires once, right after a successful open (so the agent can
// hold the container until the gadget is capturing), never when open fails
// (the agent releases its barrier on the drain goroutine's return instead),
// and a nil onReady is an inert no-op (the unfiltered Run path).
func TestAttachSignalling(t *testing.T) {
	t.Run("fires after a successful open", func(t *testing.T) {
		calls := 0
		open := attachSignalling(fakeOpener(newFakeReader()), func() { calls++ })
		_, cleanup, err := open()
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		_ = cleanup()
		if calls != 1 {
			t.Fatalf("onReady called %d times, want 1", calls)
		}
	})
	t.Run("stays silent when open fails", func(t *testing.T) {
		want := errors.New("load boom")
		failing := func() (recordReader, func() error, error) { return nil, nil, want }
		called := false
		if _, _, err := attachSignalling(failing, func() { called = true })(); !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
		if called {
			t.Fatal("onReady fired despite a failed open")
		}
	})
	t.Run("nil onReady is a no-op", func(t *testing.T) {
		if _, _, err := attachSignalling(fakeOpener(newFakeReader()), nil)(); err != nil {
			t.Fatalf("open: %v", err)
		}
	})
}
