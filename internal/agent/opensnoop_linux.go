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

// selfPIDNSPath is the procfs link whose inode IS the calling task's PID
// namespace id — the same number the kernel exposes as `ns.inum` and that
// the gadget's BPF program reads off each task. The agent stats it to learn
// its own namespace so the gadget can exclude it (see opensnoop.go).
const selfPIDNSPath = "/proc/self/ns/pid"

// readSelfPIDNS returns the inode number of the agent's PID namespace. The
// gadget treats every openat from a task in this namespace — the agent,
// guest init, kernel threads, crun — as harness noise and drops it, leaving
// only the container workload (a child PID namespace per the OCI bundle).
func readSelfPIDNS() (uint32, error) { return readSelfPIDNSAt(selfPIDNSPath) }

// readSelfPIDNSAt is the testable core: the procfs path is a parameter so a
// test can drive the stat-failure branch without unmounting /proc.
func readSelfPIDNSAt(path string) (uint32, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, fmt.Errorf("agent: stat %s: %w", path, err)
	}
	// Namespace inode numbers are the kernel's 32-bit ns.inum; the wider
	// st.Ino carries the same value, so the narrowing is exact.
	return uint32(st.Ino), nil //nolint:gosec // G115: ns.inum is 32-bit; the value fits.
}

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
