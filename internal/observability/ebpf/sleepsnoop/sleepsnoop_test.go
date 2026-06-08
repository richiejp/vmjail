//go:build linux

package sleepsnoop

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

func recordBytes(t *testing.T, e sleepsnoopEvent) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, &e); err != nil {
		t.Fatalf("encode event: %v", err)
	}
	return buf.Bytes()
}

func makeRecord(t *testing.T, pid uint32, comm string, reqNs uint64) []byte {
	t.Helper()
	return recordBytes(t, sleepsnoopEvent{ReqNs: reqNs, Pid: pid, Comm: arr16(comm)})
}

func TestParseEvent(t *testing.T) {
	t.Run("decodes a full record", func(t *testing.T) {
		raw := makeRecord(t, 4321, "node", 100*uint64(time.Millisecond))
		ev, err := parseEvent(raw)
		if err != nil {
			t.Fatalf("parseEvent: %v", err)
		}
		if ev.PID != 4321 || ev.Comm != "node" || ev.ReqNs != 100*uint64(time.Millisecond) {
			t.Errorf("fields wrong: %+v", ev)
		}
	})

	t.Run("rejects a short record", func(t *testing.T) {
		if _, err := parseEvent([]byte{1, 2, 3}); err == nil {
			t.Fatal("expected an error for a short record")
		}
	})
}

func TestEventString(t *testing.T) {
	s := Event{PID: 7, Comm: "sh", ReqNs: 250 * uint64(time.Millisecond)}.String()
	for _, sub := range []string{"pid=7", `comm="sh"`, "dur=250ms"} {
		if !strings.Contains(s, sub) {
			t.Errorf("String()=%q missing %q", s, sub)
		}
	}
	// An absolute-deadline sleep records 0 → renders 0s, not garbage.
	if abs := (Event{PID: 1, Comm: "x", ReqNs: 0}).String(); !strings.Contains(abs, "dur=0s") {
		t.Errorf("zero-duration String()=%q want dur=0s", abs)
	}
}

func TestDurString(t *testing.T) {
	cases := []struct {
		ns   uint64
		want string
	}{
		{0, "0s"},
		{uint64(time.Second), "1s"},
		{1500 * uint64(time.Millisecond), "1.5s"},
		{^uint64(0), "18446744073709551615ns"}, // beyond int64 → raw ns, not a negative duration
	}
	for _, c := range cases {
		if got := durString(c.ns); got != c.want {
			t.Errorf("durString(%d) = %q, want %q", c.ns, got, c.want)
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
	fr := newFakeReader(makeRecord(t, 1, "sleep", uint64(time.Second)))
	var out bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, &out, nil, fakeOpener(fr)) }()

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), "dur=1s") {
		if time.Now().After(deadline) {
			t.Fatal("event never written")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("run after cancel = %v, want nil", err)
	}
	if !strings.Contains(out.String(), `comm="sleep"`) {
		t.Errorf("output missing comm: %q", out.String())
	}
}

func TestRunSkipsShortRecord(t *testing.T) {
	good := makeRecord(t, 9, "ok", uint64(time.Millisecond))
	fr := newFakeReader([]byte{0, 1, 2}, good)
	var out bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- run(ctx, &out, nil, fakeOpener(fr)) }()
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), `comm="ok"`) {
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
	fr := newFakeReader(makeRecord(t, 2, "x", 0))
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
	fr := newFakeReader(makeRecord(t, 7, "make", 50*uint64(time.Millisecond)))
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
	if s.Total != 1 || s.TotalSleepMs != 50 || len(s.TopCallers) == 0 || s.TopCallers[0].Key != "make" {
		t.Fatalf("aggregator not fed correctly: %+v", s)
	}
}

func baseDeps() bpfDeps {
	return bpfDeps{
		memlock:   func() error { return nil },
		load:      func(*sleepsnoopObjects, uint32) error { return nil },
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
		d.load = func(*sleepsnoopObjects, uint32) error { return errors.New("verifier rejected") }
		if _, _, err := openSourceWith(d, 0); err == nil || !strings.Contains(err.Error(), "verifier rejected") {
			t.Fatalf("err = %v, want a load failure", err)
		}
	})

	t.Run("threads selfPidns into load", func(t *testing.T) {
		var got uint32
		d := baseDeps()
		d.load = func(_ *sleepsnoopObjects, selfPidns uint32) error { got = selfPidns; return nil }
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

// TestOpenSourceWithUnwind covers the attach/ring-buffer failure ladder: each
// step that fails unwinds the steps already taken.
func TestOpenSourceWithUnwind(t *testing.T) {
	t.Run("nanosleep attach failure", func(t *testing.T) {
		d := baseDeps()
		d.attach = func(string, *ebpf.Program) (func() error, error) { return nil, errors.New("no tracepoint") }
		if _, _, err := openSourceWith(d, 0); err == nil || !strings.Contains(err.Error(), "no tracepoint") {
			t.Fatalf("err = %v, want an attach failure", err)
		}
	})

	t.Run("clock_nanosleep attach failure detaches nanosleep", func(t *testing.T) {
		nanoDetached := false
		d := baseDeps()
		d.attach = func(name string, _ *ebpf.Program) (func() error, error) {
			if name == "sys_enter_clock_nanosleep" {
				return nil, errors.New("no clock tracepoint")
			}
			return func() error { nanoDetached = true; return nil }, nil
		}
		if _, _, err := openSourceWith(d, 0); err == nil || !strings.Contains(err.Error(), "no clock tracepoint") {
			t.Fatalf("err = %v, want a clock-attach failure", err)
		}
		if !nanoDetached {
			t.Error("nanosleep tracepoint not detached after the clock attach failed")
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
	if got := binary.Size(sleepsnoopEvent{}); got != recordSize {
		t.Fatalf("recordSize=%d but the generated sleepsnoopEvent is %d bytes — the C struct and decoder drifted; rerun `make gen-bpf` and update recordSize",
			recordSize, got)
	}
}

func TestDefaultBPFDeps(t *testing.T) {
	d := defaultBPFDeps()
	if d.memlock == nil || d.load == nil || d.attach == nil || d.newReader == nil {
		t.Fatal("defaultBPFDeps left a nil primitive")
	}
	_ = d.memlock()
	if err := d.load(&sleepsnoopObjects{}, 4026531836); err == nil {
		t.Skip("BPF load unexpectedly succeeded; attach/read paths are covered by the VM e2e test")
	}
	if _, err := d.attach("sys_enter_nanosleep", nil); err == nil {
		t.Error("attach with a nil program should fail")
	}
}

// TestRunFailsUnprivileged drives the production entry points: make check runs
// without CAP_BPF, so the real load must fail cleanly rather than panic. The
// success path is exercised by the e2e gadget test inside a VM.
func TestRunFailsUnprivileged(t *testing.T) {
	// Pre-cancelled context so a *successful* load (a privileged env such
	// as the daft self-build sandbox) returns promptly instead of blocking
	// forever on the ring-buffer read. The unprivileged path returns the
	// load error before the read loop. A privileged env legitimately allows
	// the load, so that is a skip, not a failure (mirrors the sibling
	// gadgets).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range []error{
		Run(ctx, io.Discard, 4026531836),
		RunAggregating(ctx, io.Discard, NewAggregator(), 4026531836),
	} {
		if err == nil {
			t.Skip("environment unexpectedly allowed BPF load; success path is covered by the VM e2e test")
		}
	}
}
