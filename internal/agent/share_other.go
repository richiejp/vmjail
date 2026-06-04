//go:build !linux

package agent

import (
	"context"
	"errors"
	"io"
	"os"
)

// defaultShareDeps on non-Linux returns a Mount that always errors —
// virtiofs/overlayfs/tmpfs are Linux-kernel features. The agent
// binary is only shipped for Linux (see flake.nix); the package
// compiles everywhere so editor tooling and `go vet ./...` work
// for developers on macOS.
func defaultShareDeps() ShareDeps {
	return ShareDeps{
		Mount: func(_, _, _, _ string) error {
			return errors.New("agent: mount(2) unsupported off Linux")
		},
		MkdirAll: os.MkdirAll,
		ReadFile: os.ReadFile,
	}
}

// defaultContainerDeps on non-Linux returns Exec that always errors —
// crun is Linux-only.
func defaultContainerDeps() ContainerDeps {
	return ContainerDeps{
		Exec: func(_ context.Context, _ io.Writer, _ string) error {
			return errors.New("agent: container exec unsupported off Linux")
		},
	}
}
