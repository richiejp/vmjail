package workspacedisk

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

const testWorkspaceID = "00000000-0000-4000-8000-000000000001"

type failingFile struct {
	*os.File
	syncErr  error
	closeErr error
}

func (f *failingFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.File.Sync()
}

func (f *failingFile) Close() error {
	if f.closeErr != nil {
		_ = f.File.Close()
		return f.closeErr
	}
	return f.File.Close()
}

type fakeMarker struct {
	syncErr  error
	closeErr error
}

func (f fakeMarker) Sync() error  { return f.syncErr }
func (f fakeMarker) Close() error { return f.closeErr }

func TestStorePrepareAndReuse(t *testing.T) {
	root := filepath.Join(t.TempDir(), "disks")
	store, err := OpenStore(root, "")
	if err != nil {
		t.Fatal(err)
	}
	path, created, err := store.Prepare(testWorkspaceID, DefaultDiskBytes)
	if err != nil || !created {
		t.Fatalf("first Prepare = %q, %v, %v", path, created, err)
	}
	if got, err := VirtualSize(path); err != nil || got != DefaultDiskBytes {
		t.Fatalf("VirtualSize = %d, %v", got, err)
	}
	again, created, err := store.Prepare(testWorkspaceID, DefaultDiskBytes)
	if err != nil || created || again != path {
		t.Fatalf("second Prepare = %q, %v, %v", again, created, err)
	}
	if _, _, err := store.Prepare(testWorkspaceID, DefaultDiskBytes*2); !errors.Is(err, ErrSizeConflict) {
		t.Fatalf("size conflict = %v, want %v", err, ErrSizeConflict)
	}
}

func TestStorePrepareConcurrentConverges(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	paths := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path, _, err := store.Prepare(testWorkspaceID, DefaultDiskBytes)
			paths <- path
			errs <- err
		}()
	}
	wg.Wait()
	close(paths)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Prepare: %v", err)
		}
	}
	want := filepath.Join(store.root, testWorkspaceID+diskSuffix)
	for path := range paths {
		if path != want {
			t.Errorf("path = %q, want %q", path, want)
		}
	}
}

func TestStoreLegacyMigrationRunsOnce(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "disks")
	legacy := filepath.Join(parent, "mounts")
	if err := os.MkdirAll(filepath.Join(legacy, "sandbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "sandbox", "untrusted"), []byte("discard"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(root, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy tree remains: %v", err)
	}
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(root, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("migration reran after durable marker: %v", err)
	}
}

func TestVirtualSizeRejectsInvalidImages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.qcow2")
	if err := os.WriteFile(path, []byte("not qcow2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VirtualSize(path); err == nil {
		t.Fatal("invalid image accepted")
	}
}

func TestVirtualSizeRejectsUnsupportedHeaders(t *testing.T) {
	tests := []struct {
		name   string
		offset int64
		value  []byte
	}{
		{name: "magic", offset: 0, value: []byte{0, 0, 0, 0}},
		{name: "cluster geometry", offset: 20, value: []byte{0, 0, 0, 15}},
		{name: "header length", offset: 100, value: []byte{0, 0, 0, 1}},
		{name: "zero size", offset: 24, value: make([]byte, 8)},
		{name: "unaligned size", offset: 24, value: []byte{0, 0, 0, 0, 0, 0, 2, 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "image.qcow2")
			// #nosec G304 -- path is beneath the test-owned temporary directory.
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := CreateEmptyQCOW2(file, DefaultDiskBytes); err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteAt(tc.value, tc.offset); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := VirtualSize(path); err == nil {
				t.Fatal("unsupported header accepted")
			}
		})
	}
}

func TestStorePrepareFailurePaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "disks")
	store, err := OpenStore(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Prepare(testWorkspaceID, 1); !errors.Is(err, ErrUnalignedSize) {
		t.Fatalf("unaligned size error = %v", err)
	}
	corruptPath := filepath.Join(root, testWorkspaceID+diskSuffix)
	if err := os.WriteFile(corruptPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Prepare(testWorkspaceID, DefaultDiskBytes); err == nil {
		t.Fatal("corrupt retained image accepted")
	}
	if err := os.Remove(corruptPath); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Prepare(testWorkspaceID, DefaultDiskBytes); err == nil {
		t.Fatal("missing store root accepted")
	}
}

func TestStorePreparePublishFailurePaths(t *testing.T) {
	tests := []struct {
		name string
		link func(string, string) error
		want error
	}{
		{
			name: "publish error",
			link: func(_, _ string) error { return errors.New("publish failed") },
		},
		{
			name: "publish race reuses matching image",
			link: func(oldPath, newPath string) error {
				if err := os.Link(oldPath, newPath); err != nil {
					return err
				}
				return fs.ErrExist
			},
		},
		{
			name: "publish race rejects conflicting image",
			link: func(oldPath, newPath string) error {
				if err := os.Link(oldPath, newPath); err != nil {
					return err
				}
				// #nosec G304 -- newPath is the store-owned test image path.
				file, err := os.OpenFile(newPath, os.O_WRONLY, 0)
				if err != nil {
					return err
				}
				var size [8]byte
				binary.BigEndian.PutUint64(size[:], DefaultDiskBytes*2)
				if _, err := file.WriteAt(size[:], 24); err != nil {
					_ = file.Close() // Preserve the write error.
					return err
				}
				if err := file.Close(); err != nil {
					return err
				}
				return fs.ErrExist
			},
			want: ErrSizeConflict,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "")
			if err != nil {
				t.Fatal(err)
			}
			store.link = tc.link
			path, created, err := store.Prepare(testWorkspaceID, DefaultDiskBytes)
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Prepare error = %v, want %v", err, tc.want)
			}
			if tc.want == nil && tc.name == "publish race reuses matching image" && (err != nil || created || path == "") {
				t.Fatalf("Prepare = %q, %v, %v", path, created, err)
			}
			if tc.name == "publish error" && err == nil {
				t.Fatal("publish error ignored")
			}
		})
	}
}

func TestStorePrepareSyncAndCleanupErrors(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	store.syncRoot = func(string) error { return errors.New("sync failed") }
	if _, _, err := store.Prepare(testWorkspaceID, DefaultDiskBytes); err == nil {
		t.Fatal("directory sync error ignored")
	}

	store, err = OpenStore(filepath.Join(t.TempDir(), "other-disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	store.remove = func(string) error { return errors.New("remove failed") }
	if _, _, err := store.Prepare(testWorkspaceID, DefaultDiskBytes); err == nil {
		t.Fatal("temporary cleanup error ignored")
	}
}

func TestStorePrepareTemporaryFileErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		syncErr  error
		closeErr error
	}{
		{name: "sync", syncErr: errors.New("sync failed")},
		{name: "close", closeErr: errors.New("close failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "")
			if err != nil {
				t.Fatal(err)
			}
			store.create = func(dir, pattern string) (temporaryFile, error) {
				file, err := os.CreateTemp(dir, pattern)
				return &failingFile{File: file, syncErr: tc.syncErr, closeErr: tc.closeErr}, err
			}
			if _, _, err := store.Prepare(testWorkspaceID, DefaultDiskBytes); err == nil {
				t.Fatalf("%s error ignored", tc.name)
			}
		})
	}

	store, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	store.create = func(string, string) (temporaryFile, error) {
		return nil, errors.New("create failed")
	}
	if _, _, err := store.Prepare(testWorkspaceID, DefaultDiskBytes); err == nil {
		t.Fatal("temporary creation error ignored")
	}
}

func TestStoreMigrationFailures(t *testing.T) {
	tests := []struct {
		name   string
		legacy bool
		mutate func(*Store)
	}{
		{
			name: "marker stat",
			mutate: func(store *Store) {
				store.stat = func(string) (fs.FileInfo, error) { return nil, errors.New("stat failed") }
			},
		},
		{
			name:   "legacy removal",
			legacy: true,
			mutate: func(store *Store) {
				store.removeAll = func(string) error { return errors.New("remove failed") }
			},
		},
		{
			name: "marker creation",
			mutate: func(store *Store) {
				store.openMarker = func(string, int, fs.FileMode) (syncCloser, error) {
					return nil, errors.New("open failed")
				}
			},
		},
		{
			name: "marker sync",
			mutate: func(store *Store) {
				store.openMarker = func(string, int, fs.FileMode) (syncCloser, error) {
					return fakeMarker{syncErr: errors.New("sync failed")}, nil
				}
			},
		},
		{
			name: "marker close",
			mutate: func(store *Store) {
				store.openMarker = func(string, int, fs.FileMode) (syncCloser, error) {
					return fakeMarker{closeErr: errors.New("close failed")}, nil
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			root := filepath.Join(parent, "disks")
			legacy := ""
			if tc.legacy {
				legacy = filepath.Join(parent, "mounts")
			}
			store := newStore()
			tc.mutate(store)
			if _, err := openStore(root, legacy, store); err == nil {
				t.Fatalf("%s error ignored", tc.name)
			}
		})
	}
}

func TestStoreRejectsUnsafeInputs(t *testing.T) {
	if _, err := OpenStore("relative", ""); err == nil {
		t.Fatal("relative root accepted")
	}
	if _, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "relative"); err == nil {
		t.Fatal("relative legacy root accepted")
	}
	blockedRoot := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blockedRoot, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(blockedRoot, ""); err == nil {
		t.Fatal("file store root accepted")
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", ".", "..", "../escape", "a/b"} {
		if runtime.GOOS == "windows" && id == "a/b" {
			continue
		}
		if _, _, err := store.Prepare(id, DefaultDiskBytes); err == nil {
			t.Errorf("unsafe id %q accepted", id)
		}
	}
	var nilStore *Store
	if _, _, err := nilStore.Prepare(testWorkspaceID, DefaultDiskBytes); err == nil {
		t.Fatal("nil store accepted")
	}
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("syncDir accepted missing directory")
	}
}

func TestStoreGrow(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	path, _, err := store.Prepare(testWorkspaceID, DefaultDiskBytes)
	if err != nil {
		t.Fatal(err)
	}
	const grown = 24 << 30
	if err := store.Grow(testWorkspaceID, grown); err != nil {
		t.Fatal(err)
	}
	if got, err := VirtualSize(path); err != nil || got != grown {
		t.Fatalf("VirtualSize = %d, %v; want %d", got, err, uint64(grown))
	}
	if err := store.Grow(testWorkspaceID, DefaultDiskBytes); !errors.Is(err, ErrShrink) {
		t.Fatalf("shrink error = %v, want %v", err, ErrShrink)
	}
}
