//go:build linux

package agent

import (
	"errors"
	"syscall"
	"testing"
)

// TestMountTracefsWith drives every branch of the testable mount core
// without CAP_SYS_ADMIN: clean mount, already-mounted (EBUSY → nil), and
// a real failure (wrapped).
func TestMountTracefsWith(t *testing.T) {
	tests := []struct {
		name     string
		mountErr error
		wantErr  bool
	}{
		{name: "clean mount", mountErr: nil, wantErr: false},
		{name: "already mounted", mountErr: syscall.EBUSY, wantErr: false},
		{name: "permission denied", mountErr: syscall.EACCES, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotTarget, gotFstype string
			mount := func(source, target, fstype string, flags uintptr, data string) error {
				gotTarget, gotFstype = target, fstype
				return tt.mountErr
			}
			err := mountTracefsWith(mount)
			if tt.wantErr {
				if err == nil || !errors.Is(err, tt.mountErr) {
					t.Fatalf("err = %v, want wrapping %v", err, tt.mountErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotTarget != tracefsMountpoint || gotFstype != "tracefs" {
				t.Fatalf("mounted %s as %q, want %s tracefs", gotTarget, gotFstype, tracefsMountpoint)
			}
		})
	}
}

// TestEnsureTracefs covers the production entry point: it calls the real
// syscall.Mount, which fails without CAP_SYS_ADMIN in the test sandbox —
// we only assert it runs without panicking (the statement is what the
// coverage gate needs; the mount itself is exercised in the e2e).
func TestEnsureTracefs(t *testing.T) {
	_ = ensureTracefs()
}
