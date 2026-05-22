package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// versionSubcommand is the smoke-test argv. Pulled into a
// const so goconst doesn't flag it across three call sites.
const versionSubcommand = "version"

// disableUsernsReexec installs a no-op userns stub on the test
// seam for the duration of the test, restoring the previous
// value on cleanup. Without this, main() would try to re-exec
// the test binary into its own user namespace — neither what
// the test wants nor what `go test` survives.
func disableUsernsReexec(t *testing.T) {
	t.Helper()
	prev := usernsReexec
	usernsReexec = func(io.Writer) error { return nil }
	t.Cleanup(func() { usernsReexec = prev })
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("nope") }

func TestRunSuccessPath(t *testing.T) {
	var out, errBuf bytes.Buffer
	if code := run([]string{versionSubcommand}, &out, &errBuf); code != 0 {
		t.Errorf("code = %d (stderr=%q), want 0", code, errBuf.String())
	}
}

func TestRunErrorPathReturns1AndPrefixesStderr(t *testing.T) {
	var out, errBuf bytes.Buffer
	code := run([]string{"definitely-not-real"}, &out, &errBuf)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errBuf.String(), "daft-sandboxd:") {
		t.Errorf("expected 'daft-sandboxd:' in stderr, got %q", errBuf.String())
	}
}

func TestRunReturns2WhenStderrWriteFails(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"definitely-not-real"}, &out, failingWriter{}); code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}

func TestMainPassesArgvAndCallsOsExit(t *testing.T) {
	disableUsernsReexec(t)
	oldArgs, oldExit := os.Args, osExit
	defer func() { os.Args, osExit = oldArgs, oldExit }()
	os.Args = []string{"daft-sandboxd", versionSubcommand}
	code := -1
	osExit = func(c int) { code = c }

	main()

	if code != 0 {
		t.Errorf("osExit called with %d, want 0", code)
	}
}

// TestMainExitsOnUsernsFailure pins the parent-side error path
// of the userns wiring: if Reexec returns an error (e.g. the
// host's /etc/subuid is malformed), main() reports it on
// stderr and exits 1. Caller relies on this to avoid silently
// running outside the userns and discovering the missing caps
// far downstream.
func TestMainExitsOnUsernsFailure(t *testing.T) {
	prev := usernsReexec
	usernsReexec = func(io.Writer) error { return errors.New("synthetic") }
	t.Cleanup(func() { usernsReexec = prev })

	oldArgs, oldExit := os.Args, osExit
	defer func() { os.Args, osExit = oldArgs, oldExit }()
	os.Args = []string{"daft-sandboxd", versionSubcommand}
	code := -1
	osExit = func(c int) { code = c }

	// Redirect stderr so the test doesn't pollute output.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	defer func() {
		os.Stderr = oldStderr
		_ = r.Close()
	}()

	main()
	_ = w.Close()

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	if code != 1 {
		t.Errorf("osExit code = %d, want 1", code)
	}
	if !strings.Contains(buf.String(), "userns: synthetic") {
		t.Errorf("stderr = %q, want it to mention 'userns: synthetic'", buf.String())
	}
}
