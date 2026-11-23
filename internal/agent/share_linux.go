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
			flags := uintptr(0)
			if fstype == "virtiofs" {
				flags = syscall.MS_NODEV | syscall.MS_NOSUID
				if data == "ro" {
					flags |= syscall.MS_RDONLY
					data = ""
				}
			}
			return syscall.Mount(source, target, fstype, flags, data)
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
