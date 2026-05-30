//go:build linux

package sandbox

import (
	"syscall"
	"testing"
)

// TestWithCgroupFD_NilAttr: a nil SysProcAttr should be allocated
// and populated. This is the common case — exec.Command doesn't
// pre-set SysProcAttr.
func TestWithCgroupFD_NilAttr(t *testing.T) {
	got := withCgroupFD(nil, 7)
	if got == nil {
		t.Fatal("got nil, want allocated SysProcAttr")
	}
	if !got.UseCgroupFD {
		t.Error("UseCgroupFD = false, want true")
	}
	if got.CgroupFD != 7 {
		t.Errorf("CgroupFD = %d, want 7", got.CgroupFD)
	}
}

// TestWithCgroupFD_ExistingAttr: an existing SysProcAttr (e.g.
// one carrying Setpgid for our process-group bookkeeping) should
// be mutated in place rather than replaced, preserving its other
// fields.
func TestWithCgroupFD_ExistingAttr(t *testing.T) {
	in := &syscall.SysProcAttr{Setpgid: true}
	got := withCgroupFD(in, 11)
	if got != in {
		t.Fatal("want the same pointer back so other fields aren't dropped")
	}
	if !got.Setpgid {
		t.Error("Setpgid was clobbered")
	}
	if !got.UseCgroupFD || got.CgroupFD != 11 {
		t.Errorf("cgroup fields not set: %+v", got)
	}
}
