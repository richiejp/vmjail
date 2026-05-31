//go:build linux && integration

package testdaemons

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadTail pins the bounded tail reader dumpObserverLogs uses: it
// returns whole trailing lines, caps by line count, and — when the
// byte cap bisects a line — drops the leading fragment so every line
// logged is intact JSON.
func TestReadTail(t *testing.T) {
	t.Run("whole small file, under both caps", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "observer.jsonl")
		writeForTail(t, path, "a\nb\nc\n")
		got, err := readTail(path, 1024, 10)
		if err != nil {
			t.Fatalf("readTail: %v", err)
		}
		if got != "a\nb\nc" {
			t.Errorf("readTail = %q, want the whole file (no trailing newline)", got)
		}
	})

	t.Run("line cap keeps only the last N", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "observer.jsonl")
		var b strings.Builder
		for i := range 10 {
			fmt.Fprintf(&b, "line%d\n", i)
		}
		writeForTail(t, path, b.String())
		got, err := readTail(path, 1<<20, 3)
		if err != nil {
			t.Fatalf("readTail: %v", err)
		}
		if got != "line7\nline8\nline9" {
			t.Errorf("readTail = %q, want the last 3 lines", got)
		}
	})

	t.Run("byte cap drops the leading partial line", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "observer.jsonl")
		// Three 10-byte lines (incl. newline). A 15-byte cap starts mid
		// way through line 1, so its fragment must be dropped.
		writeForTail(t, path, "aaaaaaaaa\nbbbbbbbbb\nccccccccc\n")
		got, err := readTail(path, 15, 10)
		if err != nil {
			t.Fatalf("readTail: %v", err)
		}
		if strings.Contains(got, "a") {
			t.Errorf("readTail = %q, want no fragment of the byte-capped first line", got)
		}
		if got != "ccccccccc" && got != "bbbbbbbbb\nccccccccc" {
			t.Errorf("readTail = %q, want whole trailing lines only", got)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		if _, err := readTail(filepath.Join(t.TempDir(), "nope"), 1024, 10); err == nil {
			t.Fatal("expected an error for a missing file")
		}
	})
}

// TestDumpObserverLogsNoFiles confirms the no-log path is reported, not
// silently skipped, so a failing test that never booted a VM still says
// so rather than leaving the reader wondering.
func TestDumpObserverLogsNoFiles(t *testing.T) {
	// dumpObserverLogs only logs (t.Logf); calling it on an empty dir
	// must not fail or panic. The "no observer logs" branch is exercised.
	dumpObserverLogs(t, t.TempDir())
}

func writeForTail(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
