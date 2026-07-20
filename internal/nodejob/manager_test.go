package nodejob

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

const testCommand = "run"

type fakeRunner struct {
	mu      sync.Mutex
	process *fakeProcess
	err     error
	stdout  string
	stderr  string
	started chan struct{}
	release chan struct{}
}

func (r *fakeRunner) Start(ctx context.Context, _ StartRequest, stdout, stderr io.Writer) (Process, error) {
	if r.started != nil {
		close(r.started)
	}
	if r.release != nil {
		select {
		case <-r.release:
		case <-ctx.Done():
		}
	}
	_, _ = io.WriteString(stdout, r.stdout)
	_, _ = io.WriteString(stderr, r.stderr)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.process, r.err
}

type fakeProcess struct {
	mu          sync.Mutex
	done        chan struct{}
	result      Result
	err         error
	cancelCalls int
	onCancel    func()
}

func newFakeProcess() *fakeProcess { return &fakeProcess{done: make(chan struct{})} }

func (p *fakeProcess) Wait() (Result, error) {
	<-p.done
	return p.result, p.err
}

func (p *fakeProcess) Cancel() error {
	p.mu.Lock()
	p.cancelCalls++
	f := p.onCancel
	p.mu.Unlock()
	if f != nil {
		f()
	}
	return nil
}

func (p *fakeProcess) finish(result Result, err error) {
	p.result, p.err = result, err
	close(p.done)
}

func TestManagerSuccessAndResumableSeparateSpools(t *testing.T) {
	proc := newFakeProcess()
	runner := &fakeRunner{process: proc, stdout: "stdout-data", stderr: "stderr-data"}
	m := newTestManager(t, runner, Limits{SpoolBytes: 64, MaxReadBytes: 4})

	started, err := m.Start(StartRequest{SandboxUID: "sandbox-1", Command: []string{"agent", testCommand}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if started.State != StateRunning || len(started.ID) != 36 || started.ID[14] != '4' || started.Stdout.NextCursor != 4 || string(started.Stdout.Data) != "stdo" {
		t.Fatalf("started = %+v", started)
	}
	if string(started.Stderr.Data) != "stde" {
		t.Fatalf("stderr = %q", started.Stderr.Data)
	}

	next, err := m.Get(started.ID, started.Stdout.NextCursor, started.Stderr.NextCursor, 4)
	if err != nil {
		t.Fatalf("Get next: %v", err)
	}
	if string(next.Stdout.Data) != "ut-d" || string(next.Stderr.Data) != "rr-d" {
		t.Fatalf("next chunks stdout=%q stderr=%q", next.Stdout.Data, next.Stderr.Data)
	}

	proc.finish(Result{}, nil)
	finished := waitTerminal(t, m, started.ID)
	if finished.State != StateSucceeded || !finished.ExitCodeSet || finished.ExitCode != 0 || finished.CompletedAt.IsZero() {
		t.Fatalf("finished = %+v", finished)
	}
	if got, err := m.Get(started.ID, 0, 0, 64); err != nil || string(got.Stdout.Data) != "stdo" || string(got.Stderr.Data) != "stde" {
		t.Fatalf("final Get = %+v, %v", got, err)
	}
}

func TestManagerSpoolExpiryAndCursorValidation(t *testing.T) {
	proc := newFakeProcess()
	m := newTestManager(t, &fakeRunner{process: proc, stdout: "0123456789", stderr: "abcdefghij"}, Limits{SpoolBytes: 6, MaxReadBytes: 3})
	s, err := m.Start(StartRequest{SandboxUID: "sb", Command: []string{testCommand}})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Stdout.CursorExpired || string(s.Stdout.Data) != "456" || s.Stdout.NextCursor != 7 {
		t.Fatalf("stdout = %+v", s.Stdout)
	}
	if !s.Stderr.CursorExpired || string(s.Stderr.Data) != "efg" {
		t.Fatalf("stderr = %+v", s.Stderr)
	}
	if _, err := m.Get(s.ID, 11, 10, 3); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("ahead cursor error = %v", err)
	}
	proc.finish(Result{}, nil)
}

func TestManagerCancelWinsTerminalRace(t *testing.T) {
	proc := newFakeProcess()
	var once sync.Once
	proc.onCancel = func() { once.Do(func() { proc.finish(Result{ExitCode: 137}, nil) }) }
	m := newTestManager(t, &fakeRunner{process: proc}, Limits{})
	s, err := m.Start(StartRequest{SandboxUID: "sb", Command: []string{"sleep"}})
	if err != nil {
		t.Fatal(err)
	}
	canceling, err := m.Cancel(s.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if canceling.State != StateCanceling || !canceling.CancelCalled {
		t.Fatalf("canceling = %+v", canceling)
	}
	finished := waitTerminal(t, m, s.ID)
	if finished.State != StateCanceled || finished.ExitCode != 137 {
		t.Fatalf("finished = %+v", finished)
	}
	if again, err := m.Cancel(s.ID); err != nil || again.State != StateCanceled {
		t.Fatalf("second Cancel = %+v, %v", again, err)
	}
}

func TestManagerIdempotentStartConflictAndTerminalDelete(t *testing.T) {
	proc := newFakeProcess()
	m := newTestManager(t, &fakeRunner{process: proc}, Limits{MaxJobs: 1})
	req := StartRequest{IdempotencyKey: "run-1", SandboxUID: "sb", Command: []string{testCommand}}
	first, err := m.Start(req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := m.Start(req)
	if err != nil || again.ID != first.ID {
		t.Fatalf("idempotent Start = %+v, %v", again, err)
	}
	conflict := req
	conflict.Command = []string{"different"}
	if _, err := m.Start(conflict); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting Start = %v", err)
	}
	if err := m.Delete(first.ID); !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("Delete running = %v", err)
	}
	proc.finish(Result{}, nil)
	waitTerminal(t, m, first.ID)
	if err := m.Delete(first.ID); err != nil {
		t.Fatalf("Delete terminal: %v", err)
	}
	if err := m.Delete(first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete = %v", err)
	}

	// Deletion releases both capacity and the key, allowing a fresh job.
	proc2 := newFakeProcess()
	m.runner = &fakeRunner{process: proc2}
	fresh, err := m.Start(req)
	if err != nil || fresh.ID == first.ID {
		t.Fatalf("fresh Start after Delete = %+v, %v", fresh, err)
	}
	proc2.finish(Result{}, nil)
}

func TestManagerLifecycleCancellationStopsJobsAndRejectsStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	proc := newFakeProcess()
	var once sync.Once
	proc.onCancel = func() { once.Do(func() { proc.finish(Result{}, context.Canceled) }) }
	m, err := New(ctx, &fakeRunner{process: proc}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := m.Start(StartRequest{SandboxUID: "sb", Command: []string{testCommand}})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if final := waitTerminal(t, m, s.ID); final.State != StateCanceled {
		t.Fatalf("final = %+v", final)
	}
	if _, err := m.Start(StartRequest{SandboxUID: "sb", Command: []string{"again"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start after shutdown = %v", err)
	}
}

func TestManagerCancelWhileRunnerStarting(t *testing.T) {
	proc := newFakeProcess()
	var once sync.Once
	proc.onCancel = func() { once.Do(func() { proc.finish(Result{}, context.Canceled) }) }
	runner := &fakeRunner{process: proc, started: make(chan struct{}), release: make(chan struct{})}
	m := newTestManager(t, runner, Limits{})
	result := make(chan Snapshot, 1)
	go func() {
		s, _ := m.Start(StartRequest{SandboxUID: "sb", Command: []string{testCommand}})
		result <- s
	}()
	<-runner.started

	m.mu.RLock()
	var id string
	for id = range m.jobs {
		break
	}
	m.mu.RUnlock()
	if _, err := m.Cancel(id); err != nil {
		t.Fatalf("Cancel starting: %v", err)
	}
	close(runner.release)
	s := <-result
	if s.State != StateCanceling && s.State != StateCanceled {
		t.Fatalf("start result state = %s", s.State)
	}
	if final := waitTerminal(t, m, id); final.State != StateCanceled {
		t.Fatalf("final = %+v", final)
	}
}

func TestManagerStartFailureIsDurableAndBounded(t *testing.T) {
	reason := strings.Repeat("x", MaxTerminalReasonLen+20)
	m := newTestManager(t, &fakeRunner{err: errors.New(reason)}, Limits{})
	s, err := m.Start(StartRequest{SandboxUID: "sb", Command: []string{testCommand}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.State != StateFailed || len(s.Reason) != MaxTerminalReasonLen || s.CompletedAt.IsZero() {
		t.Fatalf("failed snapshot = %+v reason len=%d", s, len(s.Reason))
	}
}

func TestManagerFailedProcessAndLimits(t *testing.T) {
	proc := newFakeProcess()
	m := newTestManager(t, &fakeRunner{process: proc}, Limits{MaxJobs: 1})
	s, err := m.Start(StartRequest{SandboxUID: "sb", Command: []string{testCommand}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(StartRequest{SandboxUID: "sb2", Command: []string{testCommand}}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("capacity error = %v", err)
	}
	proc.finish(Result{ExitCode: 2}, errors.New("wait failed"))
	if final := waitTerminal(t, m, s.ID); final.State != StateFailed || final.Reason != "wait failed" {
		t.Fatalf("final = %+v", final)
	}
	if _, err := m.Get("missing", 0, 0, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Get = %v", err)
	}
	if _, err := m.Cancel("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Cancel = %v", err)
	}
}

func TestManagerValidationAndConstruction(t *testing.T) {
	if _, err := New(nil, &fakeRunner{}, Limits{}); err == nil { //nolint:staticcheck // intentionally verifies nil-context rejection.
		t.Fatal("New nil context succeeded")
	}
	if _, err := New(t.Context(), nil, Limits{}); err == nil {
		t.Fatal("New nil runner succeeded")
	}
	m := newTestManager(t, &fakeRunner{}, Limits{})
	bad := []StartRequest{
		{},
		{SandboxUID: strings.Repeat("s", MaxSandboxUIDBytes+1), Command: []string{"x"}},
		{SandboxUID: "sb"},
		{SandboxUID: "sb", Command: make([]string, MaxCommandArgs+1)},
		{SandboxUID: "sb", Command: []string{""}},
		{SandboxUID: "sb", Command: []string{strings.Repeat("x", MaxCommandArgBytes+1)}},
		{IdempotencyKey: strings.Repeat("k", MaxIdempotencyBytes+1), SandboxUID: "sb", Command: []string{"x"}},
	}
	for i, req := range bad {
		if _, err := m.Start(req); err == nil {
			t.Errorf("bad request %d succeeded", i)
		}
	}
}

func newTestManager(t *testing.T, runner Runner, limits Limits) *Manager {
	t.Helper()
	m, err := New(t.Context(), runner, limits)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func waitTerminal(t *testing.T, m *Manager, id string) Snapshot {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s, err := m.Get(id, 0, 0, 1)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if s.State.Terminal() {
			return s
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("job did not become terminal")
	return Snapshot{}
}
