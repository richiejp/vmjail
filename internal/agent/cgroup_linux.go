//go:build linux

package agent

import (
	"fmt"
	"io"
	"syscall"
)

// ensureCgroupV2 mounts the unified cgroup v2 hierarchy at
// /sys/fs/cgroup so crun manages the container under v2 (eBPF device
// controller + native cgroup.freeze) instead of the legacy v1 layout
// u-root's libinit sets up. It stacks over u-root's best-effort tmpfs
// mount; with `cgroup_no_v1=all` on the kernel cmdline the v1
// controller mounts no-op, leaving every controller free for v2.
//
// Best-effort: a mount failure is reported but does not fail the boot,
// so a host without the v2 controllers still limps to a usable state.
func ensureCgroupV2(w io.Writer) {
	mountCgroupV2(w, syscall.Mount)
}

// mountCgroupV2 is the testable core of ensureCgroupV2: the mount syscall
// is a parameter so a test can drive both branches without CAP_SYS_ADMIN
// or actually mounting over /sys/fs/cgroup.
func mountCgroupV2(w io.Writer, mount func(source, target, fstype string, flags uintptr, data string) error) {
	if err := mount("cgroup2", "/sys/fs/cgroup", "cgroup2", 0, ""); err != nil {
		_, _ = fmt.Fprintf(w, "cgroup2: mount /sys/fs/cgroup failed: %v\n", err)
		return
	}
	_, _ = fmt.Fprintln(w, "cgroup2: mounted unified hierarchy at /sys/fs/cgroup")
}
