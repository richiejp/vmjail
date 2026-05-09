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
		Mount: func(source, target, fstype string) error {
			return syscall.Mount(source, target, fstype, 0, "")
		},
		MkdirAll: os.MkdirAll,
		ReadFile: os.ReadFile,
	}
}

// readKernelCmdline returns the contents of /proc/cmdline. Errors
// short-circuit cmdline-driven features (sharing) without failing the
// agent — Linux without /proc is exotic enough that we treat it as a
// "no-features" scenario rather than a panic case.
func readKernelCmdline() (string, error) {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return "", err
	}
	return string(b), nil
}
