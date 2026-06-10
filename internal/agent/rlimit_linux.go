//go:build linux

package agent

import (
	"fmt"
	"io"
	"syscall"
)

// rlimitNPROC is the Linux RLIMIT_NPROC resource id. The stdlib syscall
// package exports RLIMIT_NOFILE but not NPROC; the value is stable kernel
// ABI.
const rlimitNPROC = 6

// guestRlimit is the soft+hard value raiseGuestRlimits sets NOFILE and
// NPROC to. 1048576 matches a normal systemd host (DefaultLimitNOFILE)
// and buildah's RUN defaults. daft's microVM otherwise inherits the
// kernel's pristine-low limits (NOFILE 1024/4096; NPROC RAM-derived)
// because there is no systemd/PAM in the guest to lift them — which
// surprises FD- or process-hungry workloads (databases, node, and an
// in-sandbox image build's RUN step, which forces these to ~1M).
const guestRlimit uint64 = 1 << 20

// raiseGuestRlimits lifts the agent's RLIMIT_NOFILE and RLIMIT_NPROC to
// guestRlimit before any container starts, so crun and the workload
// inherit a normal-host-like limit (the runtime spec leaves Rlimits
// empty, so the container inherits the agent's). The agent is guest-root
// with CAP_SYS_RESOURCE, so raising the hard limit is permitted here even
// though the zero-cap container could not raise it itself.
func raiseGuestRlimits(w io.Writer) {
	raiseRlimits(w, syscall.Setrlimit)
}

// raiseRlimits is the testable core of raiseGuestRlimits: the setrlimit
// syscall is a parameter so a test can drive both branches without
// touching the process's real limits. Best-effort — a per-resource
// failure is reported but does not fail the boot.
func raiseRlimits(w io.Writer, setrlimit func(resource int, rlim *syscall.Rlimit) error) {
	for _, r := range []struct {
		name     string
		resource int
	}{
		{"NOFILE", syscall.RLIMIT_NOFILE},
		{"NPROC", rlimitNPROC},
	} {
		lim := syscall.Rlimit{Cur: guestRlimit, Max: guestRlimit}
		if err := setrlimit(r.resource, &lim); err != nil {
			_, _ = fmt.Fprintf(w, "rlimit: raise %s to %d failed: %v\n", r.name, guestRlimit, err)
			continue
		}
		_, _ = fmt.Fprintf(w, "rlimit: raised %s to %d\n", r.name, guestRlimit)
	}
}
