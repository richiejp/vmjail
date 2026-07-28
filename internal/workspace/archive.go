package workspace

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
)

// StreamMount writes the current durable manifest as a tar stream without
// materialising a host tree. The caller owns transport framing; even an
// unknown mount produces a valid empty archive so first launch can start with
// an empty XFS directory.
func (s *Server) StreamMount(ctx context.Context, key string, destination io.Writer) (bool, error) {
	if key == "" {
		return false, errors.New("workspace: empty stream key")
	}
	if destination == nil {
		return false, errors.New("workspace: nil archive destination")
	}
	s.mu.RLock()
	manifest, exists := s.manifests[key]
	s.mu.RUnlock()
	tw := tar.NewWriter(destination)
	if !exists {
		return false, tw.Close()
	}
	for _, entry := range manifest.GetEntries() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := s.streamArchiveEntry(tw, entry); err != nil {
			return false, err
		}
	}
	if err := tw.Close(); err != nil {
		return false, fmt.Errorf("workspace: close mount archive: %w", err)
	}
	return true, nil
}

func (s *Server) streamArchiveEntry(tw *tar.Writer, entry *workspacev1.FileEntry) error {
	if entry == nil || !SanitisePath(entry.GetPath()) {
		return fmt.Errorf("workspace: rejected archive entry path %q", entry.GetPath())
	}
	header := &tar.Header{
		Name: entry.GetPath(), Mode: int64(entry.GetMode()),
		ModTime: time.Unix(0, entry.GetMtimeUnixNano()),
	}
	switch entry.GetKind() {
	case workspacev1.EntryKind_ENTRY_KIND_DIR:
		header.Typeflag = tar.TypeDir
	case workspacev1.EntryKind_ENTRY_KIND_FILE:
		if entry.GetSize() > uint64(^uint64(0)>>1) {
			return fmt.Errorf("workspace: archive file %q exceeds tar size limit", entry.GetPath())
		}
		header.Typeflag = tar.TypeReg
		header.Size = int64(entry.GetSize()) // #nosec G115 -- bounded above by MaxInt64.
	case workspacev1.EntryKind_ENTRY_KIND_SYMLINK:
		header.Typeflag = tar.TypeSymlink
		header.Linkname = entry.GetSymlinkTarget()
	default:
		return fmt.Errorf("workspace: unknown archive entry kind %v at %q", entry.GetKind(), entry.GetPath())
	}
	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("workspace: write archive header %q: %w", entry.GetPath(), err)
	}
	if entry.GetKind() == workspacev1.EntryKind_ENTRY_KIND_FILE {
		if err := streamChunks(tw, s.store, entry.GetChunks()); err != nil {
			return fmt.Errorf("workspace: stream archive file %q: %w", entry.GetPath(), err)
		}
	}
	return nil
}
