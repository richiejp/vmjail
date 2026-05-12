package workspace

import (
	"sort"

	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
)

// ChangeKind classifies one entry-level difference between two
// manifests. A single entry can produce at most one Change so the
// kinds are mutually exclusive (a file that switched from regular
// to symlink reports Type, not Modified).
type ChangeKind int

const (
	// Added means the entry exists in the "to" manifest only.
	Added ChangeKind = iota + 1
	// Removed means the entry exists in the "from" manifest only.
	Removed
	// Modified means a regular file's chunk list (and thus
	// content) differs between the two manifests. Mode and
	// mtime alone do not trigger Modified — see ModeChanged.
	Modified
	// ModeChanged means the entry kind is unchanged and the
	// chunk list (if any) is identical, but Unix perm bits
	// differ.
	ModeChanged
	// TypeChanged means the two entries share a path but a
	// different EntryKind (file <-> symlink, dir <-> file, etc.).
	// Always a destructive change on apply.
	TypeChanged
)

func (c ChangeKind) String() string {
	switch c {
	case Added:
		return "added"
	case Removed:
		return "removed"
	case Modified:
		return "modified"
	case ModeChanged:
		return "mode"
	case TypeChanged:
		return "type"
	default:
		return "unknown"
	}
}

// Change is one entry-level difference. From / To carry the
// before/after FileEntry pointers; one is nil for Added / Removed.
type Change struct {
	Path string
	Kind ChangeKind
	From *workspacev1.FileEntry // nil when Kind == Added
	To   *workspacev1.FileEntry // nil when Kind == Removed
}

// Diff is the full result of DiffManifests. Changes is sorted by
// path so identical inputs always produce byte-identical output
// (useful for snapshot tests and idempotent reporting).
type Diff struct {
	Changes []Change
}

// Empty reports whether the two manifests are equivalent.
func (d Diff) Empty() bool { return len(d.Changes) == 0 }

// Counts breaks down the diff by kind.
func (d Diff) Counts() (added, removed, modified, modeChanged, typeChanged int) {
	for _, c := range d.Changes {
		switch c.Kind {
		case Added:
			added++
		case Removed:
			removed++
		case Modified:
			modified++
		case ModeChanged:
			modeChanged++
		case TypeChanged:
			typeChanged++
		}
	}
	return
}

// DiffManifests compares two manifests entry-by-entry and returns
// the set of changes needed to turn `from` into `to`. Nil manifests
// are treated as empty.
//
// Comparison ignores mtime — it's best-effort metadata, not
// content. Two files with the same chunks at the same offsets and
// the same mode are equal regardless of when they were last
// touched.
func DiffManifests(from, to *workspacev1.Manifest) Diff {
	fromMap := indexEntries(from)
	toMap := indexEntries(to)
	changes := []Change{}
	for path, f := range fromMap {
		t, present := toMap[path]
		if !present {
			changes = append(changes, Change{Path: path, Kind: Removed, From: f})
			continue
		}
		if k, differ := classify(f, t); differ {
			changes = append(changes, Change{Path: path, Kind: k, From: f, To: t})
		}
	}
	for path, t := range toMap {
		if _, present := fromMap[path]; !present {
			changes = append(changes, Change{Path: path, Kind: Added, To: t})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return Diff{Changes: changes}
}

func indexEntries(m *workspacev1.Manifest) map[string]*workspacev1.FileEntry {
	out := map[string]*workspacev1.FileEntry{}
	if m == nil {
		return out
	}
	for _, e := range m.GetEntries() {
		out[e.GetPath()] = e
	}
	return out
}

// classify returns the strongest non-zero change between two entries
// at the same path, or (0, false) when they're indistinguishable
// modulo mtime.
func classify(from, to *workspacev1.FileEntry) (ChangeKind, bool) {
	if from.GetKind() != to.GetKind() {
		return TypeChanged, true
	}
	switch from.GetKind() {
	case workspacev1.EntryKind_ENTRY_KIND_FILE:
		if !sameChunks(from.GetChunks(), to.GetChunks()) || from.GetSize() != to.GetSize() {
			return Modified, true
		}
	case workspacev1.EntryKind_ENTRY_KIND_SYMLINK:
		if from.GetSymlinkTarget() != to.GetSymlinkTarget() {
			return Modified, true
		}
	}
	if from.GetMode() != to.GetMode() {
		return ModeChanged, true
	}
	return 0, false
}

func sameChunks(a, b []*workspacev1.ChunkRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].GetDigest() != b[i].GetDigest() || a[i].GetSize() != b[i].GetSize() {
			return false
		}
	}
	return true
}
