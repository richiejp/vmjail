//go:build linux

package daftfuse_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/daftfuse"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
)

// fuseMustHaveDev fails fast when /dev/fuse is missing — same
// hard-fail contract the e2e tests use for /dev/kvm. The repo's
// dev environment ships /dev/fuse + fusermount unconditionally;
// missing it is a misconfiguration, not a skip case.
func fuseMustHaveDev(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Fatalf("/dev/fuse missing: %v", err)
	}
}

func mountTempFS(t *testing.T, m *workspacev1.Manifest, fetcher daftfuse.ChunkFetcher) string {
	t.Helper()
	fuseMustHaveDev(t)
	mnt, _ := newMountWithCAS(t, m, fetcher, daftfuse.Options{})
	mp := t.TempDir()
	srv, err := mnt.Serve(context.Background(), mp)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Unmount(); err != nil {
			t.Logf("Unmount %s: %v", mp, err)
		}
	})
	return mp
}

func TestFuseReadFile(t *testing.T) {
	chunkA := []byte("alpha-")
	chunkB := []byte("beta!!")
	entry, digests, _ := chunkedFile(fixtureFile, chunkA, chunkB)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
	f := newFakeFetcher()
	primeFetcher(t, f, digests[0], chunkA)
	primeFetcher(t, f, digests[1], chunkB)
	mp := mountTempFS(t, manifest, f)

	data, err := os.ReadFile(filepath.Join(mp, fixtureFile)) //nolint:gosec // path is t.TempDir-rooted.
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := append(append([]byte{}, chunkA...), chunkB...)
	if !bytes.Equal(data, want) {
		t.Errorf("got %q, want %q", data, want)
	}
}

func TestFuseReaddirAndStat(t *testing.T) {
	chunk := []byte("contents")
	const filename = "a.txt"
	file, digests, _ := chunkedFile("dir/"+filename, chunk)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{
		file,
		{Path: "dir/b", Kind: workspacev1.EntryKind_ENTRY_KIND_DIR, Mode: 0o755},
		{
			Path: "dir/" + fixtureLink, Kind: workspacev1.EntryKind_ENTRY_KIND_SYMLINK,
			Mode: 0o777, SymlinkTarget: filename,
		},
	}}
	f := newFakeFetcher()
	primeFetcher(t, f, digests[0], chunk)
	mp := mountTempFS(t, manifest, f)

	entries, err := os.ReadDir(filepath.Join(mp, "dir"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	want := []string{filename, "b", fixtureLink}
	if len(got) != len(want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entries[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	target, err := os.Readlink(filepath.Join(mp, "dir", fixtureLink))
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != filename {
		t.Errorf("link target = %q", target)
	}

	info, err := os.Stat(filepath.Join(mp, "dir", filename))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != int64(len(chunk)) {
		t.Errorf("size = %d, want %d", info.Size(), len(chunk))
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("perm = %v, want 0o644", info.Mode().Perm())
	}
}

func TestFuseEnoent(t *testing.T) {
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{
		{Path: "real", Kind: workspacev1.EntryKind_ENTRY_KIND_FILE, Mode: 0o644},
	}}
	mp := mountTempFS(t, manifest, nil)
	if _, err := os.Open(filepath.Join(mp, "missing")); !errors.Is(err, os.ErrNotExist) { //nolint:gosec // path is t.TempDir-rooted.
		t.Errorf("expected ENOENT, got %v", err)
	}
}

func TestFusePolicyDeny(t *testing.T) {
	chunk := []byte("secret")
	entry, digests, _ := chunkedFile("s.txt", chunk)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
	f := newFakeFetcher()
	primeFetcher(t, f, digests[0], chunk)

	_, mp := mustMountWithPolicy(t, manifest, f, policyFunc(func(*daftfuse.ReadEvent) daftfuse.Decision {
		return daftfuse.Deny
	}))

	_, err := os.ReadFile(filepath.Join(mp, "s.txt")) //nolint:gosec // path is t.TempDir-rooted.
	if !errors.Is(err, syscall.EACCES) {
		t.Errorf("err = %v, want EACCES", err)
	}
}

func TestFusePolicyFreeze(t *testing.T) {
	chunk := []byte("frozen!")
	entry, digests, _ := chunkedFile("f.txt", chunk)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
	f := newFakeFetcher()
	primeFetcher(t, f, digests[0], chunk)
	_, mp := mustMountWithPolicy(t, manifest, f, policyFunc(func(*daftfuse.ReadEvent) daftfuse.Decision {
		return daftfuse.Freeze
	}))
	// O_NONBLOCK: the kernel forwards EAGAIN to userspace verbatim
	// for non-blocking opens. A blocking read would loop on the
	// EAGAIN until the policy flips, which is the correct VM-side
	// behaviour (Slice 4c will signal the freeze handler) but
	// wedges this test.
	fd, err := syscall.Open(filepath.Join(mp, "f.txt"), syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = syscall.Close(fd) }()
	buf := make([]byte, 8)
	_, err = syscall.Read(fd, buf)
	if !errors.Is(err, syscall.EAGAIN) {
		t.Errorf("err = %v, want EAGAIN", err)
	}
}

func mustMountWithPolicy(t *testing.T, m *workspacev1.Manifest, fetcher daftfuse.ChunkFetcher, p daftfuse.Policy) (mountObj *daftfuse.Mount, mountpoint string) {
	t.Helper()
	fuseMustHaveDev(t)
	mnt, _ := newMountWithCAS(t, m, fetcher, daftfuse.Options{Policy: p})
	mp := t.TempDir()
	srv, err := mnt.Serve(context.Background(), mp)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Unmount()
	})
	return mnt, mp
}

func TestFuseFstatHitsGetattr(t *testing.T) {
	chunk := []byte("getattr-me")
	entry, digests, _ := chunkedFile(fixtureFile, chunk)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
	f := newFakeFetcher()
	primeFetcher(t, f, digests[0], chunk)
	mp := mountTempFS(t, manifest, f)

	// Open the file then fstat the fd — go-fuse only invokes the
	// node's Getattr on fd-style stat; path-style stat lands in
	// Lookup's EntryOut population instead.
	file, err := os.Open(filepath.Join(mp, fixtureFile)) //nolint:gosec // path is t.TempDir-rooted.
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size() != int64(len(chunk)) {
		t.Errorf("size = %d, want %d", info.Size(), len(chunk))
	}
}

func TestFuseReadMissingChunkEIO(t *testing.T) {
	// File declares a chunk; the fetcher fails permanently and
	// the CAS is empty, so the kernel sees EIO on read.
	chunk := []byte("inaccessible")
	entry, digests, _ := chunkedFile("io.txt", chunk)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
	f := newFakeFetcher()
	f.failPerm[digests[0]] = true
	mp := mountTempFS(t, manifest, f)

	_, err := os.ReadFile(filepath.Join(mp, "io.txt")) //nolint:gosec // path is t.TempDir-rooted.
	if !errors.Is(err, syscall.EIO) {
		t.Errorf("err = %v, want EIO", err)
	}
}

func TestFuseOpenDirReturnsEISDIR(t *testing.T) {
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{
		{Path: "d", Kind: workspacev1.EntryKind_ENTRY_KIND_DIR, Mode: 0o755},
	}}
	mp := mountTempFS(t, manifest, nil)
	// Opening a dir for write (O_RDWR) trips the EISDIR branch
	// in Open — the read-only path open succeeds via Readdir.
	if _, err := os.OpenFile(filepath.Join(mp, "d"), os.O_RDWR, 0); err == nil { //nolint:gosec // path is t.TempDir-rooted.
		t.Error("expected error opening dir RDWR")
	}
}

func TestFuseReadlinkOnFile(t *testing.T) {
	entry, _, _ := chunkedFile(fixtureFile, []byte("x"))
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
	mp := mountTempFS(t, manifest, nil)
	if _, err := os.Readlink(filepath.Join(mp, fixtureFile)); err == nil {
		t.Error("expected error reading non-symlink as symlink")
	}
}

func TestFuseServerWait(t *testing.T) {
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{
		{Path: "x", Kind: workspacev1.EntryKind_ENTRY_KIND_FILE, Mode: 0o644},
	}}
	fuseMustHaveDev(t)
	mnt, _ := newMountWithCAS(t, manifest, nil, daftfuse.Options{})
	mp := t.TempDir()
	srv, err := mnt.Serve(context.Background(), mp)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	done := make(chan struct{})
	go func() {
		srv.Wait()
		close(done)
	}()
	if err := srv.Unmount(); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after Unmount")
	}
}

// silence linter about the unused io import on non-cover builds.
var _ = io.EOF
