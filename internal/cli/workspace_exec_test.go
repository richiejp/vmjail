package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
)

// testOrchURL is the placeholder URL every exec test uses; the
// dial path is stubbed so the value is never resolved.
const testOrchURL = "https://x"

// fakeExecStream is an in-process bidi stream stub: tests push
// frames into recvCh and inspect anything the CLI sends via sent.
type fakeExecStream struct {
	recvCh   chan *workspacev1.AttachFrame
	recvErr  chan error
	closedRq bool
	closedRs bool

	mu   sync.Mutex
	sent []*workspacev1.AttachFrame
}

func newFakeExecStream() *fakeExecStream {
	return &fakeExecStream{
		recvCh:  make(chan *workspacev1.AttachFrame, 16),
		recvErr: make(chan error, 1),
	}
}

func (s *fakeExecStream) Send(f *workspacev1.AttachFrame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, f)
	return nil
}

func (s *fakeExecStream) Receive() (*workspacev1.AttachFrame, error) {
	select {
	case f := <-s.recvCh:
		return f, nil
	case err := <-s.recvErr:
		return nil, err
	}
}

func (s *fakeExecStream) CloseRequest() error  { s.closedRq = true; return nil }
func (s *fakeExecStream) CloseResponse() error { s.closedRs = true; return nil }

func (s *fakeExecStream) sentFrames() []*workspacev1.AttachFrame {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*workspacev1.AttachFrame, len(s.sent))
	copy(out, s.sent)
	return out
}

// fakeExecTerminal is a deterministic execTerminal: tests control
// the reported window size, raw-mode result, and the resize
// channel.
type fakeExecTerminal struct {
	isTTY        bool
	cols, rows   uint32
	winsizeErr   error
	makeRawErr   error
	restoredOnce bool

	resize chan struct{}
}

func newFakeExecTerminal(isTTY bool, cols, rows uint32) *fakeExecTerminal {
	return &fakeExecTerminal{
		isTTY:  isTTY,
		cols:   cols,
		rows:   rows,
		resize: make(chan struct{}, 4),
	}
}

func (t *fakeExecTerminal) IsTTY() bool { return t.isTTY }

func (t *fakeExecTerminal) Winsize() (cols, rows uint32, err error) {
	if t.winsizeErr != nil {
		return 0, 0, t.winsizeErr
	}
	return t.cols, t.rows, nil
}

func (t *fakeExecTerminal) MakeRaw() (func() error, error) {
	if t.makeRawErr != nil {
		return nil, t.makeRawErr
	}
	return func() error { t.restoredOnce = true; return nil }, nil
}

func (t *fakeExecTerminal) Resize(_ context.Context) <-chan struct{} {
	if !t.isTTY {
		return nil
	}
	return t.resize
}

func (t *fakeExecTerminal) pushResize(cols, rows uint32) {
	t.cols, t.rows = cols, rows
	t.resize <- struct{}{}
}

// TestRunWorkspaceExecDefaultsWorkspaceFromConfig pins that an
// omitted workspace is filled from ./.daft/config.json: with the name
// set there, resolution succeeds and the command proceeds to dial
// (here a stubbed dial error), rather than rejecting a missing arg.
func TestRunWorkspaceExecDefaultsWorkspaceFromConfig(t *testing.T) {
	clearOrchestratorEnv(t)
	chdirClean(t)
	dlPath, err := dirLocalConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := saveFileConfig(dlPath, fileConfig{Name: "cfgws"}); err != nil {
		t.Fatal(err)
	}
	wantDial := errors.New("dial sentinel")
	deps := execDeps{
		dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
			return nil, wantDial
		},
	}
	err = runWorkspaceExecWith(context.Background(), io.Discard, io.Discard,
		workspaceExecOpts{configDir: t.TempDir()}, deps)
	if !errors.Is(err, wantDial) {
		t.Errorf("err = %v, want dial sentinel (proves workspace resolved from config)", err)
	}
}

// TestRunWorkspaceExecSendsHelloAndPumpsData drives a full session
// against the in-process fake stream. Verifies:
//   - Hello frame carries workspace, command, tty + initial winsize
//   - stdin bytes are wrapped into Data frames
//   - server-side Data frames are written to stdout
//   - MakeRaw is called and restored on exit
func TestRunWorkspaceExecSendsHelloAndPumpsData(t *testing.T) {
	stream := newFakeExecStream()
	term := newFakeExecTerminal(true, 100, 30)

	var stdoutBuf syncBuf
	stdin := &slowReader{
		data:  []byte("ls\n"),
		block: make(chan struct{}),
	}
	deps := execDeps{
		dialOrchestrator: stubDialOrch,
		openStream: func(context.Context, workspacev1connect.WorkspaceCatalogServiceClient) execClientStream {
			return stream
		},
		stdin:    stdin,
		stdout:   &stdoutBuf,
		terminal: term,
	}
	opts := workspaceExecOpts{
		orchestrator: testOrchURL,
		workspace:    "ws",
		command:      []string{"/bin/sh", "-l"},
		tty:          true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- runWorkspaceExecWith(ctx, io.Discard, io.Discard, opts, deps) }()

	// Wait for Hello + the first Data frame from stdin.
	waitFor(t, 2*time.Second, func() bool { return len(stream.sentFrames()) >= 2 })

	frames := stream.sentFrames()
	hello := frames[0].GetHello()
	if hello == nil {
		t.Fatalf("first frame = %T, want Hello", frames[0].GetBody())
	}
	if hello.GetTty() != true || hello.GetCols() != 100 || hello.GetRows() != 30 {
		t.Errorf("Hello mismatch: %+v", hello)
	}
	if h := hello.GetWorkspace().GetName(); h != "ws" {
		t.Errorf("workspace = %q, want ws", h)
	}
	if got := hello.GetCommand(); len(got) != 2 || got[1] != "-l" {
		t.Errorf("command = %v, want [/bin/sh -l]", got)
	}

	dataFrame := frames[1].GetData()
	if !bytes.Equal(dataFrame, []byte("ls\n")) {
		t.Errorf("first data frame = %q, want ls\\n", dataFrame)
	}

	// Push a server-side Data frame; stdout receives it.
	stream.recvCh <- &workspacev1.AttachFrame{
		Body: &workspacev1.AttachFrame_Data{Data: []byte("hello from agent")},
	}
	waitFor(t, time.Second, func() bool {
		return strings.Contains(stdoutBuf.String(), "hello from agent")
	})

	// Server closes the stream; the run should return cleanly.
	stream.recvErr <- io.EOF
	close(stdin.block) // unblock the stdin reader if it's still parked
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runWorkspaceExecWith: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runWorkspaceExecWith did not return")
	}
	if !term.restoredOnce {
		t.Error("terminal restore func not invoked")
	}
}

// TestRunWorkspaceExecForwardsResize: a SIGWINCH-style event on
// the fake terminal becomes a Resize frame with the updated dims.
func TestRunWorkspaceExecForwardsResize(t *testing.T) {
	stream := newFakeExecStream()
	term := newFakeExecTerminal(true, 80, 24)

	stdin := &slowReader{block: make(chan struct{})}
	deps := execDeps{
		dialOrchestrator: stubDialOrch,
		openStream: func(context.Context, workspacev1connect.WorkspaceCatalogServiceClient) execClientStream {
			return stream
		},
		stdin:    stdin,
		stdout:   io.Discard,
		terminal: term,
	}
	opts := workspaceExecOpts{orchestrator: testOrchURL, workspace: "ws", tty: true}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- runWorkspaceExecWith(ctx, io.Discard, io.Discard, opts, deps) }()

	waitFor(t, 2*time.Second, func() bool { return len(stream.sentFrames()) >= 1 })
	term.pushResize(132, 50)
	waitFor(t, 2*time.Second, func() bool {
		for _, f := range stream.sentFrames() {
			if r := f.GetResize(); r != nil && r.GetCols() == 132 && r.GetRows() == 50 {
				return true
			}
		}
		return false
	})

	stream.recvErr <- io.EOF
	close(stdin.block)
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("session did not exit")
	}
}

// TestRunWorkspaceExecNoTTYSkipsWinsizeAndRaw verifies the
// piped-mode path: tty=false yields a Hello with no dims, no
// MakeRaw call, no Resize channel.
func TestRunWorkspaceExecNoTTYSkipsWinsizeAndRaw(t *testing.T) {
	stream := newFakeExecStream()
	term := newFakeExecTerminal(true, 80, 24)

	deps := execDeps{
		dialOrchestrator: stubDialOrch,
		openStream: func(context.Context, workspacev1connect.WorkspaceCatalogServiceClient) execClientStream {
			return stream
		},
		stdin:    bytes.NewReader(nil),
		stdout:   io.Discard,
		terminal: term,
	}
	opts := workspaceExecOpts{
		orchestrator: testOrchURL,
		workspace:    "ws",
		command:      []string{"echo", "hi"},
		tty:          false,
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- runWorkspaceExecWith(ctx, io.Discard, io.Discard, opts, deps) }()

	waitFor(t, time.Second, func() bool { return len(stream.sentFrames()) >= 1 })
	hello := stream.sentFrames()[0].GetHello()
	if hello == nil || hello.GetTty() {
		t.Fatalf("expected Hello with tty=false, got %+v", hello)
	}
	if hello.GetCols() != 0 || hello.GetRows() != 0 {
		t.Errorf("piped mode should not carry winsize: %+v", hello)
	}
	if term.restoredOnce {
		t.Error("MakeRaw restore must not fire when tty=false")
	}

	stream.recvErr <- io.EOF
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("session did not exit")
	}
}

// TestRunWorkspaceExecNilStreamFromFactory pins the defensive
// branch: if the openStream factory hands back a nil stream
// (configuration bug), the run errors clearly rather than
// nil-dereferencing.
func TestRunWorkspaceExecNilStreamFromFactory(t *testing.T) {
	deps := execDeps{
		dialOrchestrator: stubDialOrch,
		openStream: func(context.Context, workspacev1connect.WorkspaceCatalogServiceClient) execClientStream {
			return nil
		},
	}
	err := runWorkspaceExecWith(context.Background(),
		io.Discard, io.Discard,
		workspaceExecOpts{orchestrator: testOrchURL, workspace: "ws"},
		deps)
	if err == nil || !strings.Contains(err.Error(), "nil stream") {
		t.Errorf("err = %v, want nil-stream error", err)
	}
}

// helloFailingStream errors on Send so the test drives the
// Hello-write error branch. Receive blocks forever — pumps never
// reach it because Send fails first.
type helloFailingStream struct {
	sendErr error
}

func (s *helloFailingStream) Send(*workspacev1.AttachFrame) error {
	return s.sendErr
}

func (s *helloFailingStream) Receive() (*workspacev1.AttachFrame, error) {
	select {}
}
func (s *helloFailingStream) CloseRequest() error  { return nil }
func (s *helloFailingStream) CloseResponse() error { return nil }

// TestRunWorkspaceExecHelloSendError surfaces a Send failure
// during the initial Hello as a wrapped error.
func TestRunWorkspaceExecHelloSendError(t *testing.T) {
	want := errors.New("synthetic send fail")
	deps := execDeps{
		dialOrchestrator: stubDialOrch,
		openStream: func(context.Context, workspacev1connect.WorkspaceCatalogServiceClient) execClientStream {
			return &helloFailingStream{sendErr: want}
		},
		stdin: bytes.NewReader(nil), stdout: io.Discard,
		terminal: newFakeExecTerminal(false, 0, 0),
	}
	err := runWorkspaceExecWith(context.Background(),
		io.Discard, io.Discard,
		workspaceExecOpts{orchestrator: testOrchURL, workspace: "ws"},
		deps)
	if err == nil || !strings.Contains(err.Error(), "send Hello") {
		t.Errorf("err = %v, want send-Hello wrap", err)
	}
	if !errors.Is(err, want) {
		t.Errorf("error does not wrap sentinel: %v", err)
	}
}

// TestPumpExecStreamToStdoutSkipsEmptyFrames covers the
// "len(data) == 0 → continue" branch — orchestrator-side Hello-
// less first frames and stray Resize echoes never reach stdout.
// Frames must be enqueued before EOF (the fakeExecStream's
// select randomises across ready channels).
func TestPumpExecStreamToStdoutSkipsEmptyFrames(t *testing.T) {
	stream := newFakeExecStream()
	stream.recvCh <- &workspacev1.AttachFrame{
		Body: &workspacev1.AttachFrame_Data{Data: nil},
	}
	stream.recvCh <- &workspacev1.AttachFrame{
		Body: &workspacev1.AttachFrame_Data{Data: []byte("payload")},
	}

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = pumpExecStreamToStdout(stream, &buf)
	}()
	// Wait for both frames to flow through, then trip the EOF.
	waitFor(t, time.Second, func() bool { return buf.Len() >= len("payload") })
	stream.recvErr <- io.EOF
	<-done
	if got := buf.String(); got != "payload" {
		t.Errorf("stdout = %q, want %q", got, "payload")
	}
}

// TestPumpExecStdinToStreamSurfacesSendError covers the Send-
// failure return path: a Send error mid-pump aborts the pump and
// bubbles up.
func TestPumpExecStdinToStreamSurfacesSendError(t *testing.T) {
	want := errors.New("synthetic send fail")
	stream := &helloFailingStream{sendErr: want}
	err := pumpExecStdinToStream(bytes.NewReader([]byte("hello")), stream)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestPumpExecResizeExitsOnResizeChannelClosed covers the
// !ok branch of the resize channel select: a terminal whose
// Resize channel is closed (rather than producing a struct{})
// should exit the pump cleanly.
func TestPumpExecResizeExitsOnResizeChannelClosed(t *testing.T) {
	stream := newFakeExecStream()
	term := newFakeExecTerminal(true, 80, 24)
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		pumpExecResize(context.Background(), term, stream, done)
	}()
	close(term.resize)
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("pumpExecResize did not exit on resize channel close")
	}
}

// TestPumpExecResizeExitsOnCtxDone covers the ctx.Done() branch
// of pumpExecResize: when the surrounding context fires before
// `done` does, the pump returns without sending more Resize
// frames. Separately from pumpExecResize's other tests because
// they all hit the `done` channel path.
func TestPumpExecResizeExitsOnCtxDone(t *testing.T) {
	stream := newFakeExecStream()
	term := newFakeExecTerminal(true, 80, 24)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		pumpExecResize(ctx, term, stream, done)
	}()
	cancel()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("pumpExecResize did not exit on ctx cancellation")
	}
}

// TestPumpExecSessionReturnsUnexpectedStreamError pins the
// stream-pump-error branch in pumpExecSession: when the server's
// Receive returns something other than an expected pump-exit
// error (canceled / EOF), pumpExecSession surfaces it instead
// of treating it as a normal end.
func TestPumpExecSessionReturnsUnexpectedStreamError(t *testing.T) {
	stream := newFakeExecStream()
	term := newFakeExecTerminal(false, 0, 0)
	want := errors.New("synthetic server fail")

	errCh := make(chan error, 1)
	go func() {
		errCh <- pumpExecSession(context.Background(),
			&slowReader{block: make(chan struct{})},
			io.Discard, stream, false, term)
	}()
	stream.recvErr <- want
	select {
	case err := <-errCh:
		if !errors.Is(err, want) {
			t.Fatalf("err = %v, want wrap of %v", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pumpExecSession did not return on unexpected stream error")
	}
}

// TestNewWorkspaceExecCmdRoundtrip drives the cobra surface
// through runWorkspaceExec → defaultExecDeps's dialOrchestrator,
// which fails dialing the unreachable URL. Verifies the cobra
// wiring (RunE callback, flag parsing, signal context) is
// reachable and propagates errors as expected.
func TestNewWorkspaceExecCmdRoundtrip(t *testing.T) {
	cmd := newWorkspaceExecCmd()
	cmd.SetArgs([]string{
		"--orchestrator=https://127.0.0.1:1", // refused
		"ws",
	})
	cmd.SetContext(context.Background())
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error from unreachable orchestrator")
	}
}

// TestPumpExecResizeNilChanReturns: a terminal that yields a nil
// resize channel (the IsTTY=false case in production) must exit
// pumpExecResize immediately without doing anything.
func TestPumpExecResizeNilChanReturns(t *testing.T) {
	stream := newFakeExecStream()
	term := newFakeExecTerminal(false, 0, 0) // !isTTY → Resize returns nil
	done := make(chan struct{})
	pumpExecResize(context.Background(), term, stream, done)
	if frames := stream.sentFrames(); len(frames) != 0 {
		t.Errorf("expected no frames sent for nil resize chan, got %d", len(frames))
	}
}

// TestPumpExecResizeWinsizeErrorExits: when the terminal yields a
// resize event but Winsize errors, the pump exits cleanly (no
// Resize frame, no goroutine pin).
func TestPumpExecResizeWinsizeErrorExits(t *testing.T) {
	stream := newFakeExecStream()
	term := newFakeExecTerminal(true, 80, 24)
	done := make(chan struct{})
	doneAfter := make(chan struct{})
	go func() {
		defer close(doneAfter)
		pumpExecResize(context.Background(), term, stream, done)
	}()
	// Arm winsizeErr first; the channel send happens-before the
	// pump's Winsize call, so the next event hits the error branch
	// and the goroutine returns.
	term.winsizeErr = errors.New("ioctl error")
	term.resize <- struct{}{}
	select {
	case <-doneAfter:
	case <-time.After(time.Second):
		t.Fatal("pumpExecResize did not exit on Winsize error")
	}
}

// TestDefaultExecDepsWiresProductionPlumbing pins that
// defaultExecDeps returns non-nil fields for every seam. Catches
// regressions where a refactor drops one of the production hooks.
func TestDefaultExecDepsWiresProductionPlumbing(t *testing.T) {
	d := defaultExecDeps()
	if d.dialOrchestrator == nil {
		t.Error("dialOrchestrator is nil")
	}
	if d.openStream == nil {
		t.Error("openStream is nil")
	}
	if d.stdin == nil || d.stdout == nil {
		t.Error("stdio is nil")
	}
	if d.terminal == nil {
		t.Error("terminal is nil")
	}
}

// TestRunWorkspaceExecDialError surfaces the dial error verbatim.
func TestRunWorkspaceExecDialError(t *testing.T) {
	want := errors.New("synthetic dial fail")
	deps := execDeps{
		dialOrchestrator: func(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
			return nil, want
		},
	}
	err := runWorkspaceExecWith(context.Background(),
		io.Discard, io.Discard,
		workspaceExecOpts{orchestrator: testOrchURL, workspace: "ws"},
		deps)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}

// TestRunWorkspaceExecHelloWinsizeError surfaces a TIOCGWINSZ
// failure as a wrapped error before any Send happens.
func TestRunWorkspaceExecHelloWinsizeError(t *testing.T) {
	stream := newFakeExecStream()
	term := newFakeExecTerminal(true, 0, 0)
	term.winsizeErr = errors.New("ioctl TIOCGWINSZ: not a tty")
	deps := execDeps{
		dialOrchestrator: stubDialOrch,
		openStream: func(context.Context, workspacev1connect.WorkspaceCatalogServiceClient) execClientStream {
			return stream
		},
		stdin: bytes.NewReader(nil), stdout: io.Discard, terminal: term,
	}
	err := runWorkspaceExecWith(context.Background(),
		io.Discard, io.Discard,
		workspaceExecOpts{orchestrator: testOrchURL, workspace: "ws", tty: true},
		deps)
	if err == nil || !strings.Contains(err.Error(), "read winsize") {
		t.Errorf("err = %v, want winsize wrap", err)
	}
}

// TestRunWorkspaceExecRawModeErrorContinuesInCookedMode pins the
// graceful-degrade branch: MakeRaw failures log a warning to
// stderr but the session continues.
func TestRunWorkspaceExecRawModeErrorContinuesInCookedMode(t *testing.T) {
	stream := newFakeExecStream()
	term := newFakeExecTerminal(true, 80, 24)
	term.makeRawErr = errors.New("ioctl set termios: bad fd")
	var stderr syncBuf
	deps := execDeps{
		dialOrchestrator: stubDialOrch,
		openStream: func(context.Context, workspacev1connect.WorkspaceCatalogServiceClient) execClientStream {
			return stream
		},
		stdin: bytes.NewReader(nil), stdout: io.Discard, terminal: term,
	}
	opts := workspaceExecOpts{orchestrator: testOrchURL, workspace: "ws", tty: true}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	errCh := make(chan error, 1)
	go func() { errCh <- runWorkspaceExecWith(ctx, io.Discard, &stderr, opts, deps) }()

	waitFor(t, time.Second, func() bool {
		return strings.Contains(stderr.String(), "raw mode failed")
	})

	stream.recvErr <- io.EOF
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("expected clean exit despite raw-mode failure, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session did not exit")
	}
}

// stubDialOrch returns a nil client because every test that uses
// it also overrides openStream — which means the client value
// passed to openStream is never dereferenced. A real client
// would require the full connect/mTLS stack stand-up for no test
// benefit.
//
//nolint:nilnil // deliberate: nil-client + nil-error is the contract every test honours.
func stubDialOrch(string, string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
	return nil, nil
}

// slowReader is an io.Reader that yields its `data` once then
// blocks on `block` until closed. Lets a test fire one Data
// frame and then sit on stdin until session exit (mirrors a
// real interactive terminal).
type slowReader struct {
	mu    sync.Mutex
	data  []byte
	block chan struct{}
}

func (r *slowReader) Read(b []byte) (int, error) {
	r.mu.Lock()
	if len(r.data) > 0 {
		n := copy(b, r.data)
		r.data = r.data[n:]
		r.mu.Unlock()
		return n, nil
	}
	r.mu.Unlock()
	<-r.block
	return 0, io.EOF
}

// waitFor and syncBuf are shared with other CLI tests; declared
// here as no-op forwards to avoid duplicate symbols if reordered.
// (Defined elsewhere in package cli.)
