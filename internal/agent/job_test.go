package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
)

type fakeJobExec struct {
	mu      sync.Mutex
	req     JobExecRequest
	process JobProcess
	err     error
}

func TestUnavailableJobExec(t *testing.T) {
	if _, err := (unavailableJobExec{}).Start(t.Context(), JobExecRequest{}); !errors.Is(err, ErrJobExecUnavailable) {
		t.Fatalf("Start error = %v", err)
	}
}

func (e *fakeJobExec) Start(_ context.Context, req JobExecRequest) (JobProcess, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.req = req
	return e.process, e.err
}

type fakeJobProcess struct {
	stdout io.Reader
	stderr io.Reader
	code   int32
	werr   error
	wait   chan struct{}

	cancelOnce sync.Once
	canceled   chan struct{}
}

type gatedJobReader struct {
	started chan<- struct{}
	release <-chan struct{}
}

func (r gatedJobReader) Read([]byte) (int, error) {
	r.started <- struct{}{}
	<-r.release
	return 0, io.EOF
}

type drainOrderedJobProcess struct {
	stdout, stderr io.Reader
	waitCalled     chan struct{}
}

func (p *drainOrderedJobProcess) Stdout() io.Reader { return p.stdout }
func (p *drainOrderedJobProcess) Stderr() io.Reader { return p.stderr }
func (p *drainOrderedJobProcess) Wait() (int32, error) {
	close(p.waitCalled)
	return 0, nil
}
func (p *drainOrderedJobProcess) Cancel() error { return nil }

func (p *fakeJobProcess) Stdout() io.Reader { return p.stdout }
func (p *fakeJobProcess) Stderr() io.Reader { return p.stderr }
func (p *fakeJobProcess) Wait() (int32, error) {
	if p.wait != nil {
		<-p.wait
	}
	return p.code, p.werr
}

func (p *fakeJobProcess) Cancel() error {
	p.cancelOnce.Do(func() {
		if p.canceled != nil {
			close(p.canceled)
		}
		if p.wait != nil {
			close(p.wait)
		}
	})
	return nil
}

func TestHandleJobSeparatesOutputAndExit(t *testing.T) {
	host, guest := net.Pipe()
	t.Cleanup(func() { _ = host.Close() })
	proc := &fakeJobProcess{stdout: bytes.NewBufferString("out"), stderr: bytes.NewBufferString("err"), code: 7}
	executor := &fakeJobExec{process: proc}
	done := make(chan struct{})
	go func() {
		handleJob(t.Context(), io.Discard, guest, executor, containerProcess{Env: []string{testEnvAssignment}, User: testExecUser})
		close(done)
	}()
	if err := WriteJobHello(host, &agentv1.JobHello{Command: []string{"agent", "work"}}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr string
	var exit *agentv1.JobExit
	for exit == nil {
		frame, err := ReadJobFrame(host)
		if err != nil {
			t.Fatalf("ReadJobFrame: %v", err)
		}
		stdout += string(frame.GetStdout())
		stderr += string(frame.GetStderr())
		exit = frame.GetExit()
	}
	if stdout != "out" || stderr != "err" || exit.GetCode() != 7 || exit.GetError() != "" {
		t.Fatalf("stdout=%q stderr=%q exit=%+v", stdout, stderr, exit)
	}
	executor.mu.Lock()
	req := executor.req
	executor.mu.Unlock()
	if len(req.Command) != 2 || req.Env[0] != testEnvAssignment || req.User != testExecUser {
		t.Fatalf("request = %+v", req)
	}
	<-done
}

func TestHandleJobDrainsOutputBeforeWait(t *testing.T) {
	host, guest := net.Pipe()
	defer func() { _ = host.Close() }()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	waitCalled := make(chan struct{})
	reader := gatedJobReader{started: started, release: release}
	proc := &drainOrderedJobProcess{stdout: reader, stderr: reader, waitCalled: waitCalled}
	go handleJob(t.Context(), io.Discard, guest, &fakeJobExec{process: proc}, containerProcess{})
	if err := WriteJobHello(host, &agentv1.JobHello{Command: []string{"short-lived"}}); err != nil {
		t.Fatal(err)
	}
	<-started
	<-started
	select {
	case <-waitCalled:
		t.Fatal("Wait called before stdout and stderr reached EOF")
	default:
	}
	close(release)
	frame, err := ReadJobFrame(host)
	if err != nil || frame.GetExit() == nil {
		t.Fatalf("exit frame = %+v, %v", frame, err)
	}
	select {
	case <-waitCalled:
	case <-time.After(time.Second):
		t.Fatal("Wait was not called after output drained")
	}
}

func TestHandleJobReportsStartAndWaitFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		exec *fakeJobExec
		want string
	}{
		{name: "start", exec: &fakeJobExec{err: errors.New("start failed")}, want: "job exec start"},
		{name: "wait", exec: &fakeJobExec{process: &fakeJobProcess{stdout: bytes.NewReader(nil), stderr: bytes.NewReader(nil), werr: errors.New("wait failed")}}, want: "job exec wait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, guest := net.Pipe()
			defer func() { _ = host.Close() }()
			go handleJob(t.Context(), io.Discard, guest, tc.exec, containerProcess{})
			if err := WriteJobHello(host, &agentv1.JobHello{Command: []string{"x"}}); err != nil {
				t.Fatal(err)
			}
			frame, err := ReadJobFrame(host)
			if err != nil {
				t.Fatal(err)
			}
			if exit := frame.GetExit(); exit == nil || !bytes.Contains([]byte(exit.GetError()), []byte(tc.want)) {
				t.Fatalf("exit = %+v, want error containing %q", exit, tc.want)
			}
		})
	}
}

func TestHandleJobHostCloseCancelsProcess(t *testing.T) {
	host, guest := net.Pipe()
	proc := &fakeJobProcess{
		stdout: bytes.NewReader(nil), stderr: bytes.NewReader(nil),
		wait: make(chan struct{}), canceled: make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		handleJob(context.Background(), io.Discard, guest, &fakeJobExec{process: proc}, containerProcess{})
		close(done)
	}()
	if err := WriteJobHello(host, &agentv1.JobHello{Command: []string{"sleep"}}); err != nil {
		t.Fatal(err)
	}
	_ = host.Close()
	select {
	case <-proc.canceled:
	case <-time.After(time.Second):
		t.Fatal("process was not canceled after host close")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handleJob did not return")
	}
}

func TestHandleJobMalformedHelloReturnsExit(t *testing.T) {
	host, guest := net.Pipe()
	defer func() { _ = host.Close() }()
	go handleJob(t.Context(), io.Discard, guest, &fakeJobExec{}, containerProcess{})
	if err := writeAttachLenPrefixed(host, []byte{0xff}); err != nil {
		t.Fatal(err)
	}
	frame, err := ReadJobFrame(host)
	if err != nil {
		t.Fatal(err)
	}
	if exit := frame.GetExit(); exit == nil || exit.GetError() == "" {
		t.Fatalf("exit = %+v", exit)
	}
}

func TestJobFramingRoundTripAndMalformed(t *testing.T) {
	var buf bytes.Buffer
	wantHello := &agentv1.JobHello{Command: []string{"a", "b"}}
	if err := WriteJobHello(&buf, wantHello); err != nil {
		t.Fatal(err)
	}
	gotHello, err := ReadJobHello(&buf)
	if err != nil || len(gotHello.GetCommand()) != 2 {
		t.Fatalf("hello = %+v, %v", gotHello, err)
	}
	wantFrame := &agentv1.JobFrame{Body: &agentv1.JobFrame_Stderr{Stderr: []byte("bad")}}
	if err := WriteJobFrame(&buf, wantFrame); err != nil {
		t.Fatal(err)
	}
	gotFrame, err := ReadJobFrame(&buf)
	if err != nil || string(gotFrame.GetStderr()) != "bad" {
		t.Fatalf("frame = %+v, %v", gotFrame, err)
	}
	buf.Reset()
	_ = writeAttachLenPrefixed(&buf, []byte{0xff})
	if _, err := ReadJobFrame(&buf); err == nil {
		t.Fatal("malformed frame succeeded")
	}
}

func TestStartJobListenerWiringAndDisabled(t *testing.T) {
	oldContainerRunning := containerRunning
	containerRunning = func(context.Context) bool { return true }
	t.Cleanup(func() { containerRunning = oldContainerRunning })
	ln := newFakeVsockListener(DefaultJobVsockPort)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	startJobListener(ctx, io.Discard, runDeps{
		cfg: &agentv1.GuestConfig{AttachEnabled: true},
		job: jobDeps{listenVsock: func(port uint32) (net.Listener, error) {
			if port != DefaultJobVsockPort {
				t.Fatalf("port = %d", port)
			}
			return ln, nil
		}, exec: &fakeJobExec{err: errors.New("expected")}},
	})
	host, guest := net.Pipe()
	ln.push(guest)
	if err := WriteJobHello(host, &agentv1.JobHello{Command: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	if frame, err := ReadJobFrame(host); err != nil || frame.GetExit() == nil {
		t.Fatalf("frame=%+v err=%v", frame, err)
	}
	_ = host.Close()

	called := false
	startJobListener(t.Context(), io.Discard, runDeps{
		cfg: &agentv1.GuestConfig{},
		job: jobDeps{listenVsock: func(uint32) (net.Listener, error) { called = true; return nil, errShouldNotListen }, exec: &fakeJobExec{}},
	})
	if called {
		t.Fatal("disabled job listener called listen")
	}
}

func TestRunJobListenerReportsListenAndAcceptErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps jobDeps
	}{
		{name: "listen", deps: jobDeps{listenVsock: func(uint32) (net.Listener, error) {
			return nil, errors.New("bind failed")
		}, exec: &fakeJobExec{}}},
		{name: "accept", deps: jobDeps{listenVsock: func(uint32) (net.Listener, error) {
			return newErrFakeListener(errors.New("accept failed")), nil
		}, exec: &fakeJobExec{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			runJobListener(t.Context(), &out, 9191, tc.deps, containerProcess{})
			if out.Len() == 0 {
				t.Fatal("listener failure was not reported")
			}
		})
	}
}

// A writer that reports a short write without an error must still yield a
// transport error, whether the writer is a downstream consumer or the wire.
type shortJobWriter struct{}

func (shortJobWriter) Write(data []byte) (int, error) { return len(data) / 2, nil }

func TestReadJobOutputRejectsShortWrites(t *testing.T) {
	for _, stderr := range []bool{false, true} {
		var wire bytes.Buffer
		output := jobOutput{conn: &wire}
		stream := output.stdout()
		if stderr {
			stream = output.stderr()
		}
		if _, err := stream.Write([]byte("output")); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadJobOutput(&wire, shortJobWriter{}, shortJobWriter{}); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("stderr=%t: expected short write, got %v", stderr, err)
		}
	}
	if err := WriteJobFrame(shortJobWriter{}, &agentv1.JobFrame{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected short wire write, got %v", err)
	}
}

func TestReadJobOutputDecodesExitAndRejectsUnexpectedFrames(t *testing.T) {
	var wire, stdout, stderr bytes.Buffer
	output := jobOutput{conn: &wire}
	if _, err := output.stdout().Write([]byte("standard output")); err != nil {
		t.Fatal(err)
	}
	if _, err := output.stderr().Write([]byte("standard error")); err != nil {
		t.Fatal(err)
	}
	if err := writeJobExit(&wire, &agentv1.JobExit{Code: 17, Error: "guest failure"}); err != nil {
		t.Fatal(err)
	}

	exit, err := ReadJobOutput(&wire, &stdout, &stderr)
	if err != nil || exit.GetCode() != 17 || exit.GetError() != "guest failure" {
		t.Fatalf("exit = %v, %v", exit, err)
	}
	if stdout.String() != "standard output" || stderr.String() != "standard error" {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}

	if _, err := ReadJobOutput(&wire, io.Discard, io.Discard); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF before exit, got %v", err)
	}
	if err := WriteJobFrame(&wire, &agentv1.JobFrame{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadJobOutput(&wire, io.Discard, io.Discard); err == nil {
		t.Fatal("empty output frame accepted")
	}
}
