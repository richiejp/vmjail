package pty

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpenPair(t *testing.T) {
	master, slave, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })

	want := unix.Winsize{Col: 132, Row: 50}
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &want); err != nil {
		t.Fatal(err)
	}
	got, err := unix.IoctlGetWinsize(int(slave.Fd()), unix.TIOCGWINSZ)
	if err != nil || *got != want {
		t.Fatalf("slave size = %v, %v; want %v", got, err, want)
	}
}

func TestOpenPairRejectsMissingMaster(t *testing.T) {
	if _, _, err := openPair(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing master: %v", err)
	}
}

func TestOpenPairClosesInvalidMaster(t *testing.T) {
	path := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openPair(path); !errors.Is(err, unix.ENOTTY) {
		t.Fatalf("regular master: %v", err)
	}

	const fdDir = "/proc/self/fd"
	fds, err := os.ReadDir(fdDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		// File descriptors can disappear between enumeration and readlink.
		target, _ := os.Readlink(filepath.Join(fdDir, fd.Name()))
		if target == path {
			t.Fatalf("invalid master still open at fd %s", fd.Name())
		}
	}
}
