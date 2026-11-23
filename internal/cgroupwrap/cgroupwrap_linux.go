//go:build linux

package cgroupwrap

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/richiejp/vmjail/internal/observability"
)

// Test seams: package-level vars so unit tests can drive every branch
// without a real systemd-run, a real cgroup, or actually replacing the
// process image.
//
//nolint:gochecknoglobals // test seams, fixed in production.
var (
	delegationOK = observability.DelegationOK
	lookPath     = exec.LookPath
	osExecutable = os.Executable
	getuid       = os.Getuid
	execFn       = syscall.Exec
)

// EnsureDelegated makes sure the calling process owns a delegated,
// writable cgroup v2 subtree it can create per-VM child cgroups in.
//
// Fast path: if the inherited cgroup is already delegated (DelegationOK),
// returns nil immediately — no wrap, no process-tree change. This is the
// common case under the test harness (the go-test process is already in
// a delegated scope) and under a production systemd unit with
// Delegate=yes.
//
// Otherwise it re-execs the process under `systemd-run [--user] --scope
// -p Delegate=yes -- <self> <args...>` via execve, so systemd-run
// replaces this process and becomes the parent that owns the transient
// scope for the process's lifetime. On success execve does not return.
//
// If systemd-run is unavailable, or the process has *already* been
// re-exec'd once (EnvMarker set) and delegation is still missing, it
// returns a fatal error with remediation — the caller must treat this as
// a startup failure, never a degrade.
func EnsureDelegated(stderr io.Writer) error {
	if delegationOK() {
		return nil
	}
	return ReexecDelegated(stderr)
}

// ReexecDelegated obtains a fresh scope when an inherited delegated scope is
// shared with other processes and cannot enable its controllers. On success
// it replaces the process image; the marker prevents a re-exec loop.
func ReexecDelegated(stderr io.Writer) error {
	if os.Getenv(EnvMarker) == "1" {
		return fmt.Errorf("cgroupwrap: cgroup v2 delegation still unavailable after re-exec " +
			"under a systemd-run scope; per-VM accounting cgroups are required and cannot be " +
			"created. Run the sandbox host under a delegated cgroup — e.g. `loginctl enable-linger` " +
			"for a headless/CI user so a --user scope can be created, or launch it from a systemd " +
			"unit with Delegate=yes")
	}
	self, err := osExecutable()
	if err != nil {
		return fmt.Errorf("cgroupwrap: locate own executable for re-exec: %w", err)
	}
	srun, err := lookPath("systemd-run")
	if err != nil {
		return fmt.Errorf("cgroupwrap: systemd-run is required to obtain a delegated cgroup but "+
			"was not found on PATH: %w", err)
	}
	argv := []string{srun}
	if getuid() != 0 {
		// Unprivileged: a transient scope under the caller's own
		// user@<uid>.service, which systemd already delegates. As root
		// we use a system scope (no --user) instead.
		argv = append(argv, "--user")
	}
	argv = append(argv, "--scope", "-p", "Delegate=yes", "--quiet", "--", self)
	argv = append(argv, os.Args[1:]...)
	env := append(os.Environ(), EnvMarker+"=1")
	_, _ = fmt.Fprintf(stderr, "%s: re-exec under a systemd-run scope for cgroup delegation\n", filepath.Base(self))
	// execve: on success the process image is replaced and this never
	// returns. The re-exec'd self re-enters EnsureDelegated, finds itself
	// delegated, and proceeds.
	if err := execFn(srun, argv, env); err != nil {
		return fmt.Errorf("cgroupwrap: exec systemd-run: %w", err)
	}
	return nil // unreachable on a successful execve
}
