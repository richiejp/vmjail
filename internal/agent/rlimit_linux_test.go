//go:build linux

package agent

import (
	"errors"
	"strings"
	"syscall"
	"testing"
)

func TestRaiseRlimits(t *testing.T) {
	t.Run("raises NOFILE and NPROC and logs both", func(t *testing.T) {
		got := map[int]syscall.Rlimit{}
		setrlimit := func(resource int, rlim *syscall.Rlimit) error {
			got[resource] = *rlim
			return nil
		}
		var buf strings.Builder
		raiseRlimits(&buf, setrlimit)

		for _, resource := range []int{syscall.RLIMIT_NOFILE, rlimitNPROC} {
			lim, ok := got[resource]
			if !ok {
				t.Fatalf("resource %d not set", resource)
			}
			if lim.Cur != guestRlimit || lim.Max != guestRlimit {
				t.Errorf("resource %d = %+v, want cur=max=%d", resource, lim, guestRlimit)
			}
		}
		out := buf.String()
		if !strings.Contains(out, "raised NOFILE") || !strings.Contains(out, "raised NPROC") {
			t.Errorf("missing success messages, got %q", out)
		}
	})

	t.Run("a per-resource failure is reported, not fatal, and the next still runs", func(t *testing.T) {
		var calls int
		setrlimit := func(resource int, _ *syscall.Rlimit) error {
			calls++
			if resource == syscall.RLIMIT_NOFILE {
				return errors.New("boom")
			}
			return nil
		}
		var buf strings.Builder
		raiseRlimits(&buf, setrlimit)

		if calls != 2 {
			t.Errorf("setrlimit calls = %d, want 2 (NOFILE failure must not stop NPROC)", calls)
		}
		out := buf.String()
		if !strings.Contains(out, "raise NOFILE") || !strings.Contains(out, "boom") {
			t.Errorf("missing NOFILE failure message, got %q", out)
		}
		if !strings.Contains(out, "raised NPROC") {
			t.Errorf("NPROC should still be raised after NOFILE failure, got %q", out)
		}
	})
}
