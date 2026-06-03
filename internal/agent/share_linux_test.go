//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultShareDepsWiresStdlibCalls exercises the body of every
// closure in defaultShareDeps so the Mount / MkdirAll / ReadFile
// paths show as covered. Mount is expected to fail without
// privileges; we just need to invoke it.
func TestDefaultShareDepsWiresStdlibCalls(t *testing.T) {
	deps := defaultShareDeps()
	if deps.Mount == nil || deps.MkdirAll == nil || deps.ReadFile == nil {
		t.Fatalf("defaultShareDeps incomplete: %+v", deps)
	}

	// MkdirAll on a t.TempDir() child: fast, no privileges needed.
	dir := t.TempDir() + "/sub/dir"
	if err := deps.MkdirAll(dir, 0o700); err != nil {
		t.Errorf("MkdirAll: %v", err)
	}

	// ReadFile of an existing file.
	if _, err := deps.ReadFile("/proc/self/comm"); err != nil {
		t.Errorf("ReadFile: %v", err)
	}

	// Mount(2) requires CAP_SYS_ADMIN. We expect it to fail in any
	// non-root test environment — but the closure body must execute
	// for coverage. A bogus fstype guarantees ENODEV / EPERM.
	if err := deps.Mount("none", t.TempDir(), "no_such_fs", ""); err == nil {
		t.Error("expected Mount to fail for bogus fstype")
	}
}

func TestDefaultContainerDepsWiresExec(t *testing.T) {
	deps := defaultContainerDeps()
	if deps.Exec == nil {
		t.Fatal("defaultContainerDeps.Exec is nil")
	}
}

// TestReadKernelCmdlineReadsProc just exercises the success branch
// — every Linux kernel exposes /proc/cmdline, and a test process
// running on the host has read permission.
func TestReadKernelCmdlineReadsProc(t *testing.T) {
	got, err := readKernelCmdline()
	if err != nil {
		t.Fatalf("readKernelCmdline: %v", err)
	}
	if got == "" {
		t.Error("readKernelCmdline returned empty cmdline")
	}
}

// TestReadCmdlineFromPropagatesError covers the error-return branch
// for real, via the injectable path seam: a nonexistent file must
// surface its open error rather than being swallowed. (Previously this
// was a tautology that read a different file and asserted a constant
// string — the read path was hard-coded with no seam.)
func TestReadCmdlineFromPropagatesError(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "definitely-absent")
	got, err := readCmdlineFrom(bad)
	if err == nil {
		t.Fatalf("readCmdlineFrom(%q) = %q, nil; want an error", bad, got)
	}
	if !os.IsNotExist(err) {
		t.Errorf("err = %v, want a not-exist error", err)
	}
	if got != "" {
		t.Errorf("on error, returned %q, want empty string", got)
	}
}

// TestReadCmdlineFromReadsFile covers the success branch through the
// same seam without depending on /proc, so the parse/return path is
// exercised deterministically.
func TestReadCmdlineFromReadsFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cmdline")
	const want = "daft.bundle=sha256:abc rw quiet\n"
	if err := os.WriteFile(p, []byte(want), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readCmdlineFrom(p)
	if err != nil {
		t.Fatalf("readCmdlineFrom: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
