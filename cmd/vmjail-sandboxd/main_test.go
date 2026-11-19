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
const binName = "vmjail-sandboxd"

// runMainCapturingStderr runs main() with argv [binName, args...],
// os.Exit and os.Stderr redirected, and returns the captured exit code
// and stderr. Restores the globals via Cleanup. Shared by the
// startup-precondition failure tests so each only has to install its
// own failing seam and assert the message.
func runMainCapturingStderr(t *testing.T, args ...string) (code int, stderr string) {
	t.Helper()
	oldArgs, oldExit := os.Args, osExit
	t.Cleanup(func() { os.Args, osExit = oldArgs, oldExit })
	os.Args = append([]string{binName}, args...)
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

// disablePrepareCgroupSubtree installs a no-op subtree-prepare stub so
// main() doesn't move the test process into a leaf cgroup / write the real
// cgroup tree on its success path.
func disablePrepareCgroupSubtree(t *testing.T) {
	t.Helper()
	prev := prepareCgroupSubtree
	prepareCgroupSubtree = func() (string, error) { return "", nil }
	t.Cleanup(func() { prepareCgroupSubtree = prev })
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
	if !strings.Contains(errBuf.String(), "vmjail-sandboxd:") {
		t.Errorf("expected 'vmjail-sandboxd:' in stderr, got %q", errBuf.String())
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
	disablePrepareCgroupSubtree(t)
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

func TestHostSetupCommandsSkipSandboxPreconditions(t *testing.T) {
	oldArgs, oldExit := os.Args, osExit
	oldCgroup, oldPrepare, oldUserns := cgroupReexec, prepareCgroupSubtree, usernsReexec
	t.Cleanup(func() {
		os.Args, osExit = oldArgs, oldExit
		cgroupReexec, prepareCgroupSubtree, usernsReexec = oldCgroup, oldPrepare, oldUserns
	})
	called := false
	cgroupReexec = func(io.Writer) error { called = true; return nil }
	prepareCgroupSubtree = func() (string, error) { called = true; return "", nil }
	usernsReexec = func(io.Writer) error { called = true; return nil }
	os.Args = []string{binName, versionSubcommand}
	code := -1
	osExit = func(c int) { code = c }

	main()

	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if called {
		t.Fatal("version command ran sandbox runtime preconditions")
	}
}

func TestIsHostSetupCommand(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"auth", "show"}, {"version"}, {"--help"}, {"help"}} {
		if !isHostSetupCommand(args) {
			t.Errorf("isHostSetupCommand(%q) = false", args)
		}
	}
	for _, args := range [][]string{{"serve"}, {"hello"}, {"debug", "run"}, {"unknown"}} {
		if isHostSetupCommand(args) {
			t.Errorf("isHostSetupCommand(%q) = true", args)
		}
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
	disablePrepareCgroupSubtree(t)
	prev := usernsReexec
	usernsReexec = func(io.Writer) error { return errors.New("synthetic") }
	t.Cleanup(func() { usernsReexec = prev })

	code, stderr := runMainCapturingStderr(t, "debug", "run")
	if code != 1 {
		t.Errorf("osExit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "userns: synthetic") {
		t.Errorf("stderr = %q, want it to mention 'userns: synthetic'", stderr)
	}
}

// TestMainFailsClosedOnSubtreeFailure pins the fail-closed posture of the
// controller-enable step (which backs the per-VM resource limits): a failure
// here (e.g. a pre-delegated shared scope that can't enable subtree_control)
// aborts the boot rather than silently running without a blast-radius cap.
// It runs after the delegation wrap, before the userns re-exec.
func TestMainFailsClosedOnSubtreeFailure(t *testing.T) {
	disableUsernsReexec(t)
	disableCgroupReexec(t)
	prev := prepareCgroupSubtree
	prepareCgroupSubtree = func() (string, error) { return "", errors.New("synthetic-subtree") }
	t.Cleanup(func() { prepareCgroupSubtree = prev })

	code, stderr := runMainCapturingStderr(t, "debug", "run")
	if code != 1 {
		t.Errorf("osExit code = %d, want 1 (fail-closed)", code)
	}
	if !strings.Contains(stderr, "cannot be enforced: synthetic-subtree") {
		t.Errorf("stderr = %q, want it to report limits cannot be enforced", stderr)
	}
	if !strings.Contains(stderr, envDisableCgroupLimits) {
		t.Errorf("stderr = %q, want it to name the opt-out env var", stderr)
	}
}

// TestMainContinuesWhenCgroupLimitsDisabled pins the conscious opt-out: with
// VMJAIL_DISABLE_CGROUP_LIMITS=1 the same controller-enable failure degrades to
// a warning and the host boots without enforced limits, instead of aborting.
func TestMainContinuesWhenCgroupLimitsDisabled(t *testing.T) {
	disableUsernsReexec(t)
	disableCgroupReexec(t)
	t.Setenv(envDisableCgroupLimits, "1")
	prev := prepareCgroupSubtree
	prepareCgroupSubtree = func() (string, error) { return "", errors.New("synthetic-subtree") }
	t.Cleanup(func() { prepareCgroupSubtree = prev })

	code, stderr := runMainCapturingStderr(t, "debug", "run", "--help")
	if code != 0 {
		t.Errorf("osExit code = %d, want 0 (opt-out warn-and-continue)", code)
	}
	if !strings.Contains(stderr, "limits disabled via "+envDisableCgroupLimits) {
		t.Errorf("stderr = %q, want it to report the opt-out degrade", stderr)
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

	code, stderr := runMainCapturingStderr(t, "debug", "run")
	if code != 1 {
		t.Errorf("osExit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "cgroup: synthetic-cgroup") {
		t.Errorf("stderr = %q, want it to mention 'cgroup: synthetic-cgroup'", stderr)
	}
}
