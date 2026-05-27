//go:build linux

package agent

import (
	"errors"
	"strings"
	"testing"
)

func TestMountCgroupV2(t *testing.T) {
	t.Run("success mounts cgroup2 and logs", func(t *testing.T) {
		var source, target, fstype string
		mount := func(src, tgt, fs string, _ uintptr, _ string) error {
			source, target, fstype = src, tgt, fs
			return nil
		}
		var buf strings.Builder
		mountCgroupV2(&buf, mount)

		if source != "cgroup2" || target != "/sys/fs/cgroup" || fstype != "cgroup2" {
			t.Fatalf("mount args = %q %q %q, want cgroup2 /sys/fs/cgroup cgroup2", source, target, fstype)
		}
		if !strings.Contains(buf.String(), "mounted unified hierarchy at /sys/fs/cgroup") {
			t.Fatalf("missing success message, got %q", buf.String())
		}
	})

	t.Run("failure is reported, not fatal", func(t *testing.T) {
		mount := func(_, _, _ string, _ uintptr, _ string) error {
			return errors.New("boom")
		}
		var buf strings.Builder
		mountCgroupV2(&buf, mount)

		if !strings.Contains(buf.String(), "mount /sys/fs/cgroup failed: boom") {
			t.Fatalf("missing failure message, got %q", buf.String())
		}
	})
}
