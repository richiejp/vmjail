//go:build linux

package agent

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestListSyscallEntriesAt covers the readdir core against a temp dir of fake
// tracefs entries (the real syscall-events dir is root-only), plus the
// missing-dir error branch.
func TestListSyscallEntriesAt(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"sys_enter_read", "sys_exit_read", "enable"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
	got, err := listSyscallEntriesAt(dir)
	if err != nil {
		t.Fatalf("listSyscallEntriesAt: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %v", len(got), got)
	}
	if _, err := listSyscallEntriesAt(filepath.Join(dir, "does-not-exist")); err == nil {
		t.Fatal("expected an error reading a missing dir")
	}
}

// TestDefaultSyscallDumpDeps covers the production wiring. listEnter reads the
// root-only tracefs, so calling it here usually errors (no CAP/over-mount under
// make check) — but the call still executes the closure + syscallEventsDir, and
// openLog round-trips a real file. The success path of listEnter is covered by
// the e2e and by TestListSyscallEntriesAt above.
func TestDefaultSyscallDumpDeps(t *testing.T) {
	d := defaultSyscallDumpDeps()
	if d.mountTracefs == nil || d.listEnter == nil || d.openLog == nil {
		t.Fatal("defaultSyscallDumpDeps left a nil primitive")
	}
	_, _ = d.listEnter() // exercise the closure + syscallEventsDir; result unchecked

	path := filepath.Join(t.TempDir(), "syscalls.txt")
	wc, err := d.openLog(path)
	if err != nil {
		t.Fatalf("openLog: %v", err)
	}
	if _, err := io.WriteString(wc, "read\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := wc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, err := os.ReadFile(path) //nolint:gosec // G304: path is t.TempDir()-derived test fixture.
	if err != nil || string(got) != "read\n" {
		t.Fatalf("read back = %q (%v), want \"read\\n\"", got, err)
	}
}
