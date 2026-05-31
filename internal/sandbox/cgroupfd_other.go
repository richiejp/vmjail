//go:build !linux

package sandbox

import "syscall"

// withCgroupFD is a no-op on non-Linux platforms (no CLONE_INTO_CGROUP).
// The sandbox package builds on macOS for unit tests; the cgroup
// wrap code path is gated by placeInCgroup opening a valid fd,
// which the non-Linux observability stub refuses to do. This
// keeps the call site's compile-time graph platform-agnostic.
func withCgroupFD(existing *syscall.SysProcAttr, _ int) *syscall.SysProcAttr {
	return existing
}

// setParentDeathSignal is a no-op on non-Linux platforms: Pdeathsig
// is a Linux-only SysProcAttr field. The sandbox package builds on
// macOS for unit tests; the parent-death-kill guarantee simply
// doesn't apply there.
func setParentDeathSignal(_ *syscall.SysProcAttr) {}
