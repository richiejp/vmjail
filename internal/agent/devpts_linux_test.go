//go:build linux

package agent

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fakeFileInfo is a minimal os.FileInfo for fakeStat returns.
// Only Name is meaningful; the rest satisfy the interface.
type fakeFileInfo struct{ name string }

func (f fakeFileInfo) Name() string     { return f.name }
func (fakeFileInfo) Size() int64        { return 0 }
func (fakeFileInfo) Mode() os.FileMode  { return 0o666 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() any           { return nil }

func TestEnsureDevPtsNoopWhenPtmxExists(t *testing.T) {
	called := false
	deps := devPtsDeps{
		Stat: func(path string) (fs.FileInfo, error) {
			if path != devPtsPtmx {
				t.Errorf("Stat path = %q, want %q", path, devPtsPtmx)
			}
			return fakeFileInfo{name: "ptmx"}, nil
		},
		MkdirAll: func(string, os.FileMode) error {
			called = true
			return nil
		},
		Mount: func(string, string, string, uintptr, string) error {
			called = true
			return nil
		},
	}
	if err := ensureDevPtsWith(deps); err != nil {
		t.Fatalf("ensureDevPtsWith: %v", err)
	}
	if called {
		t.Error("MkdirAll/Mount should not be called when ptmx already exists")
	}
}

func TestEnsureDevPtsMountsWhenPtmxMissing(t *testing.T) {
	var mkdirPath, mountTarget, mountFstype, mountData string
	var mountFlags uintptr
	deps := devPtsDeps{
		Stat: func(string) (fs.FileInfo, error) {
			return nil, fs.ErrNotExist
		},
		MkdirAll: func(p string, _ os.FileMode) error {
			mkdirPath = p
			return nil
		},
		Mount: func(_, target, fstype string, flags uintptr, data string) error {
			mountTarget, mountFstype, mountData, mountFlags = target, fstype, data, flags
			return nil
		},
	}
	if err := ensureDevPtsWith(deps); err != nil {
		t.Fatalf("ensureDevPtsWith: %v", err)
	}
	if mkdirPath != devPtsTarget {
		t.Errorf("MkdirAll path = %q, want %q", mkdirPath, devPtsTarget)
	}
	if mountTarget != devPtsTarget || mountFstype != "devpts" {
		t.Errorf("Mount(target=%q, fstype=%q), want (%q, devpts)", mountTarget, mountFstype, devPtsTarget)
	}
	if mountFlags&(unix.MS_NOSUID|unix.MS_NOEXEC) != (unix.MS_NOSUID | unix.MS_NOEXEC) {
		t.Errorf("Mount flags 0x%x missing NOSUID|NOEXEC", mountFlags)
	}
	for _, want := range []string{"ptmxmode=0666", "newinstance"} {
		if !strings.Contains(mountData, want) {
			t.Errorf("Mount data %q missing %q", mountData, want)
		}
	}
}

func TestEnsureDevPtsSurfacesStatError(t *testing.T) {
	deps := devPtsDeps{
		Stat: func(string) (fs.FileInfo, error) {
			return nil, errors.New("permission denied")
		},
	}
	err := ensureDevPtsWith(deps)
	if err == nil || !strings.Contains(err.Error(), "stat "+devPtsPtmx) {
		t.Errorf("ensureDevPtsWith err = %v, want wrapped stat error", err)
	}
}

func TestEnsureDevPtsSurfacesMkdirError(t *testing.T) {
	deps := devPtsDeps{
		Stat:     func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist },
		MkdirAll: func(string, os.FileMode) error { return errors.New("synthetic mkdir") },
	}
	err := ensureDevPtsWith(deps)
	if err == nil || !strings.Contains(err.Error(), "mkdir "+devPtsTarget) {
		t.Errorf("ensureDevPtsWith err = %v, want wrapped mkdir error", err)
	}
}

func TestEnsureDevPtsSurfacesMountError(t *testing.T) {
	deps := devPtsDeps{
		Stat:     func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist },
		MkdirAll: func(string, os.FileMode) error { return nil },
		Mount: func(string, string, string, uintptr, string) error {
			return errors.New("synthetic mount")
		},
	}
	err := ensureDevPtsWith(deps)
	if err == nil || !strings.Contains(err.Error(), "mount devpts at "+devPtsTarget) {
		t.Errorf("ensureDevPtsWith err = %v, want wrapped mount error", err)
	}
}

// TestEnsureDevPtsWiresRealDeps pins that the public ensureDevPts
// reaches the real os.Stat — on every dev host the test runs on
// /dev/pts/ptmx is already there, so the call is a no-op and
// must return nil.
func TestEnsureDevPtsWiresRealDeps(t *testing.T) {
	if _, err := os.Stat(devPtsPtmx); err != nil {
		t.Skipf("%s not present on this host: %v", devPtsPtmx, err)
	}
	if err := ensureDevPts(); err != nil {
		t.Errorf("ensureDevPts on host with devpts already mounted: %v", err)
	}
}
