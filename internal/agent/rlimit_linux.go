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
// and buildah's RUN defaults. vmjail's microVM otherwise inherits the
// kernel's pristine-low limits (NOFILE 1024/4096; NPROC RAM-derived)
// because there is no systemd/PAM in the guest to lift them — which
// surprises FD- or process-hungry workloads (databases, node, and an
// in-sandbox image build's RUN step, which forces these to ~1M).
const guestRlimit uint64 = 1 << 20

// raiseGuestRlimits lifts the agent's RLIMIT_NOFILE and RLIMIT_NPROC to at
// least guestRlimit before any container starts, so crun and the workload
// inherit a normal-host-like limit (the runtime spec leaves Rlimits empty,
// so the container inherits the agent's). The agent is guest-root with
// CAP_SYS_RESOURCE, so raising the hard limit is permitted here even
// though the zero-cap container could not raise it itself.
func raiseGuestRlimits(w io.Writer) {
	raiseRlimits(w, syscall.Getrlimit, syscall.Setrlimit)
}

// raiseRlimits is the testable core of raiseGuestRlimits: the getrlimit /
// setrlimit syscalls are parameters so a test can drive every branch
// without touching the process's real limits.
//
// It RAISES, never lowers: each soft/hard is bumped to guestRlimit only
// when it is currently below it (NPROC's kernel default scales with RAM
// and can already exceed guestRlimit on a large guest — clamping down
// would be a regression, and a function named "raise" must not lower).
// Best-effort — a per-resource failure is reported but does not fail the
// boot, and a getrlimit failure falls back to a plain raise to guestRlimit
// (we can't know the current value, but the intent is still to lift it).
func raiseRlimits(
	w io.Writer,
	getrlimit func(resource int, rlim *syscall.Rlimit) error,
	setrlimit func(resource int, rlim *syscall.Rlimit) error,
) {
	for _, r := range []struct {
		name     string
		resource int
	}{
		{"NOFILE", syscall.RLIMIT_NOFILE},
		{"NPROC", rlimitNPROC},
	} {
		var cur syscall.Rlimit
		if err := getrlimit(r.resource, &cur); err != nil {
			// Pathological (a valid resource id never fails getrlimit);
			// fall back to a plain raise rather than skip the lift.
			cur = syscall.Rlimit{}
			_, _ = fmt.Fprintf(w, "rlimit: read %s failed (%v); setting to %d\n", r.name, err, guestRlimit)
		}
		want := syscall.Rlimit{Cur: max(cur.Cur, guestRlimit), Max: max(cur.Max, guestRlimit)}
		if want == cur {
			_, _ = fmt.Fprintf(w, "rlimit: %s already >= %d (cur=%d hard=%d)\n", r.name, guestRlimit, cur.Cur, cur.Max)
			continue
		}
		if err := setrlimit(r.resource, &want); err != nil {
			_, _ = fmt.Fprintf(w, "rlimit: raise %s to %d failed: %v\n", r.name, guestRlimit, err)
			continue
		}
		_, _ = fmt.Fprintf(w, "rlimit: raised %s to cur=%d hard=%d\n", r.name, want.Cur, want.Max)
	}
}
