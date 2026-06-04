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
