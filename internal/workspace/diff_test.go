package workspace_test

import (
	"testing"

	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/workspace"
)

func file(path string, mode uint32, digests ...string) *workspacev1.FileEntry {
	chunks := make([]*workspacev1.ChunkRef, len(digests))
	var size uint64
	for i, d := range digests {
		chunks[i] = &workspacev1.ChunkRef{Digest: d, Size: 1}
		size++
	}
	return &workspacev1.FileEntry{
		Path:   path,
		Kind:   workspacev1.EntryKind_ENTRY_KIND_FILE,
		Mode:   mode,
		Chunks: chunks,
		Size:   size,
	}
}

func dir(path string, mode uint32) *workspacev1.FileEntry {
	return &workspacev1.FileEntry{
		Path: path, Kind: workspacev1.EntryKind_ENTRY_KIND_DIR, Mode: mode,
	}
}

func sym(path, target string) *workspacev1.FileEntry {
	return &workspacev1.FileEntry{
		Path: path, Kind: workspacev1.EntryKind_ENTRY_KIND_SYMLINK,
		Mode: 0o777, SymlinkTarget: target,
	}
}

func manifest(entries ...*workspacev1.FileEntry) *workspacev1.Manifest {
	return &workspacev1.Manifest{Entries: entries}
}

func TestDiffEmpty(t *testing.T) {
	if !workspace.DiffManifests(nil, nil).Empty() {
		t.Error("nil/nil should be empty")
	}
	m := manifest(file("a", 0o644, "d1"))
	if !workspace.DiffManifests(m, m).Empty() {
		t.Error("same manifest should be empty")
	}
}

func TestDiffAddedRemoved(t *testing.T) {
	from := manifest(file("keep", 0o644, "d1"), file("gone", 0o644, "d2"))
	to := manifest(file("keep", 0o644, "d1"), file("new", 0o600, "d3"))
	d := workspace.DiffManifests(from, to)
	if d.Empty() {
		t.Fatal("expected non-empty diff")
	}
	if len(d.Changes) != 2 {
		t.Fatalf("changes = %d, want 2", len(d.Changes))
	}
	// Sorted lex: "gone" before "new".
	if d.Changes[0].Path != "gone" || d.Changes[0].Kind != workspace.Removed {
		t.Errorf("changes[0] = %+v", d.Changes[0])
	}
	if d.Changes[1].Path != "new" || d.Changes[1].Kind != workspace.Added {
		t.Errorf("changes[1] = %+v", d.Changes[1])
	}
	added, removed, mod, modeC, typeC := d.Counts()
	if added != 1 || removed != 1 || mod != 0 || modeC != 0 || typeC != 0 {
		t.Errorf("counts = (+%d -%d ~%d m%d t%d)", added, removed, mod, modeC, typeC)
	}
}

func TestDiffModified(t *testing.T) {
	from := manifest(file("x", 0o644, "d1"))
	to := manifest(file("x", 0o644, "d2"))
	d := workspace.DiffManifests(from, to)
	if len(d.Changes) != 1 || d.Changes[0].Kind != workspace.Modified {
		t.Fatalf("changes = %+v", d.Changes)
	}
}

func TestDiffSymlinkTargetChange(t *testing.T) {
	from := manifest(sym("l", "old"))
	to := manifest(sym("l", "new"))
	d := workspace.DiffManifests(from, to)
	if len(d.Changes) != 1 || d.Changes[0].Kind != workspace.Modified {
		t.Fatalf("changes = %+v", d.Changes)
	}
}

func TestDiffModeOnly(t *testing.T) {
	from := manifest(file("x", 0o644, "d1"))
	to := manifest(file("x", 0o600, "d1"))
	d := workspace.DiffManifests(from, to)
	if len(d.Changes) != 1 || d.Changes[0].Kind != workspace.ModeChanged {
		t.Fatalf("changes = %+v", d.Changes)
	}
}

func TestDiffTypeChange(t *testing.T) {
	from := manifest(file("x", 0o644, "d1"))
	to := manifest(dir("x", 0o755))
	d := workspace.DiffManifests(from, to)
	if len(d.Changes) != 1 || d.Changes[0].Kind != workspace.TypeChanged {
		t.Fatalf("changes = %+v", d.Changes)
	}
}

func TestDiffMtimeIgnored(t *testing.T) {
	from := manifest(file("x", 0o644, "d1"))
	to := manifest(file("x", 0o644, "d1"))
	from.Entries[0].MtimeUnixNano = 1000
	to.Entries[0].MtimeUnixNano = 9999
	d := workspace.DiffManifests(from, to)
	if !d.Empty() {
		t.Errorf("mtime-only diff should be empty: %+v", d.Changes)
	}
}

func TestDiffChunkSizeMismatch(t *testing.T) {
	a := manifest(file("x", 0o644, "d1"))
	a.Entries[0].Chunks[0].Size = 10
	a.Entries[0].Size = 10
	b := manifest(file("x", 0o644, "d1"))
	b.Entries[0].Chunks[0].Size = 20
	b.Entries[0].Size = 20
	d := workspace.DiffManifests(a, b)
	if len(d.Changes) != 1 || d.Changes[0].Kind != workspace.Modified {
		t.Fatalf("changes = %+v", d.Changes)
	}
}

func TestDiffChunkListLengthMismatch(t *testing.T) {
	from := manifest(file("x", 0o644, "d1"))
	to := manifest(file("x", 0o644, "d1", "d2"))
	d := workspace.DiffManifests(from, to)
	if len(d.Changes) != 1 || d.Changes[0].Kind != workspace.Modified {
		t.Fatalf("changes = %+v", d.Changes)
	}
}

func TestDiffCountsCoverAllKinds(t *testing.T) {
	from := manifest(
		file("mod", 0o644, "d1"),
		file("mode", 0o644, "d2"),
		file("type", 0o644, "d3"),
		file("gone", 0o644, "d4"),
	)
	to := manifest(
		file("mod", 0o644, "d1x"),
		file("mode", 0o600, "d2"),
		dir("type", 0o755),
		file("new", 0o644, "d5"),
	)
	added, removed, mod, modeC, typeC := workspace.DiffManifests(from, to).Counts()
	if added != 1 || removed != 1 || mod != 1 || modeC != 1 || typeC != 1 {
		t.Errorf("counts = (+%d -%d ~%d m%d t%d), want all 1s",
			added, removed, mod, modeC, typeC)
	}
}

func TestChangeKindString(t *testing.T) {
	cases := map[workspace.ChangeKind]string{
		workspace.Added:          "added",
		workspace.Removed:        "removed",
		workspace.Modified:       "modified",
		workspace.ModeChanged:    "mode",
		workspace.TypeChanged:    "type",
		workspace.ChangeKind(99): "unknown",
	}
	for k, want := range cases {
		if got := k.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", k, got, want)
		}
	}
}
