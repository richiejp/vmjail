//go:build !linux

package sandbox

import "syscall"

// withCgroupFD is a no-op on non-Linux platforms (no CLONE_INTO_CGROUP).
// The sandbox package builds on macOS for unit tests; the cgroup
// wrap code path is gated by setupVMCgroup returning a valid fd,
// which the non-Linux observability stub refuses to do. This
// keeps the call site's compile-time graph platform-agnostic.
func withCgroupFD(existing *syscall.SysProcAttr, _ int) *syscall.SysProcAttr {
	return existing
}
