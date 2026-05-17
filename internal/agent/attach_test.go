package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
)

// fakeAttachProcess wraps an io.Pipe pair as an AttachProcess so
// tests can drive bytes both directions without spawning a real
// PTY. Reads come from the producer end; writes go into a
// receiving buffer the test can inspect.
type fakeAttachProcess struct {
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter

	mu       sync.Mutex
	stdinBuf bytes.Buffer
	resizes  []fakeResize
	exitCode int32
	exitErr  error

	closeOnce sync.Once
	closed    chan struct{}

	// waitGate, if non-nil, blocks Wait() until closed. Lets a
	// test that needs to observe a partial-pump state hold Wait
	// open while it inspects the listener.
	waitGate chan struct{}
}

type fakeResize struct {
	cols, rows uint32
}

func newFakeAttachProcess(exitCode int32) *fakeAttachProcess {
	r, w := io.Pipe()
	return &fakeAttachProcess{
		stdoutR:  r,
		stdoutW:  w,
		exitCode: exitCode,
		closed:   make(chan struct{}),
	}
}

func (p *fakeAttachProcess) Read(b []byte) (int, error) { return p.stdoutR.Read(b) }

func (p *fakeAttachProcess) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.closed:
		return 0, io.ErrClosedPipe
	default:
	}
	return p.stdinBuf.Write(b)
}

func (p *fakeAttachProcess) Resize(cols, rows uint32) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.resizes = append(p.resizes, fakeResize{cols: cols, rows: rows})
	return nil
}

func (p *fakeAttachProcess) Wait() (int32, error) {
	if p.waitGate != nil {
		<-p.waitGate
	}
	<-p.closed
	return p.exitCode, p.exitErr
}

func (p *fakeAttachProcess) Close() error {
	p.closeOnce.Do(func() {
		_ = p.stdoutR.Close()
		_ = p.stdoutW.Close()
		close(p.closed)
	})
	return nil
}

// stdinBytes returns a copy of whatever the listener has written
// to the process's stdin so far.
func (p *fakeAttachProcess) stdinBytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]byte, p.stdinBuf.Len())
	copy(out, p.stdinBuf.Bytes())
	return out
}

func (p *fakeAttachProcess) resizeEvents() []fakeResize {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]fakeResize, len(p.resizes))
	copy(out, p.resizes)
	return out
}

// fakeAttachExec returns the pre-constructed AttachProcess once.
// startErr lets a test substitute a Start failure path.
type fakeAttachExec struct {
	mu          sync.Mutex
	proc        AttachProcess
	gotReq      AttachExecRequest
	gotReqSet   bool
	startErr    error
	startCalled int
}

func (e *fakeAttachExec) Start(_ context.Context, req AttachExecRequest) (AttachProcess, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.startCalled++
	e.gotReq = req
	e.gotReqSet = true
	if e.startErr != nil {
		return nil, e.startErr
	}
	return e.proc, nil
}

func (e *fakeAttachExec) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.startCalled
}

func (e *fakeAttachExec) request() (AttachExecRequest, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gotReq, e.gotReqSet
}

// TestParseAttachPortCmdlineRoundtrip pins the wire form.
func TestParseAttachPortCmdlineRoundtrip(t *testing.T) {
	cmdline := FormatAttachPortCmdline(4242)
	port, ok, err := ParseAttachPortCmdline(cmdline)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !ok {
		t.Fatal("ok=false on a well-formed token")
	}
	if port != 4242 {
		t.Errorf("port = %d, want 4242", port)
	}
}

// TestParseAttachPortCmdlineAbsent → ok=false with no error.
func TestParseAttachPortCmdlineAbsent(t *testing.T) {
	port, ok, err := ParseAttachPortCmdline("daft.bundle=/foo")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ok {
		t.Errorf("ok=true on absent token (got port=%d)", port)
	}
}

// TestParseAttachPortCmdlineRejectsBadInput pins every malformed-
// value branch so a typo on the host surfaces.
func TestParseAttachPortCmdlineRejectsBadInput(t *testing.T) {
	cases := []struct {
		name    string
		cmdline string
		want    string
	}{
		{"empty", AttachPortCmdlineKey + "=", "empty"},
		{"non-numeric", AttachPortCmdlineKey + "=abc", `"abc"`},
		{"zero", AttachPortCmdlineKey + "=0", "non-zero"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseAttachPortCmdline(tc.cmdline)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestAttachFrameRoundtrip pins the wire format end-to-end.
func TestAttachFrameRoundtrip(t *testing.T) {
	var buf bytes.Buffer
	hello := &agentv1.AgentAttachHello{
		Command: []string{testShellPath, "-l"},
		Tty:     true,
		Cols:    80,
		Rows:    24,
	}
	if err := WriteAttachHello(&buf, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	got, err := readAttachHello(&buf)
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	if got.GetCols() != 80 || got.GetRows() != 24 || !got.GetTty() {
		t.Errorf("hello round-trip mismatch: %+v", got)
	}
	if len(got.GetCommand()) != 2 || got.GetCommand()[1] != "-l" {
		t.Errorf("command mismatch: %+v", got.GetCommand())
	}

	dataFrame := &agentv1.AgentAttachFrame{
		Body: &agentv1.AgentAttachFrame_Data{Data: []byte("hi")},
	}
	if err := WriteAttachFrame(&buf, dataFrame); err != nil {
		t.Fatalf("write data frame: %v", err)
	}
	gotFrame, err := ReadAttachFrame(&buf)
	if err != nil {
		t.Fatalf("read data frame: %v", err)
	}
	if !bytes.Equal(gotFrame.GetData(), []byte("hi")) {
		t.Errorf("data round-trip: got %q want %q", gotFrame.GetData(), "hi")
	}
}

// TestReadAttachPayloadRejectsOversizedPrefix pins the
// allocation-bound check: a hostile producer must not be able to
// make the agent allocate gigabytes by claiming a huge frame.
func TestReadAttachPayloadRejectsOversizedPrefix(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], attachMaxFrame+1)
	buf.Write(hdr[:])
	if _, err := readAttachPayload(&buf); err == nil {
		t.Fatal("expected error on oversized frame")
	}
}

// TestReadAttachPayloadAllowsZeroLength pins that an all-default
// proto message (which marshals to zero bytes) round-trips cleanly:
// the listener must not reject it.
func TestReadAttachPayloadAllowsZeroLength(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 0)
	buf.Write(hdr[:])
	got, err := readAttachPayload(&buf)
	if err != nil {
		t.Fatalf("read zero-length payload: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("payload = %v, want empty", got)
	}
}

// TestHandleAttachSplicesBothWays drives a full session over an
// in-process net.Pipe: client sends Hello + a data frame; agent
// pumps it onto the process's stdin; process writes a reply onto
// stdout; agent writes it back as a Data frame; client receives.
// Closing the process drives the Exit frame.
func TestHandleAttachSplicesBothWays(t *testing.T) {
	proc := newFakeAttachProcess(0)
	exec := &fakeAttachExec{proc: proc}

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var w bytes.Buffer
		handleAttach(ctx, &w, server, exec)
	}()

	hello := &agentv1.AgentAttachHello{
		Command: []string{testShellPath},
		Tty:     true,
		Cols:    80,
		Rows:    24,
	}
	if err := WriteAttachHello(client, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	// client → process.
	if err := WriteAttachFrame(client, &agentv1.AgentAttachFrame{
		Body: &agentv1.AgentAttachFrame_Data{Data: []byte("ping")},
	}); err != nil {
		t.Fatalf("write data: %v", err)
	}

	// Wait for the process to see the bytes.
	waitFor(t, time.Second, func() bool {
		return bytes.Equal(proc.stdinBytes(), []byte("ping"))
	}, "process did not receive 'ping'")

	// process → client.
	go func() { _, _ = proc.stdoutW.Write([]byte("pong")) }()
	frame := readFrameOrFail(t, client)
	if !bytes.Equal(frame.GetData(), []byte("pong")) {
		t.Errorf("client got data %q, want %q", frame.GetData(), "pong")
	}

	// Close the process to trigger the Exit path.
	_ = proc.Close()

	exitFrame := readFrameOrFail(t, client)
	exit := exitFrame.GetExit()
	if exit == nil {
		t.Fatalf("expected Exit frame, got %T", exitFrame.GetBody())
	}
	if exit.GetCode() != 0 || exit.GetError() != "" {
		t.Errorf("exit = {code=%d err=%q}, want {0, \"\"}", exit.GetCode(), exit.GetError())
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleAttach did not return within 2s")
	}

	if calls := exec.calls(); calls != 1 {
		t.Errorf("exec.Start called %d times, want 1", calls)
	}
	req, ok := exec.request()
	if !ok {
		t.Fatal("exec.Start was never called")
	}
	if !req.TTY || req.Cols != 80 || req.Rows != 24 {
		t.Errorf("AttachExecRequest mismatch: %+v", req)
	}
}

// TestHandleAttachForwardsResize verifies that a Resize frame
// reaches the process's Resize side-channel rather than being
// fed as data.
func TestHandleAttachForwardsResize(t *testing.T) {
	proc := newFakeAttachProcess(0)
	exec := &fakeAttachExec{proc: proc}

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var w bytes.Buffer
		handleAttach(ctx, &w, server, exec)
	}()

	if err := WriteAttachHello(client, &agentv1.AgentAttachHello{Tty: true, Cols: 80, Rows: 24}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if err := WriteAttachFrame(client, &agentv1.AgentAttachFrame{
		Body: &agentv1.AgentAttachFrame_Resize{
			Resize: &agentv1.AgentAttachResize{Cols: 132, Rows: 50},
		},
	}); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	waitFor(t, time.Second, func() bool {
		ev := proc.resizeEvents()
		return len(ev) == 1 && ev[0].cols == 132 && ev[0].rows == 50
	}, "process did not see resize")

	_ = proc.Close()
	_ = readFrameOrFail(t, client) // drain Exit
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleAttach did not return")
	}
}

// TestHandleAttachReportsExecStartError pins the error path: a
// failing Start surfaces as an Exit frame with a non-empty error
// string so the client sees the failure.
func TestHandleAttachReportsExecStartError(t *testing.T) {
	exec := &fakeAttachExec{startErr: errors.New("boom")}

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var w bytes.Buffer
		handleAttach(ctx, &w, server, exec)
	}()

	if err := WriteAttachHello(client, &agentv1.AgentAttachHello{}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	frame := readFrameOrFail(t, client)
	exit := frame.GetExit()
	if exit == nil {
		t.Fatalf("expected Exit frame, got %T", frame.GetBody())
	}
	if exit.GetCode() == 0 {
		t.Errorf("expected non-zero exit code, got 0")
	}
	if !strings.Contains(exit.GetError(), "boom") {
		t.Errorf("exit error %q missing 'boom'", exit.GetError())
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleAttach did not return")
	}
}

// TestHandleAttachReportsResizeError pins the non-fatal Resize
// path: a Resize whose handler errors logs to the console and the
// session continues (subsequent Data still reaches the process).
func TestHandleAttachReportsResizeError(t *testing.T) {
	proc := newFakeAttachProcess(0)
	proc.exitErr = nil
	resizeErr := errors.New("ioctl TIOCSWINSZ: bad fd")
	procWrap := &resizeFailingProc{fakeAttachProcess: proc, err: resizeErr}
	exec := &fakeAttachExec{proc: procWrap}

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	var w bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleAttach(context.Background(), &w, server, exec)
	}()

	if err := WriteAttachHello(client, &agentv1.AgentAttachHello{Tty: true, Cols: 80, Rows: 24}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if err := WriteAttachFrame(client, &agentv1.AgentAttachFrame{
		Body: &agentv1.AgentAttachFrame_Resize{
			Resize: &agentv1.AgentAttachResize{Cols: 100, Rows: 30},
		},
	}); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	if err := WriteAttachFrame(client, &agentv1.AgentAttachFrame{
		Body: &agentv1.AgentAttachFrame_Data{Data: []byte("after-resize")},
	}); err != nil {
		t.Fatalf("write data after resize: %v", err)
	}
	waitFor(t, time.Second, func() bool {
		return bytes.Equal(proc.stdinBytes(), []byte("after-resize"))
	}, "data after resize never reached proc")

	_ = proc.Close()
	_ = readFrameOrFail(t, client)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleAttach did not return")
	}
	if !strings.Contains(w.String(), "resize") {
		t.Errorf("console output %q missing resize error", w.String())
	}
}

// TestHandleAttachClientExitFrameTreatedAsEOF: the client may send
// an Exit frame to signal "I'm done." Listener treats it as a
// graceful EOF — closes proc, writes its own Exit.
func TestHandleAttachClientExitFrameTreatedAsEOF(t *testing.T) {
	proc := newFakeAttachProcess(0)
	exec := &fakeAttachExec{proc: proc}

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	var w bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleAttach(context.Background(), &w, server, exec)
	}()

	if err := WriteAttachHello(client, &agentv1.AgentAttachHello{Tty: true, Cols: 80, Rows: 24}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	if err := WriteAttachFrame(client, &agentv1.AgentAttachFrame{
		Body: &agentv1.AgentAttachFrame_Exit{Exit: &agentv1.AgentAttachExit{Code: 0}},
	}); err != nil {
		t.Fatalf("write client exit: %v", err)
	}

	frame := readFrameOrFail(t, client)
	if frame.GetExit() == nil {
		t.Fatalf("expected Exit frame, got %T", frame.GetBody())
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleAttach did not return after client Exit")
	}
}

// TestHandleAttachWaitErrorSurfacedToClient pins the proc.Wait
// error path: the Exit frame carries a non-empty error string so
// the client sees the failure.
func TestHandleAttachWaitErrorSurfacedToClient(t *testing.T) {
	proc := newFakeAttachProcess(0)
	proc.exitErr = errors.New("crun: oom-killed")
	exec := &fakeAttachExec{proc: proc}

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	var w bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleAttach(context.Background(), &w, server, exec)
	}()

	if err := WriteAttachHello(client, &agentv1.AgentAttachHello{Tty: true, Cols: 80, Rows: 24}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	_ = proc.Close()

	frame := readFrameOrFail(t, client)
	exit := frame.GetExit()
	if exit == nil {
		t.Fatalf("expected Exit frame, got %T", frame.GetBody())
	}
	if !strings.Contains(exit.GetError(), "oom-killed") {
		t.Errorf("exit error %q missing 'oom-killed'", exit.GetError())
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleAttach did not return")
	}
}

// TestHandleAttachReportsHelloReadError covers the case where the
// client closes before sending Hello — the listener writes a best-
// effort Exit (which the closed conn discards) and returns cleanly.
func TestHandleAttachReportsHelloReadError(t *testing.T) {
	client, server := net.Pipe()
	_ = client.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		var w bytes.Buffer
		handleAttach(context.Background(), &w, server, &fakeAttachExec{})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleAttach did not return after client close")
	}
}

// TestUnavailableAttachExec pins the placeholder until D2b.2 lands.
func TestUnavailableAttachExec(t *testing.T) {
	_, err := unavailableAttachExec{}.Start(context.Background(), AttachExecRequest{})
	if !errors.Is(err, ErrAttachExecUnavailable) {
		t.Fatalf("got %v, want ErrAttachExecUnavailable", err)
	}
}

// TestStartAttachListenerFromCmdlineWiringHappyPath drives
// runWith via startAttachListenerFromCmdline end-to-end: a
// daft.attach_port token brings up a fake vsock listener and a
// pushed connection reaches handleAttach.
func TestStartAttachListenerFromCmdlineWiringHappyPath(t *testing.T) {
	ln := newFakeVsockListener(9001)
	proc := newFakeAttachProcess(0)
	exec := &fakeAttachExec{proc: proc}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var w bytes.Buffer

	startAttachListenerFromCmdline(ctx, &w, runDeps{
		attach: attachDeps{
			listenVsock: func(port uint32) (net.Listener, error) {
				if port != 9001 {
					t.Fatalf("listen port = %d, want 9001", port)
				}
				return ln, nil
			},
			exec: exec,
		},
	}, FormatAttachPortCmdline(9001))

	// Push a connection and drive a Hello.
	client, serverSide := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	ln.push(serverSide)

	if err := WriteAttachHello(client, &agentv1.AgentAttachHello{Tty: true, Cols: 80, Rows: 24}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	waitFor(t, time.Second, func() bool { return exec.calls() == 1 }, "exec.Start never called")

	_ = proc.Close()
	_ = readFrameOrFail(t, client) // drain Exit
	cancel()
}

// testShellPath is the literal `/bin/sh` reused across attach
// test fixtures, extracted to a const so goconst doesn't flag
// the repetition.
const testShellPath = "/bin/sh"

// errShouldNotListen is the sentinel returned by the listenVsock
// fakes in absent-token / malformed-cmdline tests, so a bug that
// reaches the call surfaces as a clear error rather than a
// confusing nil/nil pair. The tests themselves fail the test on
// any call, so this value's only job is to satisfy the nilnil
// linter.
var errShouldNotListen = errors.New("test: listenVsock should not be called")

// TestStartAttachListenerFromCmdlineAbsent: no token ⇒ no listen.
func TestStartAttachListenerFromCmdlineAbsent(t *testing.T) {
	called := false
	var w bytes.Buffer
	startAttachListenerFromCmdline(context.Background(), &w, runDeps{
		attach: attachDeps{
			listenVsock: func(uint32) (net.Listener, error) {
				called = true
				return nil, errShouldNotListen
			},
		},
	}, "daft.bundle=/foo")
	if called {
		t.Error("listenVsock was called for absent token")
	}
}

// TestStartAttachListenerFromCmdlineMalformedReportsToConsole
// pins the cmdline-error console line and verifies no listener
// is started.
func TestStartAttachListenerFromCmdlineMalformedReportsToConsole(t *testing.T) {
	var w bytes.Buffer
	startAttachListenerFromCmdline(context.Background(), &w, runDeps{
		attach: attachDeps{
			listenVsock: func(uint32) (net.Listener, error) {
				t.Fatal("listenVsock should not be called for malformed cmdline")
				return nil, errShouldNotListen
			},
		},
	}, AttachPortCmdlineKey+"=abc")
	if !strings.Contains(w.String(), attachReportPrefix) {
		t.Errorf("console output %q missing %q prefix", w.String(), attachReportPrefix)
	}
}

// TestRunAttachListenerReportsListenError pins the bind-failure
// branch: listenVsock errors are reported on the console and the
// goroutine exits without spawning an accept loop.
func TestRunAttachListenerReportsListenError(t *testing.T) {
	var w bytes.Buffer
	runAttachListener(context.Background(), &w, 9999, attachDeps{
		listenVsock: func(uint32) (net.Listener, error) {
			return nil, errors.New("bind: address in use")
		},
	})
	if !strings.Contains(w.String(), "bind: address in use") {
		t.Errorf("console output %q missing bind error", w.String())
	}
}

// TestHandleAttachUnmarshalHelloError pins the path where a
// malformed Hello payload (valid length prefix + garbage bytes)
// surfaces as an Exit frame with an "unmarshal" error.
func TestHandleAttachUnmarshalHelloError(t *testing.T) {
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		var w bytes.Buffer
		handleAttach(context.Background(), &w, server, &fakeAttachExec{})
	}()

	// 0xff bytes are invalid as a proto wire format.
	bad := []byte{0xff, 0xff, 0xff, 0xff}
	if err := writeAttachLenPrefixed(client, bad); err != nil {
		t.Fatalf("write bad hello: %v", err)
	}
	frame := readFrameOrFail(t, client)
	exit := frame.GetExit()
	if exit == nil {
		t.Fatalf("expected Exit frame, got %T", frame.GetBody())
	}
	if !strings.Contains(exit.GetError(), "unmarshal") && !strings.Contains(exit.GetError(), "hello") {
		t.Errorf("exit error %q missing unmarshal/hello hint", exit.GetError())
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleAttach did not return")
	}
}

// TestPumpAttachConnToProcessUnknownFrameType drives a frame with
// no body set — that hits the switch default and exits the pump.
func TestPumpAttachConnToProcessUnknownFrameType(t *testing.T) {
	proc := newFakeAttachProcess(0)
	t.Cleanup(func() { _ = proc.Close() })

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	done := make(chan struct{})
	var w bytes.Buffer
	go func() {
		defer close(done)
		pumpAttachConnToProcess(&w, server, proc)
	}()

	// Empty AgentAttachFrame: no body oneof set.
	if err := WriteAttachFrame(client, &agentv1.AgentAttachFrame{}); err != nil {
		t.Fatalf("write empty frame: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not return on unknown frame")
	}
	if !strings.Contains(w.String(), "unexpected frame") {
		t.Errorf("console output %q missing 'unexpected frame'", w.String())
	}
}

// procWriteFailing wraps fakeAttachProcess so its Write returns a
// canned error — exercises the proc.Write error branch in
// pumpAttachConnToProcess.
type procWriteFailing struct {
	*fakeAttachProcess
	err error
}

func (p *procWriteFailing) Write([]byte) (int, error) { return 0, p.err }

// TestPumpAttachConnToProcessProcWriteError pins the path where a
// proc.Write fails (non-pump-exit error): pump exits and logs.
func TestPumpAttachConnToProcessProcWriteError(t *testing.T) {
	base := newFakeAttachProcess(0)
	t.Cleanup(func() { _ = base.Close() })
	proc := &procWriteFailing{fakeAttachProcess: base, err: errors.New("disk full")}

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	done := make(chan struct{})
	var w bytes.Buffer
	go func() {
		defer close(done)
		pumpAttachConnToProcess(&w, server, proc)
	}()

	if err := WriteAttachFrame(client, &agentv1.AgentAttachFrame{
		Body: &agentv1.AgentAttachFrame_Data{Data: []byte("payload")},
	}); err != nil {
		t.Fatalf("write data: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not return on proc write error")
	}
	if !strings.Contains(w.String(), "disk full") {
		t.Errorf("console output %q missing proc write error", w.String())
	}
}

// procReadFailing yields a single non-EOF read error.
type procReadFailing struct {
	*fakeAttachProcess
	err error
}

func (p *procReadFailing) Read([]byte) (int, error) { return 0, p.err }

// errFakeListener returns the canned error from Accept once, and
// blocks on Accept thereafter. Drives the non-ErrClosed branch of
// attachAcceptLoop.
type errFakeListener struct {
	once sync.Once
	err  error
	done chan struct{}
}

func newErrFakeListener(err error) *errFakeListener {
	return &errFakeListener{err: err, done: make(chan struct{})}
}

func (l *errFakeListener) Accept() (net.Conn, error) {
	var fire bool
	l.once.Do(func() { fire = true })
	if fire {
		return nil, l.err
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *errFakeListener) Close() error   { close(l.done); return nil }
func (l *errFakeListener) Addr() net.Addr { return fakeAddr{net: "fake", str: "fake"} }

// TestAttachAcceptLoopReportsNonClosedError pins the console
// report path when Accept yields an unexpected error.
func TestAttachAcceptLoopReportsNonClosedError(t *testing.T) {
	ln := newErrFakeListener(errors.New("accept: too many open files"))
	t.Cleanup(func() { _ = ln.Close() })

	var w bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		attachAcceptLoop(context.Background(), &w, ln, &fakeAttachExec{})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("accept loop did not return on unexpected error")
	}
	if !strings.Contains(w.String(), "too many open files") {
		t.Errorf("console output %q missing accept error", w.String())
	}
}

// TestPumpAttachConnToProcessReadFrameError pins the log branch
// where the conn yields a non-pump-exit read error (an oversized
// length prefix triggers readAttachPayload's cap check).
func TestPumpAttachConnToProcessReadFrameError(t *testing.T) {
	proc := newFakeAttachProcess(0)
	t.Cleanup(func() { _ = proc.Close() })

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })

	done := make(chan struct{})
	var w bytes.Buffer
	go func() {
		defer close(done)
		pumpAttachConnToProcess(&w, server, proc)
	}()

	// Send a length prefix that exceeds attachMaxFrame.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], attachMaxFrame+1)
	go func() { _, _ = client.Write(hdr[:]) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not return on oversized frame")
	}
	if !strings.Contains(w.String(), "read frame") {
		t.Errorf("console output %q missing 'read frame' log", w.String())
	}
}

// failingConn always errors on Write so we can drive the
// writeAttachData error branch in pumpAttachProcessToConn.
type failingConn struct {
	net.Conn
	writeErr error
}

func (c *failingConn) Write([]byte) (int, error) { return 0, c.writeErr }

// TestPumpAttachProcessToConnWriteFrameError pins the path where
// the conn's Write fails mid-pump (non-pump-exit error) — the
// console gets a single "write data frame" log line and the pump
// exits.
func TestPumpAttachProcessToConnWriteFrameError(t *testing.T) {
	proc := newFakeAttachProcess(0)
	t.Cleanup(func() { _ = proc.Close() })

	_, server := net.Pipe()
	fail := &failingConn{Conn: server, writeErr: errors.New("nic offline")}

	done := make(chan struct{})
	var w bytes.Buffer
	go func() {
		defer close(done)
		pumpAttachProcessToConn(&w, fail, proc)
	}()
	// Drive a write through the proc.
	go func() { _, _ = proc.stdoutW.Write([]byte("data")) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not return on conn write error")
	}
	if !strings.Contains(w.String(), "nic offline") {
		t.Errorf("console output %q missing conn write error", w.String())
	}
}

// TestPumpAttachProcessToConnReadError pins the non-EOF proc.Read
// error branch.
func TestPumpAttachProcessToConnReadError(t *testing.T) {
	base := newFakeAttachProcess(0)
	t.Cleanup(func() { _ = base.Close() })
	proc := &procReadFailing{fakeAttachProcess: base, err: errors.New("kernel: ENXIO")}

	_, server := net.Pipe()
	t.Cleanup(func() { _ = server.Close() })

	done := make(chan struct{})
	var w bytes.Buffer
	go func() {
		defer close(done)
		pumpAttachProcessToConn(&w, server, proc)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not return on proc read error")
	}
	if !strings.Contains(w.String(), "ENXIO") {
		t.Errorf("console output %q missing proc read error", w.String())
	}
}

// TestReadAttachPayloadTruncated pins the partial-payload error
// branch: a valid length prefix promising N bytes followed by
// fewer than N bytes surfaces an io.ErrUnexpectedEOF wrap.
func TestReadAttachPayloadTruncated(t *testing.T) {
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 100)
	buf.Write(hdr[:])
	buf.Write([]byte("only-12-bytes")) // less than 100
	if _, err := readAttachPayload(&buf); err == nil {
		t.Fatal("expected error on truncated payload")
	}
}

// TestReadAttachFrameUnmarshalError pins the "valid length, bad
// proto bytes" error branch.
func TestReadAttachFrameUnmarshalError(t *testing.T) {
	var buf bytes.Buffer
	if err := writeAttachLenPrefixed(&buf, []byte{0xff, 0xff, 0xff, 0xff}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := ReadAttachFrame(&buf); err == nil {
		t.Fatal("expected unmarshal error")
	}
}

// TestWriteAttachLenPrefixedRejectsOversized pins the cap on
// outbound payloads (mirrors readAttachPayload's check).
func TestWriteAttachLenPrefixedRejectsOversized(t *testing.T) {
	var buf bytes.Buffer
	payload := make([]byte, attachMaxFrame+1)
	if err := writeAttachLenPrefixed(&buf, payload); err == nil {
		t.Fatal("expected error on oversized payload")
	}
}

// resizeFailingProc overrides Resize to return a canned error.
// Used to drive the "Resize errors are logged but non-fatal" path.
type resizeFailingProc struct {
	*fakeAttachProcess
	err error
}

func (p *resizeFailingProc) Resize(_, _ uint32) error { return p.err }

// readFrameOrFail wraps ReadAttachFrame with a t.Fatal so the
// happy-path tests stay short. Uses a deadline to surface a hung
// listener as a test failure rather than a hung suite.
func readFrameOrFail(t *testing.T, r net.Conn) *agentv1.AgentAttachFrame {
	t.Helper()
	_ = r.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = r.SetReadDeadline(time.Time{}) }()
	frame, err := ReadAttachFrame(r)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return frame
}

// waitFor polls cond until it returns true or the deadline
// elapses. Cheap alternative to a sync primitive when the only
// observable is a side-effect on a third-party struct.
func waitFor(t *testing.T, maxWait time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}
