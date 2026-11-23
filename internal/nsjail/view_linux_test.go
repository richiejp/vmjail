package nsjail

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richiejp/vmjail/internal/sandbox"
	"golang.org/x/sys/unix"
)

const fsExt4 = "ext4"

func fakeMounts(t *testing.T) {
	oldM, oldA, oldI := mountFS, setMountAttrs, inspectMounts
	t.Cleanup(func() { mountFS, setMountAttrs, inspectMounts = oldM, oldA, oldI })
	mountFS = func(string, string, string, uintptr, string) error { return nil }
	inspectMounts = func() ([]mountEntry, error) { return []mountEntry{{"/", fsExt4}}, nil }
	setMountAttrs = func(int, string, uint, *unix.MountAttr) error { return nil }
}

func TestFilteredView(t *testing.T) {
	fakeMounts(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "source")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	table := []mountEntry{{"/", fsExt4}, {src + "/proc", fsProc}, {src + "/proc/nested", "sysfs"}, {src + "/ordinary", fsExt4}}
	var masked []string
	mountFS = func(_, dst, kind string, _ uintptr, _ string) error {
		if kind != fsTmpfs {
			inspectMounts = func() ([]mountEntry, error) {
				return []mountEntry{{"/", fsExt4}, {dst + "/proc", fsProc}, {dst + "/proc/nested", fsSysfs}, {dst + "/ordinary", fsExt4}}, nil
			}
		}
		if kind == fsTmpfs {
			masked = append(masked, dst)
		}
		return nil
	}
	var attrs uint64
	setMountAttrs = func(_ int, _ string, _ uint, a *unix.MountAttr) error { attrs = a.Attr_set; return nil }
	if err := filteredView(src, filepath.Join(dir, "view"), table, true); err != nil {
		t.Fatal(err)
	}
	if len(masked) != 1 || attrs&unix.MOUNT_ATTR_RDONLY == 0 || attrs&unix.MOUNT_ATTR_NODEV == 0 {
		t.Fatal(masked, attrs)
	}
	file := filepath.Join(src, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := filteredView(file, filepath.Join(dir, "file-view"), table, false); err != nil {
		t.Fatal(err)
	}
	if attrs&unix.MOUNT_ATTR_RDONLY != 0 {
		t.Fatal("writable export marked read-only")
	}
	for _, tc := range []struct {
		src, dst string
		table    []mountEntry
	}{
		{src, filepath.Join(dir, "bad"), nil},
		{src + "/missing", filepath.Join(dir, "bad"), table},
		{"/dev/null", filepath.Join(dir, "bad"), table},
		{file, filepath.Join(dir, "missing", "bad"), table},
		{src, file + "/bad", table},
	} {
		if err := filteredView(tc.src, tc.dst, tc.table, true); err == nil {
			t.Fatal("bad view accepted")
		}
	}
	mountFS = func(string, string, string, uintptr, string) error { return unix.EPERM }
	if err := filteredView(src, filepath.Join(dir, "failed"), table, true); err == nil {
		t.Fatal("bind failure")
	}
	mountFS = func(_, _, kind string, _ uintptr, _ string) error {
		if kind != fsTmpfs {
			inspectMounts = func() ([]mountEntry, error) {
				return []mountEntry{{"/", fsExt4}, {filepath.Join(dir, "mask-failed", "proc"), fsProc}}, nil
			}
		}
		if kind == fsTmpfs {
			return unix.EPERM
		}
		return nil
	}
	if err := filteredView(src, filepath.Join(dir, "mask-failed"), table, true); err == nil {
		t.Fatal("mask failure")
	}
	setMountAttrs = func(int, string, uint, *unix.MountAttr) error { return unix.EPERM }
	if err := protectView(dir, false); err == nil {
		t.Fatal("attrs failure")
	}
}

func TestMaskParent(t *testing.T) {
	fakeMounts(t)
	calls := 0
	mountFS = func(_, path, _ string, _ uintptr, _ string) error {
		calls++
		if path == "/view/locked/child" {
			return unix.EACCES
		}
		return nil
	}
	got, err := maskDirectory("/view/locked/child", "/view")
	if err != nil || got != "/view/locked" || calls != 2 {
		t.Fatal(got, err, calls)
	}
	mountFS = func(string, string, string, uintptr, string) error { return unix.EACCES }
	if _, err := maskDirectory("/view/child", "/view"); err == nil {
		t.Fatal("masked root")
	}
}

func TestPrepareSpec(t *testing.T) {
	fakeMounts(t)
	// Simulated mounts test planning and failure cleanup; real mounts are tested end to end.
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	cfg := sandbox.Config{}
	cleanup, err := prepare(Options{Root: "/", ReadOnly: []string{dir + ":/data"}, Writable: []string{dir + ":/write"}, Tmpfs: []string{"/scratch"}}, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if !cfg.HostCommand || len(cfg.Shares) != 4 || cfg.Bundle != commandBundle {
		t.Fatalf("%+v", cfg)
	}
	cleanup, err = prepare(Options{}, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	for _, o := range []Options{{Root: "/missing"}, {ReadOnly: []string{"relative"}}, {Tmpfs: []string{"relative"}}} {
		if _, err := prepare(o, &cfg); err == nil {
			t.Fatal("invalid prepare")
		}
	}
	mountFS = func(string, string, string, uintptr, string) error { return errors.New("mount") }
	if _, err := prepare(Options{}, &cfg); err == nil {
		t.Fatal("mount failure")
	}
}

func TestMountTable(t *testing.T) {
	table, err := mountTable("1 0 0:1 / / rw - ext4 root rw\n2 1 0:2 / /alternate\\040proc rw - proc none rw\n3 1 0:3 / /alternate\\040proc/data rw - tmpfs none rw\n")
	if err != nil || len(table) != 3 {
		t.Fatalf("%+v %v", table, err)
	}
	if allowedSource("/alternate proc/file", table) || !allowedSource("/alternate proc/data/file", table) || !allowedSource("/ordinary", table) || allowedSource("/unknown", nil) {
		t.Fatal("filesystem classification")
	}
	for _, s := range []string{"bad", "1 - proc", strings.Repeat("x", 70000)} {
		if _, err := mountTable(s); err == nil {
			t.Fatal("invalid mountinfo accepted")
		}
	}
	if beneath("/devil", "/dev") || !beneath("/anything", "/") {
		t.Fatal("path boundary")
	}
}
