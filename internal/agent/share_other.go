//go:build !linux

package agent

import (
	"errors"
	"os"
)

// defaultShareDeps on non-Linux returns a Mount that always errors —
// virtiofs is a Linux thing. The agent binary is only shipped for
// Linux (see flake.nix), but the package compiles everywhere so
// editor tooling and `go vet ./...` work for developers on macOS.
func defaultShareDeps() ShareDeps {
	return ShareDeps{
		Mount: func(_, _, _ string) error {
			return errors.New("agent: virtiofs mount unsupported off Linux")
		},
		MkdirAll: os.MkdirAll,
		ReadFile: os.ReadFile,
	}
}

func readKernelCmdline() (string, error) {
	return "", errors.New("agent: /proc/cmdline unsupported off Linux")
}
