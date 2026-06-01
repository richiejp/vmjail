package progress

import (
	"os"
	"path/filepath"
	"testing"
)

func sampleSummary() Summary {
	return Summary{
		TakenAtUnixMs: 1_700_000_000_000,
		Opensnoop: &Opensnoop{
			Total:          42,
			OpenatRate:     3.5,
			LastEventAgeMs: 120,
			TopComms:       []Counted{{Key: "cc1", Count: 30}},
			TopPaths:       []Counted{{Key: "/nix/store/abc/x.h", Count: 12}},
			TopDirs:        []Counted{{Key: "/nix/store/abc", Count: 30}},
		},
	}
}

// TestMarshalParseRoundTrips proves the wire contract is stable: a Summary
// survives Marshal→Parse unchanged, and the rendered form is human-readable
// (indented, newline-terminated).
func TestMarshalParseRoundTrips(t *testing.T) {
	want := sampleSummary()
	data, err := Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if data[len(data)-1] != '\n' {
		t.Fatal("Marshal output should end in a newline")
	}
	if !containsByte(data, '\n', 2) {
		t.Fatal("Marshal output should be indented (multi-line)")
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.TakenAtUnixMs != want.TakenAtUnixMs || got.Opensnoop == nil {
		t.Fatalf("round-trip envelope mismatch: %+v", got)
	}
	if got.Opensnoop.Total != 42 || got.Opensnoop.OpenatRate != 3.5 || got.Opensnoop.LastEventAgeMs != 120 {
		t.Fatalf("round-trip opensnoop scalars mismatch: %+v", got.Opensnoop)
	}
	if len(got.Opensnoop.TopPaths) != 1 || got.Opensnoop.TopPaths[0].Key != "/nix/store/abc/x.h" {
		t.Fatalf("round-trip rankings mismatch: %+v", got.Opensnoop.TopPaths)
	}
}

// TestParseRejectsGarbage covers the decode error path.
func TestParseRejectsGarbage(t *testing.T) {
	if _, err := Parse([]byte("{not json")); err == nil {
		t.Fatal("Parse should reject malformed JSON")
	}
}

// TestWriteAtomicReplacesInPlace proves WriteAtomic overwrites an existing
// file with the new summary and leaves no temp file behind — the polling
// reader always sees a complete summary.
func TestWriteAtomicReplacesInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "summary.json")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}
	if err := WriteAtomic(path, sampleSummary()); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: t.TempDir()-derived path.
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse written file: %v", err)
	}
	if got.Opensnoop == nil || got.Opensnoop.Total != 42 {
		t.Fatalf("file does not hold the new summary: %+v", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file should not survive WriteAtomic, stat err = %v", err)
	}
}

// TestWriteAtomicErrorsOnBadDir covers the temp-write failure path.
func TestWriteAtomicErrorsOnBadDir(t *testing.T) {
	if err := WriteAtomic(filepath.Join(t.TempDir(), "no-such-dir", "s.json"), sampleSummary()); err == nil {
		t.Fatal("WriteAtomic should fail when the parent directory is missing")
	}
}

// TestWriteAtomicErrorsOnRename covers the rename-failure branch: the temp
// file writes fine but the destination is a directory, so the rename over it
// fails and the temp file is cleaned up.
func TestWriteAtomicErrorsOnRename(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "summary.json")
	if err := os.Mkdir(dest, 0o750); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	if err := WriteAtomic(dest, sampleSummary()); err == nil {
		t.Fatal("WriteAtomic should fail renaming a file over a directory")
	}
	if _, err := os.Stat(dest + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file should be cleaned up after a failed rename, stat err = %v", err)
	}
}

// containsByte reports whether b contains at least want occurrences of c.
func containsByte(b []byte, c byte, want int) bool {
	n := 0
	for _, x := range b {
		if x == c {
			n++
		}
	}
	return n >= want
}
