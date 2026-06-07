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

// binName is argv[0] for the main() tests, in a const for goconst.
const binName = "daft-sandboxd"

// runMainCapturingStderr runs main() with argv [binName, version],
// os.Exit and os.Stderr redirected, and returns the captured exit code
// and stderr. Restores the globals via Cleanup. Shared by the
// startup-precondition failure tests so each only has to install its
// own failing seam and assert the message.
func runMainCapturingStderr(t *testing.T) (code int, stderr string) {
	t.Helper()
	oldArgs, oldExit := os.Args, osExit
	t.Cleanup(func() { os.Args, osExit = oldArgs, oldExit })
	os.Args = []string{binName, versionSubcommand}
	code = -1
	osExit = func(c int) { code = c }

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = oldStderr; _ = r.Close() })

	main()
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return code, buf.String()
}

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

// disableCgroupReexec installs a no-op cgroup-delegation stub on the
// test seam so main() doesn't probe the real cgroup and — on a
// non-delegated host — re-exec the test binary under systemd-run.
func disableCgroupReexec(t *testing.T) {
	t.Helper()
	prev := cgroupReexec
	cgroupReexec = func(io.Writer) error { return nil }
	t.Cleanup(func() { cgroupReexec = prev })
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
	disableCgroupReexec(t)
	oldArgs, oldExit := os.Args, osExit
	defer func() { os.Args, osExit = oldArgs, oldExit }()
	os.Args = []string{binName, versionSubcommand}
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
	disableCgroupReexec(t)
	prev := usernsReexec
	usernsReexec = func(io.Writer) error { return errors.New("synthetic") }
	t.Cleanup(func() { usernsReexec = prev })

	code, stderr := runMainCapturingStderr(t)
	if code != 1 {
		t.Errorf("osExit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "userns: synthetic") {
		t.Errorf("stderr = %q, want it to mention 'userns: synthetic'", stderr)
	}
}

// TestMainExitsOnCgroupFailure pins the strict-precondition path: if
// the cgroup-delegation wrap can't guarantee a delegated subtree it
// returns an error, and main() reports it on stderr and exits 1 — it
// must never proceed to boot VMs it can't account for. The cgroup
// check runs before the userns re-exec, so this fires first.
func TestMainExitsOnCgroupFailure(t *testing.T) {
	disableUsernsReexec(t)
	prev := cgroupReexec
	cgroupReexec = func(io.Writer) error { return errors.New("synthetic-cgroup") }
	t.Cleanup(func() { cgroupReexec = prev })

	code, stderr := runMainCapturingStderr(t)
	if code != 1 {
		t.Errorf("osExit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "cgroup: synthetic-cgroup") {
		t.Errorf("stderr = %q, want it to mention 'cgroup: synthetic-cgroup'", stderr)
	}
}
