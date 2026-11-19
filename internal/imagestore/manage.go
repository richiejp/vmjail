package imagestore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/richiejp/vmjail/internal/oops"
)

// ErrLocked is returned by Remove when the image's bundle is held by
// a running VM. Caller can stop the VM (releasing the lock) and retry.
var ErrLocked = errors.New("imagestore: image is locked by a running VM")

// List returns every known image, with Locked populated by checking
// the lockfiles directory. Sorted by LastUsed descending (most recent
// first) so `vmjail images ls` shows the freshest entries at the top.
func (s *Store) List() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	meta, err := s.loadMeta()
	if err != nil {
		return nil, err
	}
	entries := s.applyLocks(meta.Entries)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].LastUsed.After(entries[j].LastUsed)
	})
	return entries, nil
}

// Remove deletes an image and its bundle. Returns ErrLocked if a VM
// currently holds a lock on the bundle. Idempotent — removing an
// unknown digest is not an error.
func (s *Store) Remove(digest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.isLocked(digest) {
		return fmt.Errorf("%w: %s", ErrLocked, digest)
	}

	meta, err := s.loadMeta()
	if err != nil {
		return err
	}
	if !hasDigest(meta.Entries, digest) {
		return nil
	}
	if err := s.removeOnDisk(digest); err != nil {
		return err
	}
	meta.Entries = removeEntry(meta.Entries, digest)
	return s.saveMeta(meta)
}

// Prune removes images whose LastUsed is older than `olderThan`.
// Locked entries are skipped. Returns the digests it removed so the
// caller can log them. Pass olderThan = 0 to no-op.
func (s *Store) Prune(olderThan time.Duration) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if olderThan <= 0 {
		return nil, nil
	}
	meta, err := s.loadMeta()
	if err != nil {
		return nil, err
	}
	stale := FilterOlderThan(s.applyLocks(meta.Entries), olderThan, s.nowFn())
	for _, d := range stale {
		if err := s.removeOnDisk(d); err != nil {
			return nil, fmt.Errorf("imagestore: prune %s: %w", d, err)
		}
		meta.Entries = removeEntry(meta.Entries, d)
	}
	if err := s.saveMeta(meta); err != nil {
		return nil, err
	}
	return stale, nil
}

// Lock marks digest as in-use. Returns an unlock function the caller
// MUST invoke when the VM exits — eviction respects the lockfile and
// will refuse to delete a locked bundle. Idempotent: locking the same
// digest twice from one process leaves one lockfile.
func (s *Store) Lock(digest string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.lockPath(digest)
	// #nosec G301 — locks/ traversal must be open to the per-VM
	// unprivileged users that the agent will eventually drop into.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("imagestore: mkdir locks: %w", err)
	}
	// #nosec G304 — `path` is built from s.lockPath(digest); the
	// digest comes from our own meta.json, never user input.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("imagestore: create lock: %w", err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		// Roll back: close the half-written file and remove it.
		// Close after a write error often returns the same error;
		// Remove of a file we just created shouldn't fail. Log
		// either via oops so we don't lose unexpected failures.
		oops.Skip("imagestore: close partial lockfile", f.Close(),
			os.ErrClosed)
		oops.Skip("imagestore: remove partial lockfile",
			os.Remove(path), fs.ErrNotExist)
		return nil, fmt.Errorf("imagestore: write lock pid: %w", err)
	}
	if err := f.Close(); err != nil {
		oops.Skip("imagestore: remove unclosed lockfile",
			os.Remove(path), fs.ErrNotExist)
		return nil, fmt.Errorf("imagestore: close lock: %w", err)
	}
	unlock := func() {
		// The lockfile is per-process and per-digest. A successful
		// unlock removes it; ENOENT is fine (another agent cleared
		// it, or we crashed between Lock and unlock). Anything
		// else — permission, EROFS, EIO — is a real problem.
		oops.Skip("imagestore: unlock", os.Remove(path), fs.ErrNotExist)
	}
	return unlock, nil
}

// Usage reports the total bytes currently accounted to the layer
// cache. Computed from meta.Entries — does not stat the layout
// directly, so it reflects what eviction operates on.
func (s *Store) Usage() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	meta, err := s.loadMeta()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range meta.Entries {
		total += e.Size
	}
	return total, nil
}

func (s *Store) isLocked(digest string) bool {
	_, err := os.Stat(s.lockPath(digest))
	return err == nil
}

func (s *Store) lockPath(digest string) string {
	return filepath.Join(s.Root, locksDir, digestPathFragment(digest))
}
