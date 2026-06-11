//go:build linux

package iosnoop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf"
)

// Syscall names reused across the attach-plan cases.
const (
	scRead  = "read"
	scWrite = "write"
	scTee   = "tee"
)

// okResolve names an fd by its number — enough for the drain tests, which care
// about wiring, not the real /proc symlink.
func okResolve(_ uint32, fd int32) (string, error) { return fmt.Sprintf("t%d", fd), nil }

// scriptedSource returns a *source whose sample() walks the given samples,
// holding on the last one (so a final post-cancel drain re-reads end state).
func scriptedSource(samples ...sampleData) *source {
	i := 0
	return &source{
		sample: func() (sampleData, error) {
			s := samples[i]
			if i < len(samples)-1 {
				i++
			}
			return s, nil
		},
		attached: []string{scRead, scWrite},
		skipped:  []string{scTee},
		cleanup:  func() error { return nil },
	}
}

func TestDrainOnceEmitsResolvesAndAggregates(t *testing.T) {
	src := scriptedSource(sampleData{
		totals: Totals{Calls: 4, RdBytes: 30, WrBytes: 5, Errors: 1},
		stats: []FDStat{
			{PID: 5, FD: 7, RdBytes: 30, Calls: 3, Role: roleRead},
			{PID: 5, FD: 3, WrBytes: 5, Calls: 1, Role: roleWrite},
		},
	})
	var w bytes.Buffer
	agg := NewAggregator()
	cache, last := map[uint64]string{}, map[uint64]uint64{}
	if err := drainOnce(&w, agg, okResolve, src, cache, last); err != nil {
		t.Fatal(err)
	}
	out := w.String()
	if !strings.Contains(out, `pid=5 fd=7 r target="t7" r=30 w=0 calls=3 err=0`) {
		t.Fatalf("missing fd7 line: %q", out)
	}
	if !strings.Contains(out, `pid=5 fd=3 w target="t3"`) {
		t.Fatalf("missing fd3 line: %q", out)
	}
	if len(cache) != 2 {
		t.Fatalf("cache should hold both fds: %v", cache)
	}
	s := agg.Snapshot()
	if s.Total != 4 || s.Failed != 1 || s.ReadBytes != 30 || s.WriteBytes != 5 {
		t.Fatalf("aggregate totals: %+v", s)
	}
	if len(s.TopReaders) != 1 || s.TopReaders[0].Key != "t7" {
		t.Fatalf("TopReaders: %+v", s.TopReaders)
	}
}

func TestDrainOnceDeltaSuppressesUnchangedAndPrunes(t *testing.T) {
	hot := FDStat{PID: 1, FD: 4, RdBytes: 10, Calls: 2, Role: roleRead}
	advanced := FDStat{PID: 1, FD: 4, RdBytes: 20, Calls: 5, Role: roleRead}
	src := scriptedSource(
		sampleData{stats: []FDStat{hot}},      // first sighting → emits
		sampleData{stats: []FDStat{hot}},      // unchanged calls → silent
		sampleData{stats: []FDStat{advanced}}, // calls advanced → emits
		sampleData{stats: nil},                // fd gone → prune
	)
	var w bytes.Buffer
	cache, last := map[uint64]string{}, map[uint64]uint64{}
	for range 3 {
		if err := drainOnce(&w, nil, okResolve, src, cache, last); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(w.String(), "fd=4"); n != 2 {
		t.Fatalf("expected 2 emitted lines (first + advanced), got %d: %q", n, w.String())
	}
	// Final tick with no fds prunes the cross-tick state.
	if err := drainOnce(&w, nil, okResolve, src, cache, last); err != nil {
		t.Fatal(err)
	}
	if len(cache) != 0 || len(last) != 0 {
		t.Fatalf("prune left state: cache=%v last=%v", cache, last)
	}
}

func TestDrainOnceFallbackName(t *testing.T) {
	src := scriptedSource(sampleData{stats: []FDStat{{PID: 9, FD: 2, WrBytes: 1, Calls: 1, Role: roleWrite}}})
	for _, resolve := range []resolveFunc{
		func(uint32, int32) (string, error) { return "", errors.New("gone") }, // error
		func(uint32, int32) (string, error) { return "", nil },                // empty
	} {
		var w bytes.Buffer
		if err := drainOnce(&w, nil, resolve, src, map[uint64]string{}, map[uint64]uint64{}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(w.String(), `target="fd 2 (pid 9)"`) {
			t.Fatalf("missing synthetic fallback name: %q", w.String())
		}
	}
}

func TestDrainOnceSampleError(t *testing.T) {
	src := &source{sample: func() (sampleData, error) { return sampleData{}, errors.New("readfail") }}
	if err := drainOnce(io.Discard, nil, okResolve, src, map[uint64]string{}, map[uint64]uint64{}); err == nil ||
		!strings.Contains(err.Error(), "readfail") {
		t.Fatalf("want readfail, got %v", err)
	}
}

func TestRunDrainsUntilCancelledAndWritesHeader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	src := &source{
		sample: func() (sampleData, error) {
			calls++
			if calls >= 2 {
				cancel()
			}
			return sampleData{totals: Totals{Calls: uint64(calls)}}, nil //nolint:gosec // G115: a small test counter.
		},
		attached: []string{scRead},
		skipped:  []string{scTee},
		cleanup:  func() error { return nil },
	}
	var w bytes.Buffer
	agg := NewAggregator()
	if err := run(ctx, &w, agg, func() (*source, error) { return src, nil }, okResolve, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.String(), "# iosnoop attached=[read] skipped=[tee]") {
		t.Fatalf("header missing: %q", w.String())
	}
	if calls < 2 {
		t.Fatalf("expected >=2 samples, got %d", calls)
	}
}

func TestRunOpenError(t *testing.T) {
	open := func() (*source, error) { return nil, errors.New("noopen") }
	if err := run(context.Background(), io.Discard, nil, open, okResolve, time.Millisecond); err == nil ||
		!strings.Contains(err.Error(), "noopen") {
		t.Fatalf("want noopen, got %v", err)
	}
}

func TestRunReturnsSampleErrorWhileLive(t *testing.T) {
	src := &source{
		sample:  func() (sampleData, error) { return sampleData{}, errors.New("boom") },
		cleanup: func() error { return nil },
	}
	err := run(context.Background(), io.Discard, nil, func() (*source, error) { return src, nil }, okResolve, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want boom, got %v", err)
	}
}

func TestRunSwallowsSampleErrorOnceCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := &source{
		sample:  func() (sampleData, error) { cancel(); return sampleData{}, errors.New("late") },
		cleanup: func() error { return nil },
	}
	// First tick fires while ctx is live → drainOnce errors, but sample has by
	// then cancelled ctx, so the tick branch returns nil rather than the error.
	if err := run(ctx, io.Discard, nil, func() (*source, error) { return src, nil }, okResolve, time.Millisecond); err != nil {
		t.Fatalf("cancelled run must swallow the sample error, got %v", err)
	}
}

// ---- openSourceWith with fake bpfDeps (no kernel) ----

func fakeAvail(names ...string) func() (map[string]struct{}, error) {
	return func() (map[string]struct{}, error) {
		set := make(map[string]struct{}, len(names))
		for _, n := range names {
			set[n] = struct{}{}
		}
		return set, nil
	}
}

// allAvail exposes the enter+exit tracepoint of every syscall in the plan, so a
// happy-path open attaches the whole set and exercises every enter-program
// selector closure.
func allAvail() func() (map[string]struct{}, error) {
	names := make([]string, 0, 2*len(enterPlan()))
	for _, e := range enterPlan() {
		names = append(names, "sys_enter_"+e.syscall, "sys_exit_"+e.syscall)
	}
	return fakeAvail(names...)
}

func fakeDeps() bpfDeps {
	return bpfDeps{
		memlock:     func() error { return nil },
		load:        func(*iosnoopObjects, uint32) error { return nil },
		listEnter:   allAvail(),
		attach:      func(string, *ebpf.Program) (func() error, error) { return func() error { return nil }, nil },
		readTotals:  func(*iosnoopObjects) ([]iosnoopIototals, error) { return nil, nil },
		iterFDStats: func(*iosnoopObjects, func(uint64, iosnoopFdstat)) error { return nil },
	}
}

func TestOpenSourceWithHappyPath(t *testing.T) {
	attaches := 0
	d := fakeDeps()
	d.attach = func(string, *ebpf.Program) (func() error, error) {
		attaches++
		return func() error { return nil }, nil
	}
	src, err := openSourceWith(d, 1234)
	if err != nil {
		t.Fatal(err)
	}
	plan := enterPlan()
	if len(src.attached) != len(plan) || len(src.skipped) != 0 {
		t.Fatalf("attached %d/%d, skipped %v", len(src.attached), len(plan), src.skipped)
	}
	if attaches != 2*len(plan) { // enter+exit per syscall
		t.Fatalf("expected %d attaches, got %d", 2*len(plan), attaches)
	}
	if _, err := src.sample(); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if err := src.cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

func TestOpenSourceWithSkipsAbsentSyscalls(t *testing.T) {
	d := fakeDeps()
	d.listEnter = fakeAvail("sys_enter_read", "sys_exit_read", "sys_enter_write", "sys_exit_write")
	src, err := openSourceWith(d, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(src.attached, ",") != "read,write" {
		t.Fatalf("attached = %v, want [read write]", src.attached)
	}
	// Every other target syscall is reported skipped — no silent gaps.
	if len(src.skipped) != len(enterPlan())-2 || src.skipped[0] != "pread64" {
		t.Fatalf("skipped = %v", src.skipped)
	}
}

func TestOpenSourceWithErrors(t *testing.T) {
	cases := []struct {
		name string
		mut  func(d *bpfDeps)
		want string
	}{
		{"memlock", func(d *bpfDeps) { d.memlock = func() error { return errors.New("ml") } }, "memlock"},
		{"load", func(d *bpfDeps) { d.load = func(*iosnoopObjects, uint32) error { return errors.New("ld") } }, "load objects"},
		{"list", func(d *bpfDeps) {
			d.listEnter = func() (map[string]struct{}, error) { return nil, errors.New("ls") }
		}, "list syscall tracepoints"},
		{"none-available", func(d *bpfDeps) { d.listEnter = fakeAvail() }, "no traceable I/O syscalls"},
		{"enter-attach", func(d *bpfDeps) {
			d.attach = func(tp string, _ *ebpf.Program) (func() error, error) {
				if strings.HasPrefix(tp, "sys_enter_") {
					return nil, errors.New("ae")
				}
				return func() error { return nil }, nil
			}
		}, "attach sys_enter_read"},
		{"exit-attach", func(d *bpfDeps) {
			d.attach = func(tp string, _ *ebpf.Program) (func() error, error) {
				if strings.HasPrefix(tp, "sys_exit_") {
					return nil, errors.New("xe")
				}
				return func() error { return nil }, nil
			}
		}, "attach sys_exit_read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := fakeDeps()
			tc.mut(&d)
			_, err := openSourceWith(d, 0)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

// ---- pure decode helpers ----

func TestSumTotals(t *testing.T) {
	got := sumTotals([]iosnoopIototals{
		{Calls: 1, RdBytes: 10, WrBytes: 2, Errors: 1},
		{Calls: 3, RdBytes: 5, WrBytes: 0, Errors: 0},
	})
	if got != (Totals{Calls: 4, RdBytes: 15, WrBytes: 2, Errors: 1}) {
		t.Fatalf("sumTotals = %+v", got)
	}
}

func TestDecodeFDStat(t *testing.T) {
	got := decodeFDStat(fdKey(0x1234, 7), iosnoopFdstat{RdBytes: 9, WrBytes: 4, Calls: 2, Errors: 1, Role: roleRead | roleWrite})
	want := FDStat{PID: 0x1234, FD: 7, RdBytes: 9, WrBytes: 4, Calls: 2, Errors: 1, Role: roleRead | roleWrite}
	if got != want {
		t.Fatalf("decodeFDStat = %+v, want %+v", got, want)
	}
}

func TestFdKeyRoundTrip(t *testing.T) {
	k := fdKey(424242, 13)
	if pid := uint32(k >> 32); pid != 424242 {
		t.Fatalf("pid round-trip: %d", pid)
	}
	if fd := int32(uint32(k)); fd != 13 { //nolint:gosec // G115: symmetric unpack of fdKey, mirrors decodeFDStat.
		t.Fatalf("fd round-trip: %d", fd)
	}
}

// ---- production primitives that don't need CAP_BPF ----

func TestListTracepointSyscalls(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"sys_enter_read", "sys_exit_read", "enable"} {
		if err := os.Mkdir(filepath.Join(dir, n), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	set, err := listTracepointSyscalls(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := set["sys_enter_read"]; !ok || len(set) != 3 {
		t.Fatalf("set = %v", set)
	}
	if _, err := listTracepointSyscalls(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("expected error for a missing dir")
	}
}

func TestDefaultResolve(t *testing.T) {
	// Resolve a real fd of this process via /proc/self/fd → the temp file path.
	f, err := os.CreateTemp(t.TempDir(), "io")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	got, err := defaultResolve(uint32(os.Getpid()), int32(f.Fd())) //nolint:gosec // G115: a process fd is small.
	if err != nil {
		t.Fatalf("defaultResolve: %v", err)
	}
	if got != f.Name() {
		t.Fatalf("defaultResolve = %q, want %q", got, f.Name())
	}
	if _, err := defaultResolve(uint32(os.Getpid()), 0x7fffffff); err == nil { //nolint:gosec // G115: a real pid fits in uint32.
		t.Fatal("expected error resolving a bogus fd")
	}
}

func TestDefaultBPFDepsAndOpenSourceForWired(t *testing.T) {
	d := defaultBPFDeps()
	if d.memlock == nil || d.load == nil || d.listEnter == nil || d.attach == nil ||
		d.readTotals == nil || d.iterFDStats == nil {
		t.Fatal("defaultBPFDeps left a nil primitive")
	}
	// listEnter reads the real tracefs; under make check that dir is usually
	// absent, so the call just exercises the closure (result unchecked).
	_, _ = d.listEnter()
	if openSourceFor(7) == nil {
		t.Fatal("openSourceFor returned a nil opener")
	}
}

// TestCollectSample drives the sample assembly with fake map reads: totals are
// summed across CPUs and each fd entry is decoded; either read failing is
// surfaced wrapped.
func TestCollectSample(t *testing.T) {
	readTotals := func(*iosnoopObjects) ([]iosnoopIototals, error) {
		return []iosnoopIototals{{Calls: 2, RdBytes: 10}, {Calls: 3, WrBytes: 4}}, nil
	}
	iter := func(_ *iosnoopObjects, yield func(uint64, iosnoopFdstat)) error {
		yield(fdKey(1, 4), iosnoopFdstat{RdBytes: 10, Calls: 2, Role: roleRead})
		yield(fdKey(1, 5), iosnoopFdstat{WrBytes: 4, Calls: 3, Role: roleWrite})
		return nil
	}
	sd, err := collectSample(readTotals, iter, &iosnoopObjects{})
	if err != nil {
		t.Fatal(err)
	}
	if sd.totals != (Totals{Calls: 5, RdBytes: 10, WrBytes: 4}) {
		t.Fatalf("totals = %+v", sd.totals)
	}
	if len(sd.stats) != 2 || sd.stats[0].FD != 4 || sd.stats[1].FD != 5 {
		t.Fatalf("stats = %+v", sd.stats)
	}

	failTotals := func(*iosnoopObjects) ([]iosnoopIototals, error) { return nil, errors.New("tot") }
	if _, err := collectSample(failTotals, iter, &iosnoopObjects{}); err == nil ||
		!strings.Contains(err.Error(), "read totals") {
		t.Fatalf("want read-totals error, got %v", err)
	}
	failIter := func(*iosnoopObjects, func(uint64, iosnoopFdstat)) error { return errors.New("it") }
	if _, err := collectSample(readTotals, failIter, &iosnoopObjects{}); err == nil ||
		!strings.Contains(err.Error(), "iterate fdstats") {
		t.Fatalf("want iterate error, got %v", err)
	}
}

// TestRunRealLoadFails calls the production entry points. Loading BPF needs
// CAP_BPF, which make check lacks, so the load fails fast — but the call still
// drives Run / RunAggregating / openSourceFor / openSourceWith and the early
// load path, mirroring the sibling gadgets' coverage seam. The real attach +
// map reads run only in the guest under the e2e.
func TestRunRealLoadFails(t *testing.T) {
	// Pre-cancelled context: a privileged env (e.g. the daft self-build
	// sandbox with CAP_SYS_ADMIN) lets the load succeed, and without a
	// cancelled ctx Run would block forever on the ring-buffer read. The
	// unprivileged path returns the load error before the read loop.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, io.Discard, 0); err == nil {
		t.Skip("BPF load unexpectedly succeeded (running privileged); nothing to assert")
	}
	if err := RunAggregating(ctx, io.Discard, NewAggregator(), 0, nil); err == nil {
		t.Skip("BPF load unexpectedly succeeded (running privileged)")
	}
}

// TestAttachSignalling pins the readiness signal RunAggregating threads to the
// agent: onReady fires once, right after a successful open (so the agent can
// hold the container until the gadget is counting the workload's I/O), never
// when open fails (the agent releases its barrier on the drain goroutine's
// return instead), and a nil onReady is an inert no-op (the unfiltered Run
// path).
func TestAttachSignalling(t *testing.T) {
	t.Run("fires after a successful open", func(t *testing.T) {
		calls := 0
		src := &source{cleanup: func() error { return nil }}
		got, err := attachSignalling(func() (*source, error) { return src, nil }, func() { calls++ })()
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		if got != src {
			t.Fatal("open did not return the wrapped source")
		}
		if calls != 1 {
			t.Fatalf("onReady called %d times, want 1", calls)
		}
	})
	t.Run("stays silent when open fails", func(t *testing.T) {
		want := errors.New("load boom")
		called := false
		if _, err := attachSignalling(func() (*source, error) { return nil, want }, func() { called = true })(); !errors.Is(err, want) {
			t.Fatalf("err = %v, want %v", err, want)
		}
		if called {
			t.Fatal("onReady fired despite a failed open")
		}
	})
	t.Run("nil onReady is a no-op", func(t *testing.T) {
		src := &source{cleanup: func() error { return nil }}
		if _, err := attachSignalling(func() (*source, error) { return src, nil }, nil)(); err != nil {
			t.Fatalf("open: %v", err)
		}
	})
}
