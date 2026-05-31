//go:build linux

package sandbox

import "syscall"

// withCgroupFD attaches the CLONE_INTO_CGROUP arguments to an
// exec.Cmd's SysProcAttr. Returns a new SysProcAttr (or mutates
// the existing one) so the caller can chain it. Linux-only — the
// `UseCgroupFD` field is gated by GOOS=linux in the syscall
// package.
func withCgroupFD(existing *syscall.SysProcAttr, fd int) *syscall.SysProcAttr {
	if existing == nil {
		existing = &syscall.SysProcAttr{}
	}
	existing.UseCgroupFD = true
	existing.CgroupFD = fd
	return existing
}

// setParentDeathSignal asks the kernel to SIGKILL the child if this
// process (sandboxd) dies, so a supervisor crash doesn't strand
// running VMMs. Linux-only — Pdeathsig is gated by GOOS=linux in the
// syscall package; the non-Linux stub is a no-op.
func setParentDeathSignal(attr *syscall.SysProcAttr) {
	attr.Pdeathsig = syscall.SIGKILL
}
