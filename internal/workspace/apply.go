package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"connectrpc.com/connect"

	"github.com/localai-org/darkaifactory/internal/cas"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/localai-org/darkaifactory/internal/oops"
	"github.com/localai-org/darkaifactory/internal/trace"
)

// ApplyOptions tunes how a manifest is materialised on disk.
type ApplyOptions struct {
	// PreserveModTime, when true, restores the recorded mtime on
	// each entry. Off by default — restored mtimes break tools
	// that drive on file freshness, and the manifest is the
	// source of truth either way.
	PreserveModTime bool
}

// ApplyManifest reconstructs the tree described by m at dest, pulling
// any missing chunks from client and storing them in store. The dest
// directory must already exist (caller's choice whether to MkdirAll
// first); files are created/replaced atomically (.tmp + Rename) so a
// crash mid-apply never leaves a partial file at a final path.
//
// ApplyManifest does NOT delete entries in dest that aren't in the
// manifest — it's an additive merge. Callers wanting "make dest look
// exactly like m" should clear dest themselves first.
func ApplyManifest(
	ctx context.Context,
	client workspacev1connect.WorkspaceServiceClient,
	store *cas.Store,
	m *workspacev1.Manifest,
	dest string,
	opts ApplyOptions,
) error {
	if client == nil {
		return errors.New("workspace: nil client")
	}
	if store == nil {
		return errors.New("workspace: nil cas store")
	}
	if m == nil {
		return errors.New("workspace: nil manifest")
	}
	if dest == "" {
		return errors.New("workspace: empty dest")
	}
	ctx, span := trace.Start(ctx, "workspace.ApplyManifest",
		trace.WithAttributes(
			trace.KV{Key: "dest", Value: dest},
			trace.KV{Key: "entries", Value: strconv.Itoa(len(m.GetEntries()))},
		))
	defer span.End()
	// lastDir caches the parent directory for the most recent file
	// entry: BuildManifest sorts entries lex, so files in the same
	// directory arrive consecutively. Skipping the redundant
	// MkdirAll calls is significant on large flat trees.
	state := &applyState{}
	for _, e := range m.GetEntries() {
		if !SanitisePath(e.GetPath()) {
			return trace.RecordError(ctx,
				fmt.Errorf("workspace: rejected entry path %q", e.GetPath()))
		}
		if err := applyEntry(ctx, client, store, dest, e, opts, state); err != nil {
			return trace.RecordError(ctx, err)
		}
	}
	return nil
}

// applyState carries per-apply scratch (e.g. the last MkdirAll'd
// directory) so we don't re-issue duplicate filesystem ops.
type applyState struct {
	lastDir string
}

func applyEntry(
	ctx context.Context,
	client workspacev1connect.WorkspaceServiceClient,
	store *cas.Store,
	dest string,
	e *workspacev1.FileEntry,
	opts ApplyOptions,
	state *applyState,
) error {
	target := filepath.Join(dest, filepath.FromSlash(e.GetPath()))
	switch e.GetKind() {
	case workspacev1.EntryKind_ENTRY_KIND_DIR:
		if err := os.MkdirAll(target, applyMode(e)); err != nil {
			return fmt.Errorf("workspace: mkdir %s: %w", e.GetPath(), err)
		}
		state.lastDir = target
	case workspacev1.EntryKind_ENTRY_KIND_FILE:
		if err := writeFileEntry(ctx, client, store, target, e, state); err != nil {
			return err
		}
	case workspacev1.EntryKind_ENTRY_KIND_SYMLINK:
		// Replace any existing entry at target. Symlinks are
		// idempotent — recreating one with the same target is a
		// no-op result-wise but always succeeds.
		oops.Skip("workspace: remove existing symlink target",
			os.Remove(target), os.ErrNotExist)
		if err := os.Symlink(e.GetSymlinkTarget(), target); err != nil {
			return fmt.Errorf("workspace: symlink %s: %w", e.GetPath(), err)
		}
	default:
		return fmt.Errorf("workspace: unknown entry kind %v at %q",
			e.GetKind(), e.GetPath())
	}
	if opts.PreserveModTime && e.GetMtimeUnixNano() != 0 {
		// Best effort: setting times on a symlink target is OS-
		// dependent; ignore ErrNotExist for chase-the-symlink
		// inconsistencies.
		oops.Skip("workspace: chtimes",
			restoreMtime(target, e.GetMtimeUnixNano()),
			os.ErrNotExist)
	}
	return nil
}

// writeFileEntry materialises a single regular-file entry: ensures
// every chunk is in the local CAS (fetching missing ones from the
// server), then concatenates them into a temp file and renames
// into place. Chunk-by-chunk fetch + copy logic is split into
// streamChunks so this function reads top-to-bottom.
func writeFileEntry(
	ctx context.Context,
	client workspacev1connect.WorkspaceServiceClient,
	store *cas.Store,
	target string,
	e *workspacev1.FileEntry,
	state *applyState,
) error {
	parent := filepath.Dir(target)
	if parent != state.lastDir {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return fmt.Errorf("workspace: mkdir parent of %s: %w", e.GetPath(), err)
		}
		state.lastDir = parent
	}
	if err := ensureChunksLocal(ctx, client, store, e.GetChunks()); err != nil {
		return err
	}
	// #nosec G304 — target is built from a SanitisePath'd entry
	// path under the caller-supplied dest.
	tmp, err := os.CreateTemp(filepath.Dir(target), ".daft-tmp-*")
	if err != nil {
		return fmt.Errorf("workspace: create tmp for %s: %w", e.GetPath(), err)
	}
	tmpPath := tmp.Name()
	defer func() {
		oops.Skip("workspace: remove leftover tmp",
			os.Remove(tmpPath), os.ErrNotExist)
	}()
	if err := streamChunks(tmp, store, e.GetChunks()); err != nil {
		oops.Skip("workspace: close tmp on stream error",
			tmp.Close(), os.ErrClosed)
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("workspace: close tmp for %s: %w", e.GetPath(), err)
	}
	if err := os.Chmod(tmpPath, applyMode(e)); err != nil {
		return fmt.Errorf("workspace: chmod tmp for %s: %w", e.GetPath(), err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("workspace: rename %s: %w", e.GetPath(), err)
	}
	return nil
}

// ensureChunksLocal fetches any chunks missing from the local CAS.
func ensureChunksLocal(
	ctx context.Context,
	client workspacev1connect.WorkspaceServiceClient,
	store *cas.Store,
	chunks []*workspacev1.ChunkRef,
) error {
	for _, c := range chunks {
		have, err := store.Has(c.GetDigest())
		if err != nil {
			return err
		}
		if have {
			continue
		}
		if err := fetchChunk(ctx, client, store, c.GetDigest()); err != nil {
			return fmt.Errorf("workspace: fetch %s: %w", c.GetDigest(), err)
		}
	}
	return nil
}

// streamChunks concatenates each chunk's verified-decompressed
// payload into dst, in order. All chunk-side errors are wrapped
// with the digest for diagnostics.
func streamChunks(dst io.Writer, store *cas.Store, chunks []*workspacev1.ChunkRef) error {
	for _, c := range chunks {
		if err := streamOne(dst, store, c.GetDigest()); err != nil {
			return fmt.Errorf("workspace: chunk %s: %w", c.GetDigest(), err)
		}
	}
	return nil
}

func streamOne(dst io.Writer, store *cas.Store, digest string) error {
	rc, err := store.Get(digest)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(dst, rc)
	closeErr := rc.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// fetchChunk pulls a single chunk's compressed bytes from the server
// via Connect-RPC, verifies + commits via PutCompressed.
func fetchChunk(
	ctx context.Context,
	client workspacev1connect.WorkspaceServiceClient,
	store *cas.Store,
	digest string,
) error {
	resp, err := client.GetChunk(ctx, connect.NewRequest(&workspacev1.GetChunkRequest{
		Digest: digest,
	}))
	if err != nil {
		return err
	}
	return store.PutCompressed(digest, resp.Msg.GetCompressed())
}

// applyMode returns a 0700-floored mode for dirs (we always need to
// be able to traverse) or the recorded perm for everything else,
// falling back to 0o600 when the manifest is missing perms.
func applyMode(e *workspacev1.FileEntry) os.FileMode {
	mode := os.FileMode(e.GetMode())
	if e.GetKind() == workspacev1.EntryKind_ENTRY_KIND_DIR {
		// Without owner +rwx the rest of the apply pass would
		// fail walking the dir; ensure 0700 minimum.
		return mode | 0o700
	}
	if mode == 0 {
		return 0o600
	}
	return mode
}

// restoreMtime sets both atime and mtime from a Unix-nanos
// timestamp; the manifest doesn't record atime separately.
func restoreMtime(path string, unixNano int64) error {
	t := time.Unix(0, unixNano)
	return os.Chtimes(path, t, t)
}
