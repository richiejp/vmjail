package main

import (
	"bytes"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/agent"
)

func TestRunPrintsBannerAndReturnsZero(t *testing.T) {
	withNoWorkspace(t)
	var buf bytes.Buffer
	if code := run(&buf); code != 0 {
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
	if code := run(failingWriter{}); code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
}

func TestMainCallsRunAndOsExit(t *testing.T) {
	withNoWorkspace(t)
	oldExit, oldOut := osExit, stdout
	defer func() { osExit, stdout = oldExit, oldOut }()
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

func TestRunWorkspaceRoundTrip(t *testing.T) {
	wrote := withWorkspace(t, map[string][]byte{
		agent.WorkspaceInPath: []byte("hello-from-host"),
	})
	var buf bytes.Buffer
	if code := run(&buf); code != 0 {
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
	if code := run(&buf); code != 2 {
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
	if code := run(&buf); code != 2 {
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
