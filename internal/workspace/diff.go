package workspace

import (
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
	// Modified means content differs: a regular file's chunk
	// list, or a symlink's target. Mode and mtime alone do not
	// trigger Modified — see ModeChanged for the mode-only case.
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

// Glyph returns the one-rune marker conventional CLIs use for this
// change kind: + added, - removed, ~ modified, m mode-only, t
// type-change. Lives next to String() so the CLI presentation and
// the kind enum can't drift.
func (c ChangeKind) Glyph() rune {
	switch c {
	case Added:
		return '+'
	case Removed:
		return '-'
	case Modified:
		return '~'
	case ModeChanged:
		return 'm'
	case TypeChanged:
		return 't'
	default:
		return '?'
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
//
// Manifests are lex-sorted by path (see workspace.proto Manifest);
// this is a two-pointer merge, O(n+m) with no auxiliary maps. The
// output preserves that sort.
func DiffManifests(from, to *workspacev1.Manifest) Diff {
	fromE := from.GetEntries()
	toE := to.GetEntries()
	var changes []Change
	i, j := 0, 0
	for i < len(fromE) && j < len(toE) {
		f, t := fromE[i], toE[j]
		switch {
		case f.GetPath() < t.GetPath():
			changes = append(changes, Change{Path: f.GetPath(), Kind: Removed, From: f})
			i++
		case f.GetPath() > t.GetPath():
			changes = append(changes, Change{Path: t.GetPath(), Kind: Added, To: t})
			j++
		default:
			if k, differ := classify(f, t); differ {
				changes = append(changes, Change{Path: f.GetPath(), Kind: k, From: f, To: t})
			}
			i++
			j++
		}
	}
	for ; i < len(fromE); i++ {
		changes = append(changes, Change{Path: fromE[i].GetPath(), Kind: Removed, From: fromE[i]})
	}
	for ; j < len(toE); j++ {
		changes = append(changes, Change{Path: toE[j].GetPath(), Kind: Added, To: toE[j]})
	}
	return Diff{Changes: changes}
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
