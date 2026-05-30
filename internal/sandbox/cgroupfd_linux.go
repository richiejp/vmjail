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
