package observability

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
	//daft:lint:disable=forloopbound reason="exits when o.done closes or the 1s deadline expires; both are bounded"
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
