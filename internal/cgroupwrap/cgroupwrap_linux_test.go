//go:build linux

package cgroupwrap

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// withSeams installs test doubles for the package re-exec seams and
// restores the originals on cleanup. Tests must not run in parallel
// because the seams are package globals.
func withSeams(t *testing.T, deleg func() bool, look func(string) (string, error),
	exe func() (string, error), uid func() int, exec func(string, []string, []string) error,
) {
	t.Helper()
	od, ol, oe, ou, ox := delegationOK, lookPath, osExecutable, getuid, execFn
	t.Cleanup(func() {
		delegationOK, lookPath, osExecutable, getuid, execFn = od, ol, oe, ou, ox
	})
	delegationOK, lookPath, osExecutable, getuid, execFn = deleg, look, exe, uid, exec
}

func okLook(string) (string, error) { return "/usr/bin/systemd-run", nil }
func okExe() (string, error)        { return "/path/to/daft-sandboxd", nil }
func nonRoot() int                  { return 1000 }
func neverExec(string, []string, []string) error {
	panic("execFn should not be called")
}

// TestEnsureDelegated_AlreadyDelegated: the fast path returns nil and
// never re-execs when the inherited cgroup is already delegated.
func TestEnsureDelegated_AlreadyDelegated(t *testing.T) {
	withSeams(t, func() bool { return true }, okLook, okExe, nonRoot, neverExec)
	if err := EnsureDelegated(io.Discard); err != nil {
		t.Fatalf("EnsureDelegated when already delegated: %v", err)
	}
}

// TestEnsureDelegated_WrapsUnprivileged: not delegated + non-root +
// no marker → execve systemd-run --user --scope with Delegate=yes, the
// marker in the child env, and our own argv forwarded after the binary.
func TestEnsureDelegated_WrapsUnprivileged(t *testing.T) {
	var gotPath string
	var gotArgv, gotEnv []string
	capture := func(path string, argv, env []string) error {
		gotPath, gotArgv, gotEnv = path, argv, env
		return nil
	}
	withSeams(t, func() bool { return false }, okLook, okExe, nonRoot, capture)

	if err := EnsureDelegated(io.Discard); err != nil {
		t.Fatalf("EnsureDelegated wrap path: %v", err)
	}
	if gotPath != "/usr/bin/systemd-run" {
		t.Errorf("exec path = %q, want systemd-run", gotPath)
	}
	for _, want := range []string{"--user", "--scope", "Delegate=yes", "/path/to/daft-sandboxd"} {
		if !slices.Contains(gotArgv, want) {
			t.Errorf("argv %v missing %q", gotArgv, want)
		}
	}
	if !slices.Contains(gotEnv, EnvMarker+"=1") {
		t.Errorf("env missing marker %s=1", EnvMarker)
	}
	// The "--" separator must precede the forwarded binary so systemd-run
	// stops parsing its own flags.
	sep := slices.Index(gotArgv, "--")
	bin := slices.Index(gotArgv, "/path/to/daft-sandboxd")
	if sep == -1 || bin != sep+1 {
		t.Errorf("argv %v: binary must immediately follow the -- separator", gotArgv)
	}
}

// TestEnsureDelegated_WrapsRoot: as root we use a system scope — no
// --user flag.
func TestEnsureDelegated_WrapsRoot(t *testing.T) {
	var gotArgv []string
	capture := func(_ string, argv, _ []string) error { gotArgv = argv; return nil }
	withSeams(t, func() bool { return false }, okLook, okExe, func() int { return 0 }, capture)

	if err := EnsureDelegated(io.Discard); err != nil {
		t.Fatalf("EnsureDelegated root wrap: %v", err)
	}
	if slices.Contains(gotArgv, "--user") {
		t.Errorf("argv %v contains --user as root; want a system scope", gotArgv)
	}
	if !slices.Contains(gotArgv, "--scope") {
		t.Errorf("argv %v missing --scope", gotArgv)
	}
}

// TestEnsureDelegated_MarkerStillUndelegated: already wrapped once
// (marker set) and still not delegated → hard error, no re-exec.
func TestEnsureDelegated_MarkerStillUndelegated(t *testing.T) {
	t.Setenv(EnvMarker, "1")
	withSeams(t, func() bool { return false }, okLook, okExe, nonRoot, neverExec)

	err := EnsureDelegated(io.Discard)
	if err == nil {
		t.Fatal("EnsureDelegated with marker set + no delegation: want error")
	}
	if !strings.Contains(err.Error(), "enable-linger") {
		t.Errorf("error %q should point at the remediation (enable-linger)", err)
	}
}

// TestEnsureDelegated_SystemdRunMissing: not delegated but systemd-run
// isn't on PATH → hard error rather than a silent /proc-only degrade.
func TestEnsureDelegated_SystemdRunMissing(t *testing.T) {
	missing := func(string) (string, error) { return "", errors.New("not found") }
	withSeams(t, func() bool { return false }, missing, okExe, nonRoot, neverExec)

	err := EnsureDelegated(io.Discard)
	if err == nil || !strings.Contains(err.Error(), "systemd-run") {
		t.Fatalf("want systemd-run-not-found error, got %v", err)
	}
}

// TestEnsureDelegated_ExecutableError: can't resolve our own path → error.
func TestEnsureDelegated_ExecutableError(t *testing.T) {
	badExe := func() (string, error) { return "", errors.New("boom") }
	withSeams(t, func() bool { return false }, okLook, badExe, nonRoot, neverExec)

	if err := EnsureDelegated(io.Discard); err == nil {
		t.Fatal("want error when os.Executable fails")
	}
}

// TestEnsureDelegated_ExecError: execve itself fails → wrapped error.
func TestEnsureDelegated_ExecError(t *testing.T) {
	failExec := func(string, []string, []string) error { return errors.New("execve EACCES") }
	withSeams(t, func() bool { return false }, okLook, okExe, nonRoot, failExec)

	err := EnsureDelegated(io.Discard)
	if err == nil || !strings.Contains(err.Error(), "exec systemd-run") {
		t.Fatalf("want exec error, got %v", err)
	}
}
