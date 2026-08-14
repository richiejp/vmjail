package workspacedisk

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// DefaultDiskBytes is 20% of the default 100 GiB node CAS limit.
	DefaultDiskBytes = 20 << 30
	diskSuffix       = ".qcow2"
	migrationMarker  = ".legacy-mounts-removed-v1"
)

var (
	ErrSizeConflict        = errors.New("workspacedisk: retained disk has a different virtual size")
	ErrReservationExceeded = errors.New("workspacedisk: durable storage reservation exceeded")
)

// Store atomically creates and reopens retained workspace images.
type Store struct {
	mu               sync.Mutex
	root             string
	reservationBytes uint64
	create           func(string, string) (temporaryFile, error)
	link             func(string, string) error
	remove           func(string) error
	syncRoot         func(string) error
	stat             func(string) (fs.FileInfo, error)
	readDir          func(string) ([]os.DirEntry, error)
	removeAll        func(string) error
	openMarker       func(string, int, fs.FileMode) (syncCloser, error)
	statfs           func(string, *syscall.Statfs_t) error
}

type temporaryFile interface {
	io.WriterAt
	Name() string
	Sync() error
	Close() error
}

type syncCloser interface {
	Sync() error
	Close() error
}

// Reservation describes one retained workspace disk. VirtualBytes is the
// admission reservation; AllocatedBytes is the host blocks currently consumed
// by the sparse qcow2 file. The store deliberately does not infer whether the
// data is safe to delete.
type Reservation struct {
	WorkspaceID    string
	VirtualBytes   uint64
	AllocatedBytes uint64
	ModifiedAt     time.Time
}

// ReservationStatus is an authoritative, coherent snapshot of the durable
// workspace-disk pool.
type ReservationStatus struct {
	CapacityBytes  uint64
	ReservedBytes  uint64
	AvailableBytes uint64
	Reservations   []Reservation
}

// OpenStore prepares root and performs the one-way legacy writable-virtiofs
// migration. legacyMountRoot is deleted without import exactly once; pass an
// empty string only when the caller has no legacy layout.
func OpenStore(root, legacyMountRoot string) (*Store, error) {
	return openStore(root, legacyMountRoot, newStore())
}

func newStore() *Store {
	return &Store{
		create: func(dir, pattern string) (temporaryFile, error) {
			return os.CreateTemp(dir, pattern)
		},
		link: os.Link, remove: os.Remove, syncRoot: syncDir,
		stat: os.Stat, readDir: os.ReadDir, removeAll: os.RemoveAll,
		openMarker: func(name string, flag int, perm fs.FileMode) (syncCloser, error) {
			return os.OpenFile(name, flag, perm) // #nosec G304 -- name is beneath the validated root.
		},
		statfs: syscall.Statfs,
	}
}

func openStore(root, legacyMountRoot string, store *Store) (*Store, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("workspacedisk: root %q must be absolute", root)
	}
	if legacyMountRoot != "" && !filepath.IsAbs(legacyMountRoot) {
		return nil, fmt.Errorf("workspacedisk: legacy mount root %q must be absolute", legacyMountRoot)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("workspacedisk: create root: %w", err)
	}
	store.root = root
	if err := store.migrateLegacyMounts(legacyMountRoot); err != nil {
		return nil, err
	}
	reserved, err := store.reservedBytes()
	if err != nil {
		return nil, err
	}
	var stat syscall.Statfs_t
	if err := store.statfs(root, &stat); err != nil {
		return nil, fmt.Errorf("workspacedisk: inspect durable storage: %w", err)
	}
	available, err := filesystemAvailable(stat)
	if err != nil {
		return nil, err
	}
	if ^uint64(0)-reserved < available {
		store.reservationBytes = ^uint64(0)
	} else {
		store.reservationBytes = reserved + available
	}
	return store, nil
}

// Prepare returns the retained image for workspaceID. A first call creates it
// atomically; later calls require the exact same virtual size so an unseen
// client configuration change cannot implicitly resize or replace data.
func (s *Store) Prepare(workspaceID string, virtualSize uint64) (path string, created bool, err error) {
	if s == nil || s.root == "" {
		return "", false, errors.New("workspacedisk: nil store")
	}
	if err := validateWorkspaceID(workspaceID); err != nil {
		return "", false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path = filepath.Join(s.root, workspaceID+diskSuffix)
	currentSize, err := VirtualSize(path)
	if err == nil {
		if currentSize != virtualSize {
			return "", false, fmt.Errorf("%w: retained=%d requested=%d", ErrSizeConflict, currentSize, virtualSize)
		}
		return path, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}
	if err := s.checkReservation(virtualSize); err != nil {
		return "", false, fmt.Errorf("%w; inspect retained reservations with %q", err,
			"daft workspace storage status --uuid "+workspaceID)
	}

	tmp, err := s.create(s.root, ".create-*.qcow2")
	if err != nil {
		return "", false, fmt.Errorf("workspacedisk: create temporary image: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if removeErr := s.remove(tmpPath); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) && err == nil {
			err = fmt.Errorf("workspacedisk: remove temporary image: %w", removeErr)
		}
	}()
	if err = CreateEmptyQCOW2(tmp, virtualSize); err != nil {
		_ = tmp.Close()
		return "", false, err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", false, fmt.Errorf("workspacedisk: sync temporary image: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return "", false, fmt.Errorf("workspacedisk: close temporary image: %w", err)
	}
	if err = s.link(tmpPath, path); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return "", false, fmt.Errorf("workspacedisk: publish image: %w", err)
		}
		currentSize, err = VirtualSize(path)
		if err != nil {
			return "", false, err
		}
		if currentSize != virtualSize {
			return "", false, fmt.Errorf("%w: retained=%d requested=%d", ErrSizeConflict, currentSize, virtualSize)
		}
		return path, false, nil
	}
	if syncErr := s.syncRoot(s.root); syncErr != nil {
		return "", false, syncErr
	}
	return path, true, nil
}

// Delete removes one retained workspace image and durably syncs the store
// directory. It is idempotent so catalog deletion retries converge safely.
func (s *Store) Delete(workspaceID string) error {
	if s == nil || s.root == "" {
		return errors.New("workspacedisk: nil store")
	}
	if err := validateWorkspaceID(workspaceID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.root, workspaceID+diskSuffix)
	if err := s.remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("workspacedisk: delete image: %w", err)
	}
	if err := s.syncRoot(s.root); err != nil {
		return fmt.Errorf("workspacedisk: sync deleted image: %w", err)
	}
	return nil
}

// Size reports the retained image's virtual allowance. Missing images are a
// normal `(0, false, nil)` result for never-launched or already-deleted workspaces.
func (s *Store) Size(workspaceID string) (size uint64, exists bool, err error) {
	if s == nil || s.root == "" {
		return 0, false, errors.New("workspacedisk: nil store")
	}
	if err := validateWorkspaceID(workspaceID); err != nil {
		return 0, false, err
	}
	size, err = VirtualSize(filepath.Join(s.root, workspaceID+diskSuffix))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return size, true, nil
}

// VirtualSize reads and validates the stable qcow2 header fields needed to
// safely identify a retained image. Cloud Hypervisor may relocate later
// metadata during live growth, so this deliberately does not assume the empty
// image's initial four-cluster layout.
func VirtualSize(path string) (uint64, error) {
	file, err := os.Open(path) // #nosec G304 -- caller supplies a node-owned path.
	if err != nil {
		return 0, fmt.Errorf("workspacedisk: open image: %w", err)
	}
	var header [headerLength]byte
	_, readErr := io.ReadFull(file, header[:])
	closeErr := file.Close()
	if readErr != nil {
		return 0, fmt.Errorf("workspacedisk: read image header: %w", readErr)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("workspacedisk: close image: %w", closeErr)
	}
	if err := validateHeader(header[:]); err != nil {
		return 0, err
	}
	size := binary.BigEndian.Uint64(header[24:])
	if size == 0 || size%sectorSize != 0 {
		return 0, errors.New("workspacedisk: invalid qcow2 virtual size")
	}
	return size, nil
}

// Grow increases a retained image's virtual allowance and durably publishes
// the new qcow2 geometry. Shrinks are rejected because they could discard
// protected workspace data.
func (s *Store) Grow(workspaceID string, virtualSize uint64) error {
	if s == nil || s.root == "" {
		return errors.New("workspacedisk: nil store")
	}
	if err := validateWorkspaceID(workspaceID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.root, workspaceID+diskSuffix)
	current, err := VirtualSize(path)
	if err != nil {
		return err
	}
	if virtualSize > current {
		if err := s.checkReservation(virtualSize - current); err != nil {
			return fmt.Errorf("%w; inspect retained reservations with %q", err,
				"daft workspace storage status --uuid "+workspaceID)
		}
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- path is beneath the validated store root.
	if err != nil {
		return fmt.Errorf("workspacedisk: open image for growth: %w", err)
	}
	growErr := GrowQCOW2(file, virtualSize)
	closeErr := file.Close()
	if growErr != nil {
		return growErr
	}
	if closeErr != nil {
		return fmt.Errorf("workspacedisk: close grown image: %w", closeErr)
	}
	return nil
}

// UnderPressure reports when the filesystem holding retained disks has less
// than ten percent of the node's startup durable reservation still available.
func (s *Store) UnderPressure() bool {
	if s == nil || s.root == "" || s.reservationBytes == 0 {
		return false
	}
	var stat syscall.Statfs_t
	if err := s.statfs(s.root, &stat); err != nil {
		return true
	}
	available, err := filesystemAvailable(stat)
	if err != nil {
		return true
	}
	return available < s.reservationBytes/10
}

// Reservations reports all retained images without assigning lifecycle or
// recoverability semantics that only the orchestrator catalog can know.
func (s *Store) Reservations() (ReservationStatus, error) {
	if s == nil || s.root == "" {
		return ReservationStatus{}, errors.New("workspacedisk: nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.readDir(s.root)
	if err != nil {
		return ReservationStatus{}, fmt.Errorf("workspacedisk: list retained images: %w", err)
	}
	status := ReservationStatus{CapacityBytes: s.reservationBytes}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), diskSuffix) {
			continue
		}
		path := filepath.Join(s.root, entry.Name())
		virtual, sizeErr := VirtualSize(path)
		if sizeErr != nil {
			return ReservationStatus{}, sizeErr
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return ReservationStatus{}, fmt.Errorf("workspacedisk: stat retained image: %w", statErr)
		}
		var allocated uint64
		if native, ok := info.Sys().(*syscall.Stat_t); ok && native.Blocks > 0 {
			allocated = uint64(native.Blocks) * 512 // #nosec G115 -- positive filesystem block count.
		}
		status.Reservations = append(status.Reservations, Reservation{
			WorkspaceID: strings.TrimSuffix(entry.Name(), diskSuffix), VirtualBytes: virtual,
			AllocatedBytes: allocated, ModifiedAt: info.ModTime(),
		})
		if ^uint64(0)-status.ReservedBytes < virtual {
			return ReservationStatus{}, fmt.Errorf("%w: retained sizes overflow", ErrReservationExceeded)
		}
		status.ReservedBytes += virtual
	}
	var stat syscall.Statfs_t
	if err := s.statfs(s.root, &stat); err != nil {
		return ReservationStatus{}, fmt.Errorf("workspacedisk: inspect durable storage: %w", err)
	}
	status.AvailableBytes, err = filesystemAvailable(stat)
	if err != nil {
		return ReservationStatus{}, err
	}
	return status, nil
}

func filesystemAvailable(stat syscall.Statfs_t) (uint64, error) {
	if stat.Bsize <= 0 {
		return 0, errors.New("workspacedisk: invalid durable filesystem block size")
	}
	blockSize := uint64(stat.Bsize) // #nosec G115 -- positivity is checked above.
	if stat.Bavail > ^uint64(0)/blockSize {
		return 0, errors.New("workspacedisk: durable filesystem capacity overflow")
	}
	return stat.Bavail * blockSize, nil
}

func (s *Store) checkReservation(additional uint64) error {
	reserved, err := s.reservedBytes()
	if err != nil {
		return err
	}
	if additional > s.reservationBytes || reserved > s.reservationBytes-additional {
		return fmt.Errorf("%w: reserved=%d requested=%d capacity=%d", ErrReservationExceeded, reserved, additional, s.reservationBytes)
	}
	return nil
}

func (s *Store) reservedBytes() (uint64, error) {
	entries, err := s.readDir(s.root)
	if err != nil {
		return 0, fmt.Errorf("workspacedisk: list retained images: %w", err)
	}
	var total uint64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), diskSuffix) {
			continue
		}
		size, err := VirtualSize(filepath.Join(s.root, entry.Name()))
		if err != nil {
			return 0, err
		}
		if ^uint64(0)-total < size {
			return 0, fmt.Errorf("%w: retained sizes overflow", ErrReservationExceeded)
		}
		total += size
	}
	return total, nil
}

func (s *Store) migrateLegacyMounts(legacyRoot string) error {
	marker := filepath.Join(s.root, migrationMarker)
	if _, err := s.stat(marker); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("workspacedisk: inspect migration marker: %w", err)
	}
	if legacyRoot != "" {
		if err := s.removeAll(legacyRoot); err != nil {
			return fmt.Errorf("workspacedisk: remove legacy mounts: %w", err)
		}
	}
	// #nosec G304 -- marker is a constant basename beneath the validated root.
	file, err := s.openMarker(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("workspacedisk: create migration marker: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("workspacedisk: sync migration marker: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("workspacedisk: close migration marker: %w", err)
	}
	return syncDir(s.root)
}

func validateWorkspaceID(id string) error {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
		return fmt.Errorf("workspacedisk: unsafe workspace id %q", id)
	}
	return nil
}

func syncDir(path string) error {
	dir, err := os.Open(path) // #nosec G304 -- path is the validated store root.
	if err != nil {
		return fmt.Errorf("workspacedisk: open root for sync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return fmt.Errorf("workspacedisk: sync root: %w", err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("workspacedisk: close root: %w", err)
	}
	return nil
}
