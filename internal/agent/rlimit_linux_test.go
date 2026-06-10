//go:build linux

package agent

import (
	"errors"
	"strings"
	"syscall"
	"testing"
)

// fakeRlimits backs a getrlimit/setrlimit pair over an in-memory map so a
// test can assert what raiseRlimits would set without touching the
// process's real limits.
type fakeRlimits struct {
	cur    map[int]syscall.Rlimit // current limits getrlimit returns
	getErr map[int]error          // getrlimit failure per resource
	setErr map[int]error          // setrlimit failure per resource
	set    map[int]syscall.Rlimit // what setrlimit was called with
	sets   int                    // total setrlimit calls
}

func newFakeRlimits() *fakeRlimits {
	return &fakeRlimits{
		cur:    map[int]syscall.Rlimit{},
		getErr: map[int]error{},
		setErr: map[int]error{},
		set:    map[int]syscall.Rlimit{},
	}
}

func (f *fakeRlimits) get(resource int, rlim *syscall.Rlimit) error {
	if err := f.getErr[resource]; err != nil {
		return err
	}
	*rlim = f.cur[resource]
	return nil
}

func (f *fakeRlimits) setf(resource int, rlim *syscall.Rlimit) error {
	f.sets++
	if err := f.setErr[resource]; err != nil {
		return err
	}
	f.set[resource] = *rlim
	return nil
}

func TestRaiseRlimitsRaisesWhenBelowTarget(t *testing.T) {
	f := newFakeRlimits()
	f.cur[syscall.RLIMIT_NOFILE] = syscall.Rlimit{Cur: 1024, Max: 4096}
	f.cur[rlimitNPROC] = syscall.Rlimit{Cur: 7929, Max: 7929}

	var buf strings.Builder
	raiseRlimits(&buf, f.get, f.setf)

	for _, resource := range []int{syscall.RLIMIT_NOFILE, rlimitNPROC} {
		got := f.set[resource]
		if got.Cur != guestRlimit || got.Max != guestRlimit {
			t.Errorf("resource %d set to %+v, want cur=max=%d", resource, got, guestRlimit)
		}
	}
	if !strings.Contains(buf.String(), "raised NOFILE") || !strings.Contains(buf.String(), "raised NPROC") {
		t.Errorf("missing raised messages: %q", buf.String())
	}
}

// TestRaiseRlimitsNeverLowers is the review fix: a current hard limit
// already above guestRlimit (NPROC on a large-RAM guest) must NOT be
// clamped down — raiseRlimits leaves it untouched and skips the setrlimit.
func TestRaiseRlimitsNeverLowers(t *testing.T) {
	const high = guestRlimit * 4
	f := newFakeRlimits()
	f.cur[syscall.RLIMIT_NOFILE] = syscall.Rlimit{Cur: high, Max: high} // both already high
	f.cur[rlimitNPROC] = syscall.Rlimit{Cur: 1024, Max: high}           // soft low, hard high

	var buf strings.Builder
	raiseRlimits(&buf, f.get, f.setf)

	// NOFILE already >= target on both → no set call at all.
	if _, set := f.set[syscall.RLIMIT_NOFILE]; set {
		t.Errorf("NOFILE was set despite already being high: %+v", f.set[syscall.RLIMIT_NOFILE])
	}
	// NPROC: soft below target is lifted, but the high hard is preserved
	// (not lowered to guestRlimit).
	np := f.set[rlimitNPROC]
	if np.Cur != guestRlimit {
		t.Errorf("NPROC soft = %d, want raised to %d", np.Cur, guestRlimit)
	}
	if np.Max != high {
		t.Errorf("NPROC hard = %d, want preserved at %d (must not lower)", np.Max, high)
	}
}

func TestRaiseRlimitsGetFailureFallsBackToPlainRaise(t *testing.T) {
	f := newFakeRlimits()
	f.getErr[syscall.RLIMIT_NOFILE] = errors.New("getrlimit boom")
	f.cur[rlimitNPROC] = syscall.Rlimit{Cur: 1024, Max: 4096}

	var buf strings.Builder
	raiseRlimits(&buf, f.get, f.setf)

	// On get failure we still attempt a raise to guestRlimit.
	if got := f.set[syscall.RLIMIT_NOFILE]; got.Cur != guestRlimit || got.Max != guestRlimit {
		t.Errorf("NOFILE after get failure = %+v, want cur=max=%d", got, guestRlimit)
	}
	if !strings.Contains(buf.String(), "read NOFILE failed") {
		t.Errorf("missing get-failure breadcrumb: %q", buf.String())
	}
}

func TestRaiseRlimitsSetFailureIsNotFatalAndNextStillRuns(t *testing.T) {
	f := newFakeRlimits()
	f.cur[syscall.RLIMIT_NOFILE] = syscall.Rlimit{Cur: 1024, Max: 4096}
	f.cur[rlimitNPROC] = syscall.Rlimit{Cur: 1024, Max: 4096}
	f.setErr[syscall.RLIMIT_NOFILE] = errors.New("setrlimit boom")

	var buf strings.Builder
	raiseRlimits(&buf, f.get, f.setf)

	if f.sets != 2 {
		t.Errorf("setrlimit calls = %d, want 2 (NOFILE failure must not stop NPROC)", f.sets)
	}
	if got := f.set[rlimitNPROC]; got.Cur != guestRlimit {
		t.Errorf("NPROC not raised after NOFILE set failure: %+v", got)
	}
	out := buf.String()
	if !strings.Contains(out, "raise NOFILE") || !strings.Contains(out, "boom") {
		t.Errorf("missing NOFILE failure message: %q", out)
	}
}
