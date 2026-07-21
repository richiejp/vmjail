package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
)

// psCmd is a throwaway command name reused across the post-start tests.
const psCmd = "do-thing"

// fakePostStartProc is an AttachProcess backed by a fixed output buffer:
// Read drains it then EOFs (so prefixCopy returns), and Wait returns a
// preset exit code WITHOUT requiring Close first (unlike fakeAttachProcess,
// which the attach listener drives Close-then-Wait).
type fakePostStartProc struct {
	out    *bytes.Reader
	code   int32
	err    error
	closed bool
}

func (p *fakePostStartProc) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *fakePostStartProc) Write(b []byte) (int, error) { return len(b), nil }
func (p *fakePostStartProc) Resize(uint32, uint32) error { return nil }
func (p *fakePostStartProc) Wait() (int32, error)        { return p.code, p.err }
func (p *fakePostStartProc) Close() error                { p.closed = true; return nil }

// postStartResult configures the next exec call's outcome.
type postStartResult struct {
	out      string
	code     int32
	waitErr  error
	startErr error
}

// recordingExec is an ExecAttach that records each request and returns
// queued results in order (falling back to a zero-exit empty result).
type recordingExec struct {
	mu      sync.Mutex
	reqs    []AttachExecRequest
	results []postStartResult
}

func (e *recordingExec) Start(_ context.Context, req AttachExecRequest) (AttachProcess, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reqs = append(e.reqs, req)
	var r postStartResult
	if len(e.results) > 0 {
		r, e.results = e.results[0], e.results[1:]
	}
	if r.startErr != nil {
		return nil, r.startErr
	}
	return &fakePostStartProc{out: bytes.NewReader([]byte(r.out)), code: r.code, err: r.waitErr}, nil
}

func (e *recordingExec) calls() []AttachExecRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]AttachExecRequest, len(e.reqs))
	copy(out, e.reqs)
	return out
}

// stubRunning swaps containerRunning for the test and restores it.
func stubRunning(t *testing.T, running bool) {
	t.Helper()
	prev := containerRunning
	containerRunning = func(context.Context) bool { return running }
	t.Cleanup(func() { containerRunning = prev })
}

func postStartCfg(argvs ...[]string) *agentv1.GuestConfig {
	gc := &agentv1.GuestConfig{Bundle: "/bundles/x"}
	for _, a := range argvs {
		gc.PostStartCommands = append(gc.PostStartCommands,
			&agentv1.GuestConfig_PostStartCommand{Argv: a})
	}
	return gc
}

func TestStartPostStartNoOp(t *testing.T) {
	ex := &recordingExec{}
	d := runDeps{attach: attachDeps{exec: ex}}

	// No commands → no-op.
	d.cfg = &agentv1.GuestConfig{Bundle: "/bundles/x"}
	startPostStart(context.Background(), &bytes.Buffer{}, d)

	// Commands but no bundle → no-op (nothing to exec into).
	d.cfg = &agentv1.GuestConfig{
		PostStartCommands: []*agentv1.GuestConfig_PostStartCommand{{Argv: []string{psCmd}}},
	}
	startPostStart(context.Background(), &bytes.Buffer{}, d)

	if got := ex.calls(); len(got) != 0 {
		t.Fatalf("expected no exec calls, got %d", len(got))
	}
}

func TestRunPostStartRunsCommandsInOrder(t *testing.T) {
	stubRunning(t, true)
	ex := &recordingExec{results: []postStartResult{{out: "first-out\n"}, {out: "second-out\n"}}}
	d := runDeps{
		cfg:    postStartCfg([]string{"cmd-one"}, []string{"cmd-two", "arg"}),
		attach: attachDeps{exec: ex},
	}
	var buf bytes.Buffer

	runPostStart(context.Background(), &buf, d, containerProcess{Env: []string{"FOO=bar"}, User: testExecUser})

	calls := ex.calls()
	if len(calls) != 2 {
		t.Fatalf("want 2 exec calls, got %d", len(calls))
	}
	if strings.Join(calls[0].Command, " ") != "cmd-one" ||
		strings.Join(calls[1].Command, " ") != "cmd-two arg" {
		t.Fatalf("commands out of order: %+v", calls)
	}
	// Container env + user are forwarded to the exec.
	if len(calls[0].Env) != 1 || calls[0].Env[0] != "FOO=bar" || calls[0].User != testExecUser {
		t.Errorf("env/user not forwarded: %+v", calls[0])
	}
	// Output is streamed to the console under the post-start prefix.
	if !strings.Contains(buf.String(), "first-out") || !strings.Contains(buf.String(), "second-out") {
		t.Errorf("output not streamed: %q", buf.String())
	}
}

func TestRunPostStartNonZeroIsLoggedNotFatal(t *testing.T) {
	stubRunning(t, true)
	ex := &recordingExec{results: []postStartResult{{code: 1}, {out: "ok\n"}}}
	d := runDeps{
		cfg:    postStartCfg([]string{"false"}, []string{"do-ok"}),
		attach: attachDeps{exec: ex},
	}
	var buf bytes.Buffer

	runPostStart(context.Background(), &buf, d, containerProcess{})

	// The failing first command is logged but the second still runs.
	if !strings.Contains(buf.String(), "non-zero") {
		t.Errorf("non-zero exit not logged: %q", buf.String())
	}
	if len(ex.calls()) != 2 {
		t.Errorf("second command should still run after a failure; calls=%d", len(ex.calls()))
	}
}

func TestRunPostStartStartErrorLogged(t *testing.T) {
	stubRunning(t, true)
	ex := &recordingExec{results: []postStartResult{{startErr: errors.New("boom")}}}
	d := runDeps{cfg: postStartCfg([]string{psCmd}), attach: attachDeps{exec: ex}}
	var buf bytes.Buffer

	runPostStart(context.Background(), &buf, d, containerProcess{})

	if !strings.Contains(buf.String(), "start: boom") {
		t.Errorf("start error not logged: %q", buf.String())
	}
}

func TestAwaitContainerRunningTimeout(t *testing.T) {
	stubRunning(t, false)
	prevTO, prevPoll := postStartReadyTimeout, postStartPollInterval
	postStartReadyTimeout, postStartPollInterval = 20*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { postStartReadyTimeout, postStartPollInterval = prevTO, prevPoll })

	var buf bytes.Buffer
	if awaitContainerRunning(context.Background(), &buf, PostStartReportPrefix, "container did not reach running") {
		t.Fatalf("await should time out when container never runs")
	}
	if !strings.Contains(buf.String(), "did not reach running") {
		t.Errorf("timeout not reported: %q", buf.String())
	}
}

func TestAwaitContainerRunningCancel(t *testing.T) {
	stubRunning(t, false)
	prevPoll := postStartPollInterval
	postStartPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { postStartPollInterval = prevPoll })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if awaitContainerRunning(ctx, &bytes.Buffer{}, PostStartReportPrefix, "not ready") {
		t.Fatalf("await should return false on cancelled ctx")
	}
}

// TestContainerRunning covers the production crun-state probe via the
// TestMain fake-crun: the test binary re-execs itself printing the JSON
// set in fakeCrunEnv.
func TestContainerRunning(t *testing.T) {
	withTestRuntime(t, os.Args[0])
	t.Setenv(fakeCrunEnv, `{"status":"running"}`)
	if !containerRunning(context.Background()) {
		t.Errorf("status=running should report running")
	}

	t.Setenv(fakeCrunEnv, `{"status":"created"}`)
	if containerRunning(context.Background()) {
		t.Errorf("status=created should not report running")
	}
}

func TestContainerRunningFalseOnError(t *testing.T) {
	withTestRuntime(t, "/nonexistent/crun")
	if containerRunning(context.Background()) {
		t.Errorf("a missing runtime should report not-running, not panic")
	}
}

// TestStartPostStartRunsViaGoroutine covers the spawn path: with a bundle,
// commands, and a running container, the detached goroutine execs the
// command. Polled because startPostStart returns before the goroutine runs.
func TestStartPostStartRunsViaGoroutine(t *testing.T) {
	stubRunning(t, true)
	ex := &recordingExec{}
	d := runDeps{cfg: postStartCfg([]string{psCmd}), attach: attachDeps{exec: ex}}

	startPostStart(context.Background(), &bytes.Buffer{}, d)

	var calls int
	for i := 0; i < 400 && calls == 0; i++ {
		calls = len(ex.calls())
		time.Sleep(5 * time.Millisecond)
	}
	if calls != 1 {
		t.Fatalf("startPostStart goroutine should exec the command once; calls=%d", calls)
	}
}

func TestRunPostStartSkipsWhenNeverRunning(t *testing.T) {
	stubRunning(t, false)
	prevTO, prevPoll := postStartReadyTimeout, postStartPollInterval
	postStartReadyTimeout, postStartPollInterval = 20*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { postStartReadyTimeout, postStartPollInterval = prevTO, prevPoll })

	ex := &recordingExec{}
	d := runDeps{cfg: postStartCfg([]string{psCmd}), attach: attachDeps{exec: ex}}
	runPostStart(context.Background(), &bytes.Buffer{}, d, containerProcess{})
	if len(ex.calls()) != 0 {
		t.Fatalf("commands must not run when container never starts; calls=%d", len(ex.calls()))
	}
}
