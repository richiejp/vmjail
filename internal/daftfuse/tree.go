package daftfuse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/workspace"
)

// node is one inode in the in-memory tree. Mirrors FileEntry but
// pre-arranges children for fast Lookup. node fields are immutable
// after buildTree returns; concurrent reads need no lock.
type node struct {
	name    string
	kind    workspacev1.EntryKind
	mode    uint32
	size    uint64
	mtime   time.Time
	target  string           // symlink target, empty otherwise
	chunks  []chunkSpan      // file chunks with cumulative offsets
	entries map[string]*node // dir children, nil for non-dirs
}

// chunkSpan is one chunk of a file with its precomputed [start,end)
// byte range so Mount.ReadAt can binary-search by offset.
type chunkSpan struct {
	digest string
	start  uint64
	end    uint64
}

// tree is the manifest as an inode graph. Root is always a dir.
type tree struct {
	root *node
}

// buildTree converts a manifest's flat sorted entry list into a
// dir-tree. Implicit ancestor directories (entries deeper than the
// nearest declared dir) are materialised with default 0o755 perms;
// declared dir entries override that.
func buildTree(m *workspacev1.Manifest) (*tree, error) {
	root := &node{
		name:    ".",
		kind:    workspacev1.EntryKind_ENTRY_KIND_DIR,
		mode:    0o755,
		entries: map[string]*node{},
	}
	for _, e := range m.GetEntries() {
		if err := insertEntry(root, e); err != nil {
			return nil, err
		}
	}
	return &tree{root: root}, nil
}

func insertEntry(root *node, e *workspacev1.FileEntry) error {
	if !workspace.SanitisePath(e.GetPath()) {
		return fmt.Errorf("daftfuse: bad entry path %q", e.GetPath())
	}
	clean := path.Clean(e.GetPath())
	parts := strings.Split(clean, "/")
	cur := root
	for i, part := range parts[:len(parts)-1] {
		child, ok := cur.entries[part]
		if !ok {
			child = &node{
				name:    part,
				kind:    workspacev1.EntryKind_ENTRY_KIND_DIR,
				mode:    0o755,
				entries: map[string]*node{},
			}
			cur.entries[part] = child
		}
		if child.kind != workspacev1.EntryKind_ENTRY_KIND_DIR {
			return fmt.Errorf("daftfuse: %q traverses non-dir %q",
				clean, strings.Join(parts[:i+1], "/"))
		}
		cur = child
	}
	leaf := parts[len(parts)-1]
	n, err := nodeFromEntry(leaf, e)
	if err != nil {
		return err
	}
	if existing, dup := cur.entries[leaf]; dup {
		if existing.kind != workspacev1.EntryKind_ENTRY_KIND_DIR ||
			n.kind != workspacev1.EntryKind_ENTRY_KIND_DIR {
			return fmt.Errorf("daftfuse: duplicate entry %q", clean)
		}
		existing.mode = n.mode
		existing.mtime = n.mtime
		return nil
	}
	cur.entries[leaf] = n
	return nil
}

func nodeFromEntry(name string, e *workspacev1.FileEntry) (*node, error) {
	n := &node{
		name:  name,
		kind:  e.GetKind(),
		mode:  e.GetMode(),
		size:  e.GetSize(),
		mtime: time.Unix(0, e.GetMtimeUnixNano()),
	}
	switch e.GetKind() {
	case workspacev1.EntryKind_ENTRY_KIND_DIR:
		n.entries = map[string]*node{}
	case workspacev1.EntryKind_ENTRY_KIND_SYMLINK:
		n.target = e.GetSymlinkTarget()
	case workspacev1.EntryKind_ENTRY_KIND_FILE:
		var off uint64
		n.chunks = make([]chunkSpan, len(e.GetChunks()))
		for i, c := range e.GetChunks() {
			n.chunks[i] = chunkSpan{
				digest: c.GetDigest(),
				start:  off,
				end:    off + c.GetSize(),
			}
			off += c.GetSize()
		}
		if off != n.size {
			return nil, fmt.Errorf(
				"daftfuse: %q chunk total %d != size %d", name, off, n.size,
			)
		}
	default:
		return nil, fmt.Errorf("daftfuse: unsupported entry kind %v for %q",
			e.GetKind(), name)
	}
	return n, nil
}

// lookup resolves a forward-slash separated path against the tree.
// "" and "." both return the root.
func (t *tree) lookup(p string) (*node, error) {
	clean := path.Clean(p)
	if clean == "." || clean == "/" || clean == "" {
		return t.root, nil
	}
	cur := t.root
	for part := range strings.SplitSeq(strings.TrimPrefix(clean, "/"), "/") {
		if cur.kind != workspacev1.EntryKind_ENTRY_KIND_DIR {
			return nil, fs.ErrNotExist
		}
		child, ok := cur.entries[part]
		if !ok {
			return nil, fs.ErrNotExist
		}
		cur = child
	}
	return cur, nil
}

// childNames returns a sorted list of a directory's children.
func (n *node) childNames() []string {
	out := make([]string, 0, len(n.entries))
	for name := range n.entries {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ReadAt copies up to len(p) bytes from the file at path starting
// at offset off. Returns (n, io.EOF) when n < len(p) and the file
// ends mid-read; (0, io.EOF) when off >= file size. Concurrent
// reads of overlapping chunks share one fetch via singleflight.
//
// Returns ErrDenied / ErrFrozen on Policy refusal, ErrNotRegular
// when path resolves to a non-file, fs.ErrNotExist when not found.
func (m *Mount) ReadAt(ctx context.Context, p string, dest []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("daftfuse: negative offset %d", off)
	}
	n, err := m.tree.lookup(p)
	if err != nil {
		return 0, err
	}
	if n.kind != workspacev1.EntryKind_ENTRY_KIND_FILE {
		return 0, ErrNotRegular
	}
	if uint64(off) >= n.size || len(dest) == 0 {
		return 0, io.EOF
	}
	remaining := n.size - uint64(off)
	want := min(uint64(len(dest)), remaining)
	written, err := m.readFileSpans(ctx, p, n, uint64(off), dest[:want])
	if err != nil {
		return written, err
	}
	if uint64(off)+uint64(written) == n.size { //nolint:gosec // off >= 0 checked above; written from copy so non-negative.
		return written, io.EOF
	}
	return written, nil
}

func (m *Mount) readFileSpans(ctx context.Context, relPath string, n *node, off uint64, dest []byte) (int, error) {
	end := off + uint64(len(dest))
	written := 0
	// chunks are sorted by span.start, non-overlapping and
	// contiguous (built that way in nodeFromEntry). Binary-search
	// for the first chunk whose end > off so a read at offset 1
	// GiB into a multi-GiB file doesn't walk 1000+ pre-offset
	// chunks linearly.
	first := sort.Search(len(n.chunks), func(i int) bool {
		return n.chunks[i].end > off
	})
	for _, span := range n.chunks[first:] {
		if span.start >= end {
			break
		}
		evStart := max(span.start, off)
		ev := &ReadEvent{
			Path:   relPath,
			Offset: evStart,
			Size:   min(span.end, end) - evStart,
			Digest: span.digest,
		}
		switch m.policy.OnRead(ev) {
		case Deny:
			return written, ErrDenied
		case Freeze:
			return written, ErrFrozen
		}
		chunkData, err := m.loadChunk(ctx, span.digest)
		if err != nil {
			return written, err
		}
		localStart := uint64(0)
		if off > span.start {
			localStart = off - span.start
		}
		localEnd := uint64(len(chunkData))
		if span.end > end {
			localEnd -= span.end - end
		}
		copied := copy(dest[written:], chunkData[localStart:localEnd])
		written += copied
	}
	return written, nil
}

// loadChunk returns the decompressed bytes for digest, fetching
// them via ChunkFetcher on a miss. Concurrent callers for the
// same digest share one fetch + decompress.
func (m *Mount) loadChunk(ctx context.Context, digest string) ([]byte, error) {
	v, err, _ := m.sf.Do(digest, func() (any, error) {
		if err := m.ensureLocal(ctx, digest); err != nil {
			return nil, err
		}
		rc, err := m.store.Get(digest)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }()
		return io.ReadAll(rc)
	})
	if err != nil {
		return nil, err
	}
	data, ok := v.([]byte)
	if !ok {
		return nil, fmt.Errorf("daftfuse: singleflight returned %T, want []byte", v)
	}
	return data, nil
}

func (m *Mount) ensureLocal(ctx context.Context, digest string) error {
	have, err := m.store.Has(digest)
	if err != nil {
		return err
	}
	if have {
		return nil
	}
	if m.fetcher == nil {
		return fmt.Errorf("daftfuse: chunk %s not local and no fetcher", digest)
	}
	return m.fetchWithRetry(ctx, digest)
}

func (m *Mount) fetchWithRetry(ctx context.Context, digest string) error {
	backoff := m.backoff
	var last error
	for attempt := 0; attempt < m.retries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			backoff *= 2
		}
		compressed, err := m.fetcher.FetchChunk(ctx, digest)
		if err != nil {
			last = err
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			continue
		}
		if err := m.store.PutCompressed(digest, compressed); err != nil {
			return fmt.Errorf("daftfuse: put %s: %w", digest, err)
		}
		return nil
	}
	return fmt.Errorf("daftfuse: fetch %s after %d attempts: %w",
		digest, m.retries, last)
}

// Stat returns metadata for path, mapping the in-memory node into
// the small subset of os.FileInfo we surface (mode, size, mtime,
// kind via Mode bits, symlink target via Sys).
type Stat struct {
	Mode    os.FileMode
	Size    uint64
	ModTime time.Time
	Kind    workspacev1.EntryKind
	Target  string // symlink target, empty otherwise
}

// Stat resolves path and returns its metadata. fs.ErrNotExist on
// missing entries.
func (m *Mount) Stat(p string) (*Stat, error) {
	n, err := m.tree.lookup(p)
	if err != nil {
		return nil, err
	}
	mode := os.FileMode(n.mode) & os.ModePerm
	switch n.kind {
	case workspacev1.EntryKind_ENTRY_KIND_DIR:
		mode |= os.ModeDir
	case workspacev1.EntryKind_ENTRY_KIND_SYMLINK:
		mode |= os.ModeSymlink
	}
	return &Stat{
		Mode:    mode,
		Size:    n.size,
		ModTime: n.mtime,
		Kind:    n.kind,
		Target:  n.target,
	}, nil
}

// Readdir returns the names of a directory's children. Errors on
// non-directory targets.
func (m *Mount) Readdir(p string) ([]string, error) {
	n, err := m.tree.lookup(p)
	if err != nil {
		return nil, err
	}
	if n.kind != workspacev1.EntryKind_ENTRY_KIND_DIR {
		return nil, fmt.Errorf("daftfuse: %q: %w", p, ErrNotRegular)
	}
	return n.childNames(), nil
}

// Readlink returns the symlink target for path. Errors when the
// path is not a symlink.
func (m *Mount) Readlink(p string) (string, error) {
	n, err := m.tree.lookup(p)
	if err != nil {
		return "", err
	}
	if n.kind != workspacev1.EntryKind_ENTRY_KIND_SYMLINK {
		return "", fmt.Errorf("daftfuse: %q: not a symlink", p)
	}
	return n.target, nil
}
