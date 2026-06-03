//go:build linux

package agent

import (
	"os"
	"syscall"
)

// defaultShareDeps wires the real Linux mount(2) syscall + stdlib
// filesystem helpers. Used by Run() in production.
func defaultShareDeps() ShareDeps {
	return ShareDeps{
		Mount: func(source, target, fstype, data string) error {
			return syscall.Mount(source, target, fstype, 0, data)
		},
		MkdirAll: os.MkdirAll,
		ReadFile: os.ReadFile,
	}
}

// defaultContainerDeps wires execContainerLinux. Used by Run() in
// production; tests inject their own ContainerDeps.
func defaultContainerDeps() ContainerDeps {
	return ContainerDeps{Exec: execContainerLinux}
}

// readKernelCmdline returns the contents of /proc/cmdline. Errors
// short-circuit cmdline-driven features (sharing) without failing the
// agent — Linux without /proc is exotic enough that we treat it as a
// "no-features" scenario rather than a panic case.
func readKernelCmdline() (string, error) {
	return readCmdlineFrom("/proc/cmdline")
}

// readCmdlineFrom is the path-injectable core of readKernelCmdline,
// split out so the error-return branch is testable for real: production
// always passes the fixed "/proc/cmdline", tests pass a nonexistent
// path to exercise the failure path (rather than asserting against a
// hard-coded read that can't fail).
func readCmdlineFrom(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // production path is the constant "/proc/cmdline"; tests inject a temp path.
	if err != nil {
		return "", err
	}
	return string(b), nil
}
