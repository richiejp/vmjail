package observability

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// syscallClose lets tests release a Go-owned FD without holding
// the os.File handle that would otherwise close on GC.
func syscallClose(fd int) error { return syscall.Close(fd) }

// fakeProc returns the same canned ProcSample every call and
// increments a counter so tests can assert sampling actually
// happened the expected number of times. tickFn is called once
// per collect — tests use it to slow the sampler down or signal
// it.
func fakeCollect(canned *ProcSample, count *atomic.Int64) func(int) (*ProcSample, error) {
	return func(int) (*ProcSample, error) {
		count.Add(1)
		return canned, nil
	}
}

func TestNew_Validation(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		pid     int
		logPath string
		opts    Options
		wantErr string
	}{
		{"pid zero", 0, filepath.Join(dir, "a.jsonl"), Options{}, "pid must be > 0"},
		{"pid negative", -1, filepath.Join(dir, "a.jsonl"), Options{}, "pid must be > 0"},
		{"empty path", 1, "", Options{}, "logPath required"},
		{"relative path", 1, "rel.jsonl", Options{}, "must be absolute"},
		{"sub-tick interval", 1, filepath.Join(dir, "a.jsonl"), Options{SampleEvery: time.Millisecond}, "SampleEvery must be >= 10ms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.pid, tc.logPath, tc.opts)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestNew_DefaultsApplied(t *testing.T) {
	dir := t.TempDir()
	o, err := New(1, filepath.Join(dir, "a.jsonl"), Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if o.sampleEvery != DefaultSampleEvery {
		t.Errorf("SampleEvery = %s, want %s", o.sampleEvery, DefaultSampleEvery)
	}
	if o.maxBytes != DefaultMaxBytes {
		t.Errorf("MaxBytes = %d, want %d", o.maxBytes, DefaultMaxBytes)
	}
	if o.now == nil {
		t.Error("now is nil; New should default it to time.Now")
	}
}

func TestObserver_SamplesAndStops(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "obs.jsonl")
	o, err := New(os.Getpid(), logPath, Options{
		SampleEvery: 15 * time.Millisecond,
		MaxBytes:    -1, // no rotation
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var count atomic.Int64
	canned := &ProcSample{Comm: "fake", State: "R", VMRSSKB: 1234}
	o.collect = fakeCollect(canned, &count)
	if err := o.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Wait for at least 3 samples — first immediate + 2 ticks
	// gives enough signal that the loop runs without making the
	// test slow.
	deadline := time.Now().Add(2 * time.Second)
	for count.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := o.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if count.Load() < 3 {
		t.Fatalf("only %d samples collected, want >= 3", count.Load())
	}

	samples := readSamples(t, logPath)
	if len(samples) < 3 {
		t.Fatalf("only %d samples on disk, want >= 3", len(samples))
	}
	for i, s := range samples {
		if s.PID != os.Getpid() {
			t.Errorf("sample %d: PID = %d, want %d", i, s.PID, os.Getpid())
		}
		if s.Proc == nil {
			t.Fatalf("sample %d: Proc nil, want populated", i)
		}
		if s.Proc.Comm != "fake" {
			t.Errorf("sample %d: Comm = %q, want %q", i, s.Proc.Comm, "fake")
		}
	}
}

func TestObserver_StopBeforeStartFails(t *testing.T) {
	dir := t.TempDir()
	o, err := New(1, filepath.Join(dir, "obs.jsonl"), Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := o.Stop(); err == nil {
		t.Fatal("Stop before Start: want error, got nil")
	}
}

func TestObserver_DoubleStartFails(t *testing.T) {
	dir := t.TempDir()
	o, err := New(os.Getpid(), filepath.Join(dir, "obs.jsonl"), Options{
		SampleEvery: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var count atomic.Int64
	o.collect = fakeCollect(&ProcSample{}, &count)
	if err := o.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer func() {
		if err := o.Stop(); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()
	if err := o.Start(); err == nil {
		t.Fatal("second Start: want error, got nil")
	}
}

func TestObserver_DeadPIDRecordsErr(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "obs.jsonl")
	o, err := New(1, logPath, Options{SampleEvery: 10 * time.Millisecond, MaxBytes: -1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	o.collect = func(int) (*ProcSample, error) {
		// Simulate the target having exited between Start and the
		// next tick: collect returns os.ErrNotExist, observer
		// should write a Sample with Err set and keep going.
		return nil, os.ErrNotExist
	}
	if err := o.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := o.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	samples := readSamples(t, logPath)
	if len(samples) == 0 {
		t.Fatal("no samples recorded")
	}
	for i, s := range samples {
		if s.Err != "pid gone" {
			t.Errorf("sample %d: Err = %q, want %q", i, s.Err, "pid gone")
		}
		if s.Proc != nil {
			t.Errorf("sample %d: Proc = %+v, want nil", i, s.Proc)
		}
	}
}

func TestObserver_GenericErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "obs.jsonl")
	o, err := New(1, logPath, Options{SampleEvery: 10 * time.Millisecond, MaxBytes: -1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	o.collect = func(int) (*ProcSample, error) {
		return nil, errors.New("synthetic collect failure")
	}
	if err := o.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if err := o.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	samples := readSamples(t, logPath)
	if len(samples) == 0 {
		t.Fatal("no samples recorded")
	}
	if !strings.Contains(samples[0].Err, "synthetic collect failure") {
		t.Errorf("Err = %q, want substring %q", samples[0].Err, "synthetic collect failure")
	}
}

func TestObserver_RotatesOnSizeCap(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "obs.jsonl")
	// MaxBytes=4000 → rotate at 2000 bytes. Sample line is
	// ~300 bytes when Proc is populated, so rotation kicks in
	// after ~7 samples and we end up with one backup file and
	// a current file each bounded at ~2000 bytes + one line of
	// overhang (the line that crossed the threshold).
	o, err := New(1, logPath, Options{
		SampleEvery: 10 * time.Millisecond,
		MaxBytes:    4000,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var count atomic.Int64
	o.collect = fakeCollect(&ProcSample{
		Comm: "rotation-test", State: "S",
		UtimeJiffies: 1000, StimeJiffies: 2000,
		VMRSSKB: 4096, VMSizeKB: 65536,
		Rchar: 1 << 20, Wchar: 1 << 19,
	}, &count)
	if err := o.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for count.Load() < 20 {
		time.Sleep(5 * time.Millisecond)
	}
	if err := o.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("expected backup at %s, stat: %v", logPath+".1", err)
	}
	curStat, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat current log: %v", err)
	}
	bakStat, err := os.Stat(logPath + ".1")
	if err != nil {
		t.Fatalf("stat backup: %v", err)
	}
	// Each file is bounded at MaxBytes/2 + one sample line; the
	// implementation enforces approximate-not-exact cap because
	// the rotation check fires before-write rather than splitting
	// lines. Allow 1.5x MaxBytes total to make this robust to
	// JSON-encoding size drift, then make sure we're not blown
	// out entirely.
	if total := curStat.Size() + bakStat.Size(); total > 6000 {
		t.Errorf("total log size %d, want <= 6000 (1.5x MaxBytes 4000)", total)
	}
}

func TestObserver_OpenAppendsToExisting(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "obs.jsonl")
	pre := []byte(`{"ts":"2026-01-01T00:00:00Z","pid":1,"err":"pre-existing"}` + "\n")
	if err := os.WriteFile(logPath, pre, 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	o, err := New(1, logPath, Options{SampleEvery: 10 * time.Millisecond, MaxBytes: -1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var count atomic.Int64
	o.collect = fakeCollect(&ProcSample{Comm: "after"}, &count)
	if err := o.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for count.Load() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	if err := o.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	samples := readSamples(t, logPath)
	if len(samples) < 2 {
		t.Fatalf("got %d samples, want >= 2 (1 pre-existing + N new)", len(samples))
	}
	if samples[0].Err != "pre-existing" {
		t.Errorf("first sample Err = %q, want %q", samples[0].Err, "pre-existing")
	}
}

func TestObserver_LogPath(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "x.jsonl")
	o, err := New(1, want, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := o.LogPath(); got != want {
		t.Errorf("LogPath() = %q, want %q", got, want)
	}
}

// TestObserver_FinishPropagatesCloseError exercises the
// close-error branch in finish(): when the underlying FD is
// already gone by the time the goroutine calls file.Close, the
// returned error path executes (we don't assert on the specific
// error since it's racy whether Stop closes via tick or via
// finish — both paths fail, both feed into finish).
func TestObserver_FinishPropagatesCloseError(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "obs.jsonl")
	o, err := New(1, logPath, Options{SampleEvery: time.Hour, MaxBytes: -1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	o.collect = fakeCollect(&ProcSample{Comm: "x"}, new(atomic.Int64))
	if err := o.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	o.mu.Lock()
	fd := int(o.file.Fd())
	if cerr := syscallClose(fd); cerr != nil {
		t.Errorf("close fd: %v", cerr)
	}
	o.mu.Unlock()
	_ = o.Stop()
}

func TestObserver_OpenFailsOnBadDir(t *testing.T) {
	// Parent dir doesn't exist — Start should fail cleanly.
	o, err := New(1, "/nonexistent/dir/obs.jsonl", Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := o.Start(); err == nil {
		t.Fatal("Start: want error on missing parent dir, got nil")
	}
}

// TestObserver_RotateFailsWhenBackupIsDirectory exercises the
// rename-error branch in rotateLocked: if the backup path
// already exists as a non-empty directory, os.Rename refuses
// to overwrite (mismatched types). The error must propagate out
// of write rather than silently corrupting the rotation.
func TestObserver_RotateFailsWhenBackupIsDirectory(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "obs.jsonl")
	// Pre-create the backup path as a non-empty directory so
	// rename(2) of a regular file onto it errors with EISDIR.
	bakDir := logPath + ".1"
	if err := os.Mkdir(bakDir, 0o750); err != nil {
		t.Fatalf("seed backup dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bakDir, "occupant"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed occupant: %v", err)
	}
	o, err := New(1, logPath, Options{
		SampleEvery: time.Hour,
		MaxBytes:    400, // small cap → rotation fires quickly
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	o.collect = fakeCollect(&ProcSample{Comm: "rotation-target"}, new(atomic.Int64))
	if err := o.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Pump tick() directly until either rotation triggers (and
	// fails) or we conclude the test setup didn't get us there.
	var tickErr error
	for range 50 {
		if tickErr = o.tick(); tickErr != nil {
			break
		}
	}
	if tickErr == nil {
		t.Fatal("expected rotation rename to fail, but tick stayed clean")
	}
	close(o.stop)
	<-o.done
}

// TestObserver_WriteAfterUnderlyingFDClosed exercises the file-
// write error path in write(): we close the underlying FD out of
// band while leaving o.file populated. The next write returns
// EBADF and propagates up.
func TestObserver_WriteAfterUnderlyingFDClosed(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "obs.jsonl")
	o, err := New(1, logPath, Options{
		SampleEvery: time.Hour,
		MaxBytes:    -1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	o.collect = fakeCollect(&ProcSample{Comm: "x"}, new(atomic.Int64))
	if err := o.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	o.mu.Lock()
	fd := int(o.file.Fd())
	if cerr := syscallClose(fd); cerr != nil {
		t.Errorf("close fd: %v", cerr)
	}
	o.mu.Unlock()
	if err := o.tick(); err == nil {
		t.Fatal("tick after fd close: want error, got nil")
	}
	close(o.stop)
	<-o.done
}

// TestObserver_TickFailsWhenFileClosed pins the write-side error
// path: if the underlying file disappears mid-run (we close it
// out of band to simulate disk eviction / fd stomp), the next
// tick returns an error and the run loop exits via finish.
func TestObserver_TickFailsWhenFileClosed(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "obs.jsonl")
	o, err := New(1, logPath, Options{
		SampleEvery: time.Hour, // never tick on the timer
		MaxBytes:    -1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var count atomic.Int64
	o.collect = fakeCollect(&ProcSample{Comm: "x"}, &count)
	if err := o.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Force the file slot to nil so tick's write hits the
	// closed-file branch on the next invocation. Acquire the
	// mutex to avoid a race against the goroutine.
	o.mu.Lock()
	if err := o.file.Close(); err != nil {
		t.Errorf("close before stomp: %v", err)
	}
	o.file = nil
	o.mu.Unlock()
	if err := o.tick(); err == nil {
		t.Fatal("tick after file stomp: want error, got nil")
	}
	// Force the goroutine to exit cleanly.
	close(o.stop)
	<-o.done
}

// TestObserver_RunLoopPropagatesTickError exercises the second
// branch of run's select: a tick error after the first
// successful tick should land in o.err and Stop must surface it.
func TestObserver_RunLoopPropagatesTickError(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "obs.jsonl")
	o, err := New(1, logPath, Options{
		SampleEvery: 10 * time.Millisecond,
		MaxBytes:    -1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// First collect returns OK so the immediate-first-tick path
	// succeeds; subsequent collects return error AND the file
	// has been removed so the write itself errors. The error
	// has to come from write (not collect) because collect
	// errors are recorded in Sample.Err and don't propagate.
	o.collect = fakeCollect(&ProcSample{Comm: "x"}, new(atomic.Int64))
	if err := o.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Wait for the first tick to land, then close+nil the file
	// so a later tick errors out.
	time.Sleep(20 * time.Millisecond)
	o.mu.Lock()
	if err := o.file.Close(); err != nil {
		t.Errorf("close before stomp: %v", err)
	}
	o.file = nil
	o.mu.Unlock()
	// Wait for the goroutine to notice on a subsequent tick.
	deadline := time.Now().Add(time.Second)
	//vmjail:lint:disable=forloopbound reason="exits when o.done closes or the 1s deadline expires; both are bounded"
	for {
		select {
		case <-o.done:
			deadline = time.Time{} // signal exit
		default:
		}
		if deadline.IsZero() || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	stopErr := o.Stop()
	if stopErr == nil {
		t.Fatal("Stop: want error after file stomp, got nil")
	}
}

// TestObserver_CgroupSamplingFlows: when CgroupDir is set, each
// sample includes a populated Cgroup field. Drives the cgroup
// collection function with a synthetic /sys/fs/cgroup tree under
// t.TempDir() so we don't need real delegation.
func TestObserver_CgroupSamplingFlows(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "obs.jsonl")
	cgDir := t.TempDir()
	o, err := New(os.Getpid(), logPath, Options{
		SampleEvery: 10 * time.Millisecond,
		MaxBytes:    -1,
		CgroupDir:   cgDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var count atomic.Int64
	o.collect = fakeCollect(&ProcSample{Comm: "x"}, &count)
	o.collectCG = func(p string) (*CgroupSample, error) {
		if p != cgDir {
			t.Errorf("got cgroup path %q, want %q", p, cgDir)
		}
		return &CgroupSample{
			Path: p,
			CPU:  CgroupCPU{UsageUsec: 42},
		}, nil
	}
	if err := o.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for count.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if err := o.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	samples := readSamples(t, logPath)
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	for i, s := range samples {
		if s.Cgroup == nil {
			t.Errorf("sample %d: Cgroup nil, want populated", i)
			continue
		}
		if s.Cgroup.CPU.UsageUsec != 42 {
			t.Errorf("sample %d: CPU.UsageUsec = %d, want 42", i, s.Cgroup.CPU.UsageUsec)
		}
	}
}

// TestObserver_CgroupGoneRecordsErr: when the cgroup dir
// disappears mid-run, the tick records `err: cgroup gone` rather
// than burning the observer down.
func TestObserver_CgroupGoneRecordsErr(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "obs.jsonl")
	o, err := New(os.Getpid(), logPath, Options{
		SampleEvery: 10 * time.Millisecond,
		MaxBytes:    -1,
		CgroupDir:   "/somewhere",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	o.collect = fakeCollect(&ProcSample{Comm: "x"}, new(atomic.Int64))
	o.collectCG = func(string) (*CgroupSample, error) { return nil, os.ErrNotExist }
	if err := o.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if err := o.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	samples := readSamples(t, logPath)
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	if samples[0].Err != "cgroup gone" {
		t.Errorf("Err = %q, want %q", samples[0].Err, "cgroup gone")
	}
}

// TestObserver_CgroupGenericErrorRecorded: any other cgroup
// collect error lands in Err verbatim, again without crashing the
// observer.
func TestObserver_CgroupGenericErrorRecorded(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "obs.jsonl")
	o, err := New(os.Getpid(), logPath, Options{
		SampleEvery: 10 * time.Millisecond,
		MaxBytes:    -1,
		CgroupDir:   "/somewhere",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	o.collect = fakeCollect(&ProcSample{Comm: "x"}, new(atomic.Int64))
	o.collectCG = func(string) (*CgroupSample, error) {
		return nil, errors.New("synthetic cgroup err")
	}
	if err := o.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := o.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	samples := readSamples(t, logPath)
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	if !strings.Contains(samples[0].Err, "synthetic cgroup err") {
		t.Errorf("Err = %q, want substring %q", samples[0].Err, "synthetic cgroup err")
	}
}

// TestNew_RejectsRelativeCgroupDir matches the absolute-path
// requirement on logPath. Relative paths would be resolved
// against whatever cwd the sandbox runs in — exactly the kind
// of surprise we don't want in a per-VM accounting directory.
func TestNew_RejectsRelativeCgroupDir(t *testing.T) {
	dir := t.TempDir()
	_, err := New(1, filepath.Join(dir, "obs.jsonl"), Options{
		CgroupDir: "relative/path",
	})
	if err == nil || !strings.Contains(err.Error(), "CgroupDir") {
		t.Fatalf("err = %v, want CgroupDir error", err)
	}
}

// TestObserver_NetSamplingFlows: when CollectNet is set, each sample
// includes a populated Net field. Drives the callback with a
// hand-built NetSample so the test doesn't need a live netstack.
func TestObserver_NetSamplingFlows(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "obs.jsonl")
	var calls atomic.Int64
	o, err := New(os.Getpid(), logPath, Options{
		SampleEvery: 10 * time.Millisecond,
		MaxBytes:    -1,
		CollectNet: func() *NetSample {
			calls.Add(1)
			return &NetSample{RxBytes: 100, TxBytes: 200}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var procCount atomic.Int64
	o.collect = fakeCollect(&ProcSample{Comm: "x"}, &procCount)
	if err := o.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for procCount.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if err := o.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if calls.Load() == 0 {
		t.Fatal("CollectNet was never called")
	}
	samples := readSamples(t, logPath)
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	for i, s := range samples {
		if s.Net == nil {
			t.Errorf("sample %d: Net nil, want populated", i)
			continue
		}
		if s.Net.RxBytes != 100 || s.Net.TxBytes != 200 {
			t.Errorf("sample %d: Net = %+v, want {100 200}", i, *s.Net)
		}
	}
}

// TestObserver_NetSamplingNilReturnOmitsBlock: a CollectNet that
// returns nil leaves the Net field absent in the JSON, which is how
// callers signal "net wasn't ready this tick" without faking zero
// counters that look real.
func TestObserver_NetSamplingNilReturnOmitsBlock(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "obs.jsonl")
	o, err := New(os.Getpid(), logPath, Options{
		SampleEvery: 10 * time.Millisecond,
		MaxBytes:    -1,
		CollectNet:  func() *NetSample { return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var procCount atomic.Int64
	o.collect = fakeCollect(&ProcSample{Comm: "x"}, &procCount)
	if err := o.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for procCount.Load() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	if err := o.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	samples := readSamples(t, logPath)
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	if samples[0].Net != nil {
		t.Errorf("Net = %+v, want nil", *samples[0].Net)
	}
}

// readSamples drains the on-disk log into Sample structs so tests
// can assert structure without re-implementing the JSON-lines
// reader.
func readSamples(t *testing.T, path string) []Sample {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // G304: test reads its own t.TempDir() output.
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close log: %v", err)
		}
	}()
	var out []Sample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1024*1024)
	for sc.Scan() {
		var s Sample
		if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
			t.Fatalf("unmarshal %q: %v", sc.Text(), err)
		}
		out = append(out, s)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}

func TestDefaultPath(t *testing.T) {
	parent := t.TempDir()
	got, err := DefaultPath(parent, "sb-42")
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	want := filepath.Join(parent, "sb-42", DefaultLogFilename)
	if got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if fi, err := os.Stat(filepath.Dir(got)); err != nil || !fi.IsDir() {
		t.Errorf("id dir not created: stat err=%v", err)
	}
}

func TestDefaultPathMkdirFailure(t *testing.T) {
	// A parent that is a regular file makes MkdirAll fail.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	if _, err := DefaultPath(blocker, "sb"); err == nil {
		t.Fatal("expected mkdir failure when parent is a regular file")
	}
}
