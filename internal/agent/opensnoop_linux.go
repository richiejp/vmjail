//go:build linux

package agent

import (
	"errors"
	"fmt"
	"syscall"
)

// tracefsMountpoint is the canonical tracefs path cilium/ebpf probes
// first when resolving a tracepoint id (it falls back to debugfs at
// /sys/kernel/debug/tracing). The kernel creates this directory when
// CONFIG_TRACEFS is set (selected by FTRACE); it just needs mounting.
const tracefsMountpoint = "/sys/kernel/tracing"

// ensureTracefs mounts tracefs at /sys/kernel/tracing so the open-snoop
// gadget's `tracepoint:syscalls:sys_enter_openat` attach can read the
// probe id. The minimal u-root guest mounts neither tracefs nor
// debugfs, so the agent does it — symmetric with ensureCgroupV2.
func ensureTracefs() error {
	return mountTracefsWith(syscall.Mount)
}

// mountTracefsWith is the testable core: the mount syscall is a
// parameter so a test can drive every branch without CAP_SYS_ADMIN. An
// already-mounted tracefs (EBUSY) is success, so a re-run (or a host
// that pre-mounted it) is not an error.
func mountTracefsWith(mount func(source, target, fstype string, flags uintptr, data string) error) error {
	err := mount("tracefs", tracefsMountpoint, "tracefs", 0, "")
	if err == nil || errors.Is(err, syscall.EBUSY) {
		return nil
	}
	return fmt.Errorf("agent: mount tracefs at %s: %w", tracefsMountpoint, err)
}
