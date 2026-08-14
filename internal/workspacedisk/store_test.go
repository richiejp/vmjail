package workspacedisk

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
)

const (
	testWorkspaceID = "00000000-0000-4000-8000-000000000001"
	testDiskBytes   = 64 << 20
)

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
	path, created, err := store.Prepare(testWorkspaceID, testDiskBytes)
	if err != nil || !created {
		t.Fatalf("first Prepare = %q, %v, %v", path, created, err)
	}
	if got, err := VirtualSize(path); err != nil || got != testDiskBytes {
		t.Fatalf("VirtualSize = %d, %v", got, err)
	}
	again, created, err := store.Prepare(testWorkspaceID, testDiskBytes)
	if err != nil || created || again != path {
		t.Fatalf("second Prepare = %q, %v, %v", again, created, err)
	}
	if _, _, err := store.Prepare(testWorkspaceID, testDiskBytes*2); !errors.Is(err, ErrSizeConflict) {
		t.Fatalf("size conflict = %v, want %v", err, ErrSizeConflict)
	}
}

func TestReservationFailureProvidesInspectionCommand(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	store.reservationBytes = testDiskBytes - 1
	_, _, err = store.Prepare(testWorkspaceID, testDiskBytes)
	if !errors.Is(err, ErrReservationExceeded) || !strings.Contains(err.Error(), "daft workspace storage status --uuid "+testWorkspaceID) {
		t.Fatalf("reservation diagnostic = %v", err)
	}
}

func TestStoreSizeAndDeleteAreDurableAndIdempotent(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	if size, exists, err := store.Size(testWorkspaceID); err != nil || exists || size != 0 {
		t.Fatalf("missing Size = %d, %t, %v", size, exists, err)
	}
	path, _, err := store.Prepare(testWorkspaceID, testDiskBytes)
	if err != nil {
		t.Fatal(err)
	}
	if size, exists, err := store.Size(testWorkspaceID); err != nil || !exists || size != testDiskBytes {
		t.Fatalf("retained Size = %d, %t, %v", size, exists, err)
	}
	if err := store.Delete(testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleted image stat = %v", err)
	}
	if err := store.Delete(testWorkspaceID); err != nil {
		t.Fatalf("idempotent Delete: %v", err)
	}
}

func TestStoreReservationsReportsVirtualAndAllocatedBytes(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Prepare(testWorkspaceID, testDiskBytes); err != nil {
		t.Fatal(err)
	}
	status, err := store.Reservations()
	if err != nil {
		t.Fatal(err)
	}
	if status.CapacityBytes == 0 || status.AvailableBytes == 0 {
		t.Fatalf("pool status = %#v", status)
	}
	if status.ReservedBytes != testDiskBytes || len(status.Reservations) != 1 {
		t.Fatalf("reservations = %#v", status)
	}
	got := status.Reservations[0]
	if got.WorkspaceID != testWorkspaceID || got.VirtualBytes != testDiskBytes || got.AllocatedBytes == 0 || got.ModifiedAt.IsZero() {
		t.Fatalf("reservation = %#v", got)
	}
	var nilStore *Store
	if _, err := nilStore.Reservations(); err == nil {
		t.Fatal("nil store Reservations succeeded")
	}
	store.statfs = func(string, *syscall.Statfs_t) error { return errors.New("statfs failed") }
	if _, err := store.Reservations(); err == nil || !strings.Contains(err.Error(), "statfs failed") {
		t.Fatalf("statfs error = %v", err)
	}
	store.statfs = func(string, *syscall.Statfs_t) error { return nil }
	if _, err := store.Reservations(); err == nil || !strings.Contains(err.Error(), "block size") {
		t.Fatalf("invalid filesystem geometry = %v", err)
	}
	store.readDir = func(string) ([]os.DirEntry, error) { return nil, errors.New("list failed") }
	if _, err := store.Reservations(); err == nil || !strings.Contains(err.Error(), "list failed") {
		t.Fatalf("directory listing error = %v", err)
	}
}

func TestStoreReservationsRejectsCorruptRetainedImage(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.root, testWorkspaceID+diskSuffix), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Reservations(); err == nil {
		t.Fatal("corrupt retained image accepted")
	}
}

func TestStoreSizeAndDeleteValidateAndReportDurabilityFailures(t *testing.T) {
	var nilStore *Store
	if err := nilStore.Delete(testWorkspaceID); err == nil {
		t.Fatal("nil store Delete succeeded")
	}
	if _, _, err := nilStore.Size(testWorkspaceID); err == nil {
		t.Fatal("nil store Size succeeded")
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete("../escape"); err == nil {
		t.Fatal("invalid workspace Delete succeeded")
	}
	if _, _, err := store.Size("../escape"); err == nil {
		t.Fatal("invalid workspace Size succeeded")
	}
	removeErr := errors.New("remove failed")
	store.remove = func(string) error { return removeErr }
	if err := store.Delete(testWorkspaceID); !errors.Is(err, removeErr) {
		t.Fatalf("Delete remove error = %v", err)
	}
	store.remove = func(string) error { return fs.ErrNotExist }
	syncErr := errors.New("sync failed")
	store.syncRoot = func(string) error { return syncErr }
	if err := store.Delete(testWorkspaceID); !errors.Is(err, syncErr) {
		t.Fatalf("Delete sync error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.root, testWorkspaceID+diskSuffix), []byte("not qcow2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.Size(testWorkspaceID); err == nil || exists {
		t.Fatalf("invalid image Size exists=%t err=%v", exists, err)
	}
}

func TestSyncDirReportsMissingRoot(t *testing.T) {
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("syncDir missing root = %v", err)
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
			path, _, err := store.Prepare(testWorkspaceID, testDiskBytes)
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
			if err := CreateEmptyQCOW2(file, testDiskBytes); err != nil {
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
	if _, _, err := store.Prepare(testWorkspaceID, testDiskBytes); err == nil {
		t.Fatal("corrupt retained image accepted")
	}
	if err := os.Remove(corruptPath); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Prepare(testWorkspaceID, testDiskBytes); err == nil {
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
				binary.BigEndian.PutUint64(size[:], testDiskBytes*2)
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
			path, created, err := store.Prepare(testWorkspaceID, testDiskBytes)
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
	if _, _, err := store.Prepare(testWorkspaceID, testDiskBytes); err == nil {
		t.Fatal("directory sync error ignored")
	}

	store, err = OpenStore(filepath.Join(t.TempDir(), "other-disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	store.remove = func(string) error { return errors.New("remove failed") }
	if _, _, err := store.Prepare(testWorkspaceID, testDiskBytes); err == nil {
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
			if _, _, err := store.Prepare(testWorkspaceID, testDiskBytes); err == nil {
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
	if _, _, err := store.Prepare(testWorkspaceID, testDiskBytes); err == nil {
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
		if _, _, err := store.Prepare(id, testDiskBytes); err == nil {
			t.Errorf("unsafe id %q accepted", id)
		}
	}
	var nilStore *Store
	if _, _, err := nilStore.Prepare(testWorkspaceID, testDiskBytes); err == nil {
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
	const grown = 24 << 30
	// The qcow2 is sparse; this test validates virtual resizing, not the
	// machine running the test's free-space policy. Keep reservation byte
	// accounting deterministic even when t.TempDir is a small tmpfs.
	store.reservationBytes = grown + testDiskBytes
	path, _, err := store.Prepare(testWorkspaceID, testDiskBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Grow(testWorkspaceID, grown); err != nil {
		t.Fatal(err)
	}
	if got, err := VirtualSize(path); err != nil || got != grown {
		t.Fatalf("VirtualSize = %d, %v; want %d", got, err, uint64(grown))
	}
	if err := store.Grow(testWorkspaceID, testDiskBytes); !errors.Is(err, ErrShrink) {
		t.Fatalf("shrink error = %v, want %v", err, ErrShrink)
	}
}

func TestStoreGrowRejectsInvalidAndMissingImages(t *testing.T) {
	var nilStore *Store
	if err := nilStore.Grow(testWorkspaceID, testDiskBytes*2); err == nil {
		t.Fatal("nil store Grow succeeded")
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Grow("../escape", testDiskBytes*2); err == nil {
		t.Fatal("invalid workspace Grow succeeded")
	}
	if err := store.Grow(testWorkspaceID, testDiskBytes*2); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing image Grow = %v", err)
	}
	path := filepath.Join(store.root, testWorkspaceID+diskSuffix)
	if err := os.WriteFile(path, []byte("not qcow2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Grow(testWorkspaceID, testDiskBytes*2); err == nil {
		t.Fatal("invalid image Grow succeeded")
	}
}

func TestStoreRejectsCreateBeyondDurableReservation(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	store.reservationBytes = testDiskBytes - 1
	if _, _, err := store.Prepare(testWorkspaceID, testDiskBytes); !errors.Is(err, ErrReservationExceeded) {
		t.Fatalf("Prepare error = %v, want ErrReservationExceeded", err)
	}
}

func TestStoreReservationAccountsRetainedImagesOnGrow(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Prepare(testWorkspaceID, 8<<20); err != nil {
		t.Fatal(err)
	}
	store.reservationBytes = 12 << 20
	if err := store.Grow(testWorkspaceID, 16<<20); !errors.Is(err, ErrReservationExceeded) {
		t.Fatalf("Grow error = %v, want ErrReservationExceeded", err)
	}
}

func TestFilesystemAvailableValidation(t *testing.T) {
	if got, err := filesystemAvailable(syscall.Statfs_t{Bavail: 3, Bsize: 4}); err != nil || got != 12 {
		t.Fatalf("filesystemAvailable = %d, %v", got, err)
	}
	if _, err := filesystemAvailable(syscall.Statfs_t{}); err == nil {
		t.Fatal("zero block size accepted")
	}
	if _, err := filesystemAvailable(syscall.Statfs_t{Bavail: ^uint64(0), Bsize: 2}); err == nil {
		t.Fatal("overflow accepted")
	}
}

func TestStorePressureSignalsInspectionFailuresAndLowSpace(t *testing.T) {
	var nilStore *Store
	if nilStore.UnderPressure() {
		t.Fatal("nil store reported pressure")
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "disks"), "")
	if err != nil {
		t.Fatal(err)
	}
	store.reservationBytes = 0
	if store.UnderPressure() {
		t.Fatal("zero reservation reported pressure")
	}
	store.reservationBytes = 1000
	store.statfs = func(string, *syscall.Statfs_t) error { return errors.New("statfs") }
	if !store.UnderPressure() {
		t.Fatal("statfs failure did not report pressure")
	}
	store.statfs = func(_ string, stat *syscall.Statfs_t) error {
		*stat = syscall.Statfs_t{}
		return nil
	}
	if !store.UnderPressure() {
		t.Fatal("invalid filesystem stats did not report pressure")
	}
	store.statfs = func(_ string, stat *syscall.Statfs_t) error {
		*stat = syscall.Statfs_t{Bavail: 9, Bsize: 10}
		return nil
	}
	if !store.UnderPressure() {
		t.Fatal("low space did not report pressure")
	}
	store.statfs = func(_ string, stat *syscall.Statfs_t) error {
		*stat = syscall.Statfs_t{Bavail: 10, Bsize: 10}
		return nil
	}
	if store.UnderPressure() {
		t.Fatal("threshold boundary reported pressure")
	}
}

func TestOpenStoreRejectsInvalidDurableFilesystemStats(t *testing.T) {
	statErr := errors.New("statfs")
	store := newStore()
	store.statfs = func(string, *syscall.Statfs_t) error { return statErr }
	if _, err := openStore(filepath.Join(t.TempDir(), "disks"), "", store); !errors.Is(err, statErr) {
		t.Fatalf("openStore error = %v", err)
	}
	store = newStore()
	store.statfs = func(_ string, stat *syscall.Statfs_t) error {
		*stat = syscall.Statfs_t{}
		return nil
	}
	if _, err := openStore(filepath.Join(t.TempDir(), "disks"), "", store); err == nil {
		t.Fatal("openStore accepted invalid filesystem stats")
	}
}
