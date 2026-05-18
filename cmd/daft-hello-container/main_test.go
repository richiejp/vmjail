package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/agent"
)

func TestRunPrintsBannerAndReturnsZero(t *testing.T) {
	withNoWorkspace(t)
	var buf bytes.Buffer
	if code := run(&buf, false); code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(buf.String(), agent.ContainerBanner) {
		t.Errorf("expected banner %q in output, got %q", agent.ContainerBanner, buf.String())
	}
}

// failingWriter forces fmt.Fprintln's error branch.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("nope") }

func TestRunReturnsOneOnWriteError(t *testing.T) {
	withNoWorkspace(t)
	if code := run(failingWriter{}, false); code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
}

func TestMainCallsRunAndOsExit(t *testing.T) {
	withNoWorkspace(t)
	oldExit, oldOut, oldArgs := osExit, stdout, os.Args
	defer func() { osExit, stdout, os.Args = oldExit, oldOut, oldArgs }()
	os.Args = []string{"daft-hello-container"}
	var buf bytes.Buffer
	stdout = &buf
	code := -1
	osExit = func(c int) { code = c }

	main()

	if code != 0 {
		t.Errorf("osExit called with %d, want 0", code)
	}
	if !strings.Contains(buf.String(), agent.ContainerBanner) {
		t.Errorf("expected banner in stdout, got %q", buf.String())
	}
}

// TestMainKeepaliveFromArgsTriggersBlock pins the argv parse: a
// `--keepalive` token in os.Args flips main() into the long-running
// branch and calls block() exactly once before exiting.
func TestMainKeepaliveFromArgsTriggersBlock(t *testing.T) {
	withNoWorkspace(t)
	oldExit, oldOut, oldArgs, oldBlock := osExit, stdout, os.Args, block
	defer func() { osExit, stdout, os.Args, block = oldExit, oldOut, oldArgs, oldBlock }()
	os.Args = []string{"daft-hello-container", "--keepalive"}
	var buf bytes.Buffer
	stdout = &buf
	blockCalls := 0
	block = func() { blockCalls++ }
	code := -1
	osExit = func(c int) { code = c }

	main()

	if code != 0 {
		t.Errorf("osExit called with %d, want 0", code)
	}
	if blockCalls != 1 {
		t.Errorf("block called %d times, want 1", blockCalls)
	}
}

// TestRunKeepaliveCallsBlock pins the keepalive parameter wiring at
// the run() level (independent of os.Args).
func TestRunKeepaliveCallsBlock(t *testing.T) {
	withNoWorkspace(t)
	oldBlock := block
	defer func() { block = oldBlock }()
	called := false
	block = func() { called = true }
	var buf bytes.Buffer
	if code := run(&buf, true); code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !called {
		t.Error("block was not called for keepalive=true")
	}
}

// TestRunWorkspaceErrorSkipsBlock pins that a workspace failure
// returns immediately without entering the keepalive hold — the
// caller (the OCI runtime) needs the non-zero exit promptly.
func TestRunWorkspaceErrorSkipsBlock(t *testing.T) {
	oldRead, oldBlock := fsRead, block
	defer func() { fsRead, block = oldRead, oldBlock }()
	fsRead = func(string) ([]byte, error) { return nil, errors.New("synthetic") }
	called := false
	block = func() { called = true }
	var buf bytes.Buffer
	if code := run(&buf, true); code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if called {
		t.Error("block was called even though workspace round-trip failed")
	}
}

// TestBlockUntilTerminationSignalReturnsOnSignal pins the
// production wait: blockUntilTerminationSignal subscribes to
// SIGINT/SIGTERM, blocks, and returns once one arrives. SIGINT is
// the only safe signal of the two to deliver from a test —
// signal.Notify intercepts it before the default handler (process
// termination) can fire, so synchronous Kill-after-call is race-
// free.
func TestBlockUntilTerminationSignalReturnsOnSignal(t *testing.T) {
	done := make(chan struct{})
	ready := make(chan struct{})
	go func() {
		defer close(done)
		close(ready)
		blockUntilTerminationSignal()
	}()
	<-ready
	// The goroutine has been scheduled but signal.Notify may not
	// yet have installed the handler; a brief sleep here is the
	// pragmatic guard. If it ever flakes, push the readiness gate
	// into blockUntilTerminationSignal itself.
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("Kill SIGINT: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("blockUntilTerminationSignal did not return on SIGINT")
	}
}

func TestRunWorkspaceRoundTrip(t *testing.T) {
	wrote := withWorkspace(t, map[string][]byte{
		agent.WorkspaceInPath: []byte("hello-from-host"),
	})
	var buf bytes.Buffer
	if code := run(&buf, false); code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	got, ok := wrote[agent.WorkspaceOutPath]
	if !ok {
		t.Fatalf("expected write to %s; got %v", agent.WorkspaceOutPath, wrote)
	}
	want := "hello-from-host" + agent.WorkspaceSuffix
	if string(got) != want {
		t.Errorf("out content = %q, want %q", got, want)
	}
	if !strings.Contains(buf.String(), agent.ContainerBanner) {
		t.Errorf("banner missing even with workspace present")
	}
}

func TestRunWorkspaceReadErrorNonNotExist(t *testing.T) {
	oldRead := fsRead
	defer func() { fsRead = oldRead }()
	fsRead = func(string) ([]byte, error) { return nil, errors.New("synthetic read fail") }
	var buf bytes.Buffer
	if code := run(&buf, false); code != 2 {
		t.Errorf("code = %d, want 2 on non-ErrNotExist read failure", code)
	}
	if !strings.Contains(buf.String(), "workspace round-trip error") {
		t.Errorf("expected diagnostic in stdout, got %q", buf.String())
	}
}

func TestRunWorkspaceWriteError(t *testing.T) {
	oldRead, oldWrite := fsRead, fsWrite
	defer func() { fsRead, fsWrite = oldRead, oldWrite }()
	fsRead = func(string) ([]byte, error) { return []byte("data"), nil }
	fsWrite = func(string, []byte, fs.FileMode) error { return errors.New("write fail") }
	var buf bytes.Buffer
	if code := run(&buf, false); code != 2 {
		t.Errorf("code = %d, want 2 on write failure", code)
	}
}

// TestWriteErrfDropsOnClosedStdout exercises the rare branch where
// the diagnostic itself fails to write. It's a 1-line method but the
// failure branch matters because hello-container's stdout is the
// VM's console — losing it would mean the container can't report
// errors home.
func TestWriteErrfDropsOnClosedStdout(t *testing.T) {
	// failingWriter Write always errors; writeErrf must not panic
	// or propagate the error.
	writeErrf(failingWriter{}, "format %s", "arg")
}

// withNoWorkspace replaces fsRead with a stub that returns ErrNotExist
// so the workspace branch is the safe-skip path.
func withNoWorkspace(t *testing.T) {
	t.Helper()
	oldRead := fsRead
	t.Cleanup(func() { fsRead = oldRead })
	fsRead = func(string) ([]byte, error) { return nil, fs.ErrNotExist }
}

// withWorkspace plants in-memory file contents and returns a map
// that records every fsWrite call. Tests use this to assert exactly
// what the binary would have written to /workspace. The map is
// mutated by the stubs in place, so the caller sees writes as they
// happen.
func withWorkspace(t *testing.T, files map[string][]byte) map[string][]byte {
	t.Helper()
	oldRead, oldWrite := fsRead, fsWrite
	t.Cleanup(func() { fsRead, fsWrite = oldRead, oldWrite })
	captured := map[string][]byte{}
	fsRead = func(path string) ([]byte, error) {
		if b, ok := files[path]; ok {
			return b, nil
		}
		return nil, fs.ErrNotExist
	}
	fsWrite = func(path string, data []byte, _ fs.FileMode) error {
		captured[path] = append([]byte(nil), data...)
		return nil
	}
	return captured
}
