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
)

const (
	// DefaultDiskBytes is 20% of the default 100 GiB node CAS limit.
	DefaultDiskBytes = 20 << 30
	diskSuffix       = ".qcow2"
	migrationMarker  = ".legacy-mounts-removed-v1"
)

var ErrSizeConflict = errors.New("workspacedisk: retained disk has a different virtual size")

// Store atomically creates and reopens retained workspace images.
type Store struct {
	root       string
	create     func(string, string) (temporaryFile, error)
	link       func(string, string) error
	remove     func(string) error
	syncRoot   func(string) error
	stat       func(string) (fs.FileInfo, error)
	removeAll  func(string) error
	openMarker func(string, int, fs.FileMode) (syncCloser, error)
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
		stat: os.Stat, removeAll: os.RemoveAll,
		openMarker: func(name string, flag int, perm fs.FileMode) (syncCloser, error) {
			return os.OpenFile(name, flag, perm) // #nosec G304 -- name is beneath the validated root.
		},
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
	if binary.BigEndian.Uint32(header[0:]) != qcowMagic || binary.BigEndian.Uint32(header[4:]) != qcowVersion {
		return 0, errors.New("workspacedisk: unsupported or corrupt qcow2 header")
	}
	if binary.BigEndian.Uint32(header[20:]) != clusterBits || binary.BigEndian.Uint32(header[100:]) < headerLength {
		return 0, errors.New("workspacedisk: unsupported qcow2 geometry")
	}
	size := binary.BigEndian.Uint64(header[24:])
	if size == 0 || size%sectorSize != 0 {
		return 0, errors.New("workspacedisk: invalid qcow2 virtual size")
	}
	return size, nil
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
