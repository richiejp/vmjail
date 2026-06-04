//go:build linux

package agent

import (
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
