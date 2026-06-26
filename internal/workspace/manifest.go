package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/richiejp/darkaifactory/internal/cas"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/oops"
)

// ManifestVersion is the on-the-wire format version this code
// emits + understands. Bump on any incompatible field-semantics
// change.
const ManifestVersion uint32 = 1

// ErrUnsupportedFileType is returned when the manifest builder
// encounters a socket / FIFO / device node — those have no useful
// representation in a content-addressed snapshot. The error wraps
// the path for diagnostics.
var ErrUnsupportedFileType = errors.New("workspace: unsupported file type")

// BuildOptions tunes manifest construction.
type BuildOptions struct {
	// Chunker overrides the chunker used for regular files. If
	// nil, &FastCDCChunker{} is used — content-defined chunking
	// so mid-stream edits only invalidate nearby chunks.
	Chunker Chunker
	// SkipPath returns true to omit a path from the manifest.
	// Called once per dirent, including dirs (skipping a dir
	// skips its whole subtree). If nil, nothing is skipped.
	SkipPath func(relPath string) bool
}

// BuildManifest walks root and produces a Manifest whose entries
// are content-addressed in store. Symlinks are recorded by their
// target (not followed). Directories are recorded so that empty
// dirs round-trip. Hard links are not deduplicated — each link is
// chunked independently; we accept the redundancy in exchange for
// not having to track inode numbers across a sync boundary.
func BuildManifest(root string, store *cas.Store, opts BuildOptions) (*workspacev1.Manifest, error) {
	if root == "" {
		return nil, errors.New("workspace: empty root")
	}
	if store == nil {
		return nil, errors.New("workspace: nil cas store")
	}
	chunker := opts.Chunker
	if chunker == nil {
		chunker = &FastCDCChunker{}
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: abs %s: %w", root, err)
	}

	var entries []*workspacev1.FileEntry
	walkErr := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(absRoot, path)
		if err != nil {
			return fmt.Errorf("workspace: rel %s: %w", path, err)
		}
		// The root itself contributes no entry — entries are
		// tree-relative; the manifest implicitly carries "."
		if rel == "." {
			return nil
		}
		// Always use forward slashes on the wire.
		rel = filepath.ToSlash(rel)
		if opts.SkipPath != nil && opts.SkipPath(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		entry, err := buildEntry(path, rel, d, chunker, store)
		if err != nil {
			return err
		}
		entries = append(entries, entry)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})

	return &workspacev1.Manifest{
		Version: ManifestVersion,
		Entries: entries,
	}, nil
}

// buildEntry classifies dirent and builds the proto entry.
func buildEntry(absPath, relPath string, d fs.DirEntry, chunker Chunker, store *cas.Store) (*workspacev1.FileEntry, error) {
	info, err := d.Info()
	if err != nil {
		return nil, fmt.Errorf("workspace: stat %s: %w", relPath, err)
	}

	mode := info.Mode()
	switch {
	case mode.IsRegular():
		return buildFileEntry(absPath, relPath, info, chunker, store)
	case mode.IsDir():
		return &workspacev1.FileEntry{
			Path:          relPath,
			Kind:          workspacev1.EntryKind_ENTRY_KIND_DIR,
			Mode:          uint32(mode.Perm()),
			MtimeUnixNano: info.ModTime().UnixNano(),
		}, nil
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(absPath)
		if err != nil {
			return nil, fmt.Errorf("workspace: readlink %s: %w", relPath, err)
		}
		return &workspacev1.FileEntry{
			Path:          relPath,
			Kind:          workspacev1.EntryKind_ENTRY_KIND_SYMLINK,
			Mode:          uint32(mode.Perm()),
			SymlinkTarget: target,
			MtimeUnixNano: info.ModTime().UnixNano(),
		}, nil
	default:
		return nil, fmt.Errorf("%w: %s (mode=%v)", ErrUnsupportedFileType, relPath, mode)
	}
}

// buildFileEntry chunks a regular file into the CAS and returns the
// matching FileEntry. The file is opened, streamed through the
// chunker, and closed via oops.Skip on the cleanup path so a Close
// failure surfaces.
func buildFileEntry(absPath, relPath string, info fs.FileInfo, chunker Chunker, store *cas.Store) (*workspacev1.FileEntry, error) {
	// #nosec G304 — absPath comes from filepath.WalkDir of the
	// caller-supplied root.
	f, err := os.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("workspace: open %s: %w", relPath, err)
	}
	defer func() { oops.Skip("workspace: close source", f.Close(), os.ErrClosed) }()

	refs, total, err := chunker.Chunk(f, store)
	if err != nil {
		return nil, fmt.Errorf("workspace: chunk %s: %w", relPath, err)
	}

	mode := info.Mode().Perm()
	return &workspacev1.FileEntry{
		Path:          relPath,
		Kind:          workspacev1.EntryKind_ENTRY_KIND_FILE,
		Mode:          uint32(mode),
		Chunks:        refs,
		Size:          total,
		MtimeUnixNano: info.ModTime().UnixNano(),
	}, nil
}

// SanitisePath returns true if rel is a safe forward-slash relative
// path with no leading slash, no `..` segments, and no embedded NUL.
// Used by the wire layer to validate paths before applying them on
// the receiving filesystem.
func SanitisePath(rel string) bool {
	if rel == "" {
		return false
	}
	if strings.ContainsRune(rel, 0) {
		return false
	}
	if strings.HasPrefix(rel, "/") {
		return false
	}
	for seg := range strings.SplitSeq(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
