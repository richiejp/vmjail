package daftfuse_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/localai-org/darkaifactory/internal/cas"
	"github.com/localai-org/darkaifactory/internal/daftfuse"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
)

// fakeFetcher is a ChunkFetcher backed by an in-memory digest→
// compressed-bytes map. Counters expose how many times each digest
// was requested so tests can assert singleflight + retry behaviour.
type fakeFetcher struct {
	mu        sync.Mutex
	chunks    map[string][]byte
	calls     map[string]int
	failOnce  map[string]int // remaining failures before success
	failPerm  map[string]bool
	failError error
	delay     time.Duration
}

func newFakeFetcher() *fakeFetcher {
	return &fakeFetcher{
		chunks:   map[string][]byte{},
		calls:    map[string]int{},
		failOnce: map[string]int{},
		failPerm: map[string]bool{},
	}
}

func (f *fakeFetcher) FetchChunk(ctx context.Context, digest string) ([]byte, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[digest]++
	if f.failPerm[digest] {
		if f.failError != nil {
			return nil, f.failError
		}
		return nil, errors.New("fake: permanent failure")
	}
	if n := f.failOnce[digest]; n > 0 {
		f.failOnce[digest] = n - 1
		return nil, errors.New("fake: transient failure")
	}
	b, ok := f.chunks[digest]
	if !ok {
		return nil, errors.New("fake: unknown digest")
	}
	return b, nil
}

func (f *fakeFetcher) callsFor(digest string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[digest]
}

// Common fixture names reused across tests.
const (
	fixtureFile = "file.txt"
	fixtureLink = "link"
)

// helpers ---------------------------------------------------------------

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func zstdCompress(t *testing.T, b []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	defer func() { _ = enc.Close() }()
	return enc.EncodeAll(b, nil)
}

// chunkedFile builds a FileEntry with one ChunkRef per len(chunks) byte slice.
// Returns the entry plus the digests so the test can prime CAS/fetcher.
func chunkedFile(path string, chunks ...[]byte) (entry *workspacev1.FileEntry, digests []string, sizes []uint64) {
	refs := make([]*workspacev1.ChunkRef, 0, len(chunks))
	digests = make([]string, 0, len(chunks))
	sizes = make([]uint64, 0, len(chunks))
	var total uint64
	for _, c := range chunks {
		d := sha256Hex(c)
		size := uint64(len(c))
		refs = append(refs, &workspacev1.ChunkRef{Digest: d, Size: size})
		digests = append(digests, d)
		sizes = append(sizes, size)
		total += size
	}
	entry = &workspacev1.FileEntry{
		Path:   path,
		Kind:   workspacev1.EntryKind_ENTRY_KIND_FILE,
		Mode:   0o644,
		Chunks: refs,
		Size:   total,
	}
	return entry, digests, sizes
}

func newMountWithCAS(t *testing.T, m *workspacev1.Manifest, fetcher daftfuse.ChunkFetcher, opts daftfuse.Options) (*daftfuse.Mount, *cas.Store) {
	t.Helper()
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatalf("cas.Open: %v", err)
	}
	mnt, err := daftfuse.New(m, store, fetcher, opts)
	if err != nil {
		t.Fatalf("daftfuse.New: %v", err)
	}
	return mnt, store
}

func primeFetcher(t *testing.T, f *fakeFetcher, digest string, payload []byte) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chunks[digest] = zstdCompress(t, payload)
}

// tests -----------------------------------------------------------------

func TestNewRejectsBadInput(t *testing.T) {
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatalf("cas.Open: %v", err)
	}
	if _, err := daftfuse.New(nil, store, nil, daftfuse.Options{}); err == nil {
		t.Error("expected error for nil manifest")
	}
	m := &workspacev1.Manifest{}
	if _, err := daftfuse.New(m, nil, nil, daftfuse.Options{}); err == nil {
		t.Error("expected error for nil store")
	}
	bad := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{
		{Path: "../escape", Kind: workspacev1.EntryKind_ENTRY_KIND_FILE},
	}}
	if _, err := daftfuse.New(bad, store, nil, daftfuse.Options{}); err == nil {
		t.Error("expected error for path with ..")
	}
}

func TestNewBadEntries(t *testing.T) {
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatalf("cas.Open: %v", err)
	}
	// File with wrong size (chunk sum != declared size).
	mismatch := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{{
		Path:   "f",
		Kind:   workspacev1.EntryKind_ENTRY_KIND_FILE,
		Chunks: []*workspacev1.ChunkRef{{Digest: "deadbeef", Size: 1}},
		Size:   99,
	}}}
	if _, err := daftfuse.New(mismatch, store, nil, daftfuse.Options{}); err == nil {
		t.Error("expected error for chunk-sum mismatch")
	}
	// Path traversing a regular file (file/file2).
	clash := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{
		{Path: "a", Kind: workspacev1.EntryKind_ENTRY_KIND_FILE, Size: 0},
		{Path: "a/inside", Kind: workspacev1.EntryKind_ENTRY_KIND_FILE, Size: 0},
	}}
	if _, err := daftfuse.New(clash, store, nil, daftfuse.Options{}); err == nil {
		t.Error("expected error for nested-under-file")
	}
	// Unsupported entry kind.
	bad := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{
		{Path: "x", Kind: workspacev1.EntryKind_ENTRY_KIND_UNSPECIFIED},
	}}
	if _, err := daftfuse.New(bad, store, nil, daftfuse.Options{}); err == nil {
		t.Error("expected error for UNSPECIFIED kind")
	}
}

func TestStatAndReaddir(t *testing.T) {
	chunk := []byte("hello world")
	filePath := "dir/" + fixtureFile
	linkPath := "dir/" + fixtureLink
	entry, _, _ := chunkedFile(filePath, chunk)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{
		entry,
		{
			Path: linkPath, Kind: workspacev1.EntryKind_ENTRY_KIND_SYMLINK,
			Mode: 0o777, SymlinkTarget: fixtureFile,
		},
		{Path: "dir/sub", Kind: workspacev1.EntryKind_ENTRY_KIND_DIR, Mode: 0o755},
	}}
	mnt, _ := newMountWithCAS(t, manifest, newFakeFetcher(), daftfuse.Options{})

	st, err := mnt.Stat(".")
	if err != nil {
		t.Fatalf("Stat root: %v", err)
	}
	if !st.Mode.IsDir() {
		t.Errorf("root mode = %v, want dir", st.Mode)
	}

	st, err = mnt.Stat(filePath)
	if err != nil {
		t.Fatalf("Stat file: %v", err)
	}
	if st.Size != uint64(len(chunk)) {
		t.Errorf("size = %d, want %d", st.Size, len(chunk))
	}

	st, err = mnt.Stat(linkPath)
	if err != nil {
		t.Fatalf("Stat symlink: %v", err)
	}
	if st.Mode&os.ModeSymlink == 0 {
		t.Errorf("link mode = %v missing ModeSymlink", st.Mode)
	}
	if st.Target != fixtureFile {
		t.Errorf("target = %q, want %s", st.Target, fixtureFile)
	}

	if _, err := mnt.Stat("missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing err = %v, want fs.ErrNotExist", err)
	}

	names, err := mnt.Readdir("dir")
	if err != nil {
		t.Fatalf("Readdir: %v", err)
	}
	want := []string{fixtureFile, fixtureLink, "sub"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Errorf("names = %v, want %v", names, want)
	}

	if _, err := mnt.Readdir(filePath); err == nil {
		t.Error("expected error reading file as dir")
	}
}

func TestReadlink(t *testing.T) {
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{
		{
			Path: fixtureLink, Kind: workspacev1.EntryKind_ENTRY_KIND_SYMLINK,
			Mode: 0o777, SymlinkTarget: "/etc/passwd",
		},
		{Path: "real", Kind: workspacev1.EntryKind_ENTRY_KIND_FILE, Mode: 0o600},
	}}
	mnt, _ := newMountWithCAS(t, manifest, nil, daftfuse.Options{})

	tgt, err := mnt.Readlink(fixtureLink)
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if tgt != "/etc/passwd" {
		t.Errorf("target = %q", tgt)
	}
	if _, err := mnt.Readlink("real"); err == nil {
		t.Error("expected error reading non-symlink")
	}
	if _, err := mnt.Readlink("missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing err = %v", err)
	}
}

func TestReadAtHappyPath(t *testing.T) {
	chunkA := []byte("alpha-")
	chunkB := []byte("beta")
	entry, digests, _ := chunkedFile("file.txt", chunkA, chunkB)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
	f := newFakeFetcher()
	primeFetcher(t, f, digests[0], chunkA)
	primeFetcher(t, f, digests[1], chunkB)
	mnt, _ := newMountWithCAS(t, manifest, f, daftfuse.Options{})

	full := make([]byte, 16)
	n, err := mnt.ReadAt(context.Background(), "file.txt", full, 0)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt err = %v, want io.EOF at end", err)
	}
	if n != len(chunkA)+len(chunkB) {
		t.Fatalf("ReadAt n = %d, want %d", n, len(chunkA)+len(chunkB))
	}
	want := append(append([]byte{}, chunkA...), chunkB...)
	if !bytes.Equal(full[:n], want) {
		t.Errorf("got %q, want %q", full[:n], want)
	}

	// Mid-file read crossing the chunk boundary.
	mid := make([]byte, 4)
	if _, err := mnt.ReadAt(context.Background(), "file.txt", mid, 4); err != nil {
		t.Fatalf("mid read: %v", err)
	}
	if string(mid) != "a-be" {
		t.Errorf("mid = %q, want %q", mid, "a-be")
	}

	// Offset at EOF.
	if n, err := mnt.ReadAt(context.Background(), "file.txt", mid, 1000); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("past-EOF n=%d err=%v, want 0/EOF", n, err)
	}

	// Empty dest.
	if n, err := mnt.ReadAt(context.Background(), "file.txt", nil, 0); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("nil dest n=%d err=%v", n, err)
	}

	// Negative offset rejected.
	if _, err := mnt.ReadAt(context.Background(), "file.txt", mid, -1); err == nil {
		t.Error("expected error for negative offset")
	}

	// Reading a non-file.
	mnt2, _ := newMountWithCAS(t, &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{
		{Path: "d", Kind: workspacev1.EntryKind_ENTRY_KIND_DIR, Mode: 0o755},
	}}, nil, daftfuse.Options{})
	if _, err := mnt2.ReadAt(context.Background(), "d", mid, 0); !errors.Is(err, daftfuse.ErrNotRegular) {
		t.Errorf("dir ReadAt err = %v, want ErrNotRegular", err)
	}

	// Missing path.
	if _, err := mnt2.ReadAt(context.Background(), "nope", mid, 0); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing ReadAt err = %v", err)
	}
}

func TestReadAtCachedChunk(t *testing.T) {
	chunk := []byte("cached!")
	entry, digests, _ := chunkedFile("c.txt", chunk)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
	f := newFakeFetcher()
	primeFetcher(t, f, digests[0], chunk)
	mnt, store := newMountWithCAS(t, manifest, f, daftfuse.Options{})

	// Prime CAS so the fetcher is never consulted.
	if err := store.PutCompressed(digests[0], zstdCompress(t, chunk)); err != nil {
		t.Fatalf("PutCompressed: %v", err)
	}
	buf := make([]byte, len(chunk))
	if _, err := mnt.ReadAt(context.Background(), "c.txt", buf, 0); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, chunk) {
		t.Errorf("got %q, want %q", buf, chunk)
	}
	if f.callsFor(digests[0]) != 0 {
		t.Errorf("fetcher should not have been called, got %d", f.callsFor(digests[0]))
	}
}

func TestReadAtFetcherRetry(t *testing.T) {
	chunk := []byte("retried-content")
	entry, digests, _ := chunkedFile("r.txt", chunk)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
	f := newFakeFetcher()
	primeFetcher(t, f, digests[0], chunk)
	f.failOnce[digests[0]] = 2 // first two attempts fail
	mnt, _ := newMountWithCAS(t, manifest, f, daftfuse.Options{
		FetchRetries: 4,
		FetchBackoff: 1 * time.Millisecond,
	})

	buf := make([]byte, len(chunk))
	if _, err := mnt.ReadAt(context.Background(), "r.txt", buf, 0); !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, chunk) {
		t.Errorf("got %q, want %q", buf, chunk)
	}
	if got := f.callsFor(digests[0]); got != 3 {
		t.Errorf("fetch calls = %d, want 3 (2 fail + 1 success)", got)
	}
}

func TestReadAtFetcherPermanentFailure(t *testing.T) {
	chunk := []byte("doomed")
	entry, digests, _ := chunkedFile("d.txt", chunk)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
	f := newFakeFetcher()
	f.failPerm[digests[0]] = true
	mnt, _ := newMountWithCAS(t, manifest, f, daftfuse.Options{
		FetchRetries: 2,
		FetchBackoff: 1 * time.Millisecond,
	})

	buf := make([]byte, len(chunk))
	if _, err := mnt.ReadAt(context.Background(), "d.txt", buf, 0); err == nil {
		t.Fatal("expected error from permanent fetcher failure")
	}
}

func TestReadAtNoFetcherMiss(t *testing.T) {
	chunk := []byte("orphan")
	entry, _, _ := chunkedFile("o.txt", chunk)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
	mnt, _ := newMountWithCAS(t, manifest, nil, daftfuse.Options{})
	buf := make([]byte, len(chunk))
	if _, err := mnt.ReadAt(context.Background(), "o.txt", buf, 0); err == nil {
		t.Fatal("expected error: no fetcher, no local chunk")
	}
}

func TestReadAtPolicyRefusals(t *testing.T) {
	cases := []struct {
		name    string
		verdict daftfuse.Decision
		wantErr error
	}{
		{"deny", daftfuse.Deny, daftfuse.ErrDenied},
		{"freeze", daftfuse.Freeze, daftfuse.ErrFrozen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunk := []byte("secret")
			entry, digests, _ := chunkedFile("s.txt", chunk)
			manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
			f := newFakeFetcher()
			primeFetcher(t, f, digests[0], chunk)
			verdict := tc.verdict
			mnt, _ := newMountWithCAS(t, manifest, f, daftfuse.Options{
				Policy: policyFunc(func(*daftfuse.ReadEvent) daftfuse.Decision { return verdict }),
			})
			if _, err := mnt.ReadAt(context.Background(), "s.txt", make([]byte, 6), 0); !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestReadAtCtxCancelDuringFetch(t *testing.T) {
	chunk := []byte("slow")
	entry, digests, _ := chunkedFile("s.txt", chunk)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
	f := newFakeFetcher()
	primeFetcher(t, f, digests[0], chunk)
	f.delay = 200 * time.Millisecond
	mnt, _ := newMountWithCAS(t, manifest, f, daftfuse.Options{
		FetchRetries: 2, FetchBackoff: 1 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := mnt.ReadAt(ctx, "s.txt", make([]byte, 4), 0); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestReadAtSingleflight(t *testing.T) {
	// One chunk shared by many concurrent readers. Fetcher delays
	// the first call so all readers queue behind it; assert the
	// fetcher saw exactly one call.
	chunk := bytes.Repeat([]byte("X"), 4096)
	entry, digests, _ := chunkedFile("p.txt", chunk)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
	f := newFakeFetcher()
	primeFetcher(t, f, digests[0], chunk)
	f.delay = 50 * time.Millisecond
	mnt, _ := newMountWithCAS(t, manifest, f, daftfuse.Options{})

	var wg sync.WaitGroup
	var errs atomic.Int32
	for range 8 {
		wg.Go(func() {
			buf := make([]byte, len(chunk))
			if _, err := mnt.ReadAt(context.Background(), "p.txt", buf, 0); err != nil && !errors.Is(err, io.EOF) {
				errs.Add(1)
			}
		})
	}
	wg.Wait()
	if errs.Load() != 0 {
		t.Fatalf("%d goroutines errored", errs.Load())
	}
	if got := f.callsFor(digests[0]); got != 1 {
		t.Errorf("fetch calls = %d, want 1 (singleflight collapsed concurrent)", got)
	}
}

// policyFunc is a Policy adapter so tests can supply a one-line
// decision lambda.
type policyFunc func(*daftfuse.ReadEvent) daftfuse.Decision

func (p policyFunc) OnRead(e *daftfuse.ReadEvent) daftfuse.Decision { return p(e) }

func TestAllowAllPolicy(t *testing.T) {
	if (daftfuse.AllowAll{}).OnRead(&daftfuse.ReadEvent{}) != daftfuse.Allow {
		t.Error("AllowAll did not allow")
	}
}

func TestLookupTraversesIntoFile(t *testing.T) {
	// A path like "a/b" where "a" is a regular file must surface
	// fs.ErrNotExist (lookup pretends children don't exist on
	// non-dir intermediates rather than erroring loudly).
	entry, _, _ := chunkedFile("a", []byte("x"))
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
	mnt, _ := newMountWithCAS(t, manifest, nil, daftfuse.Options{})
	if _, err := mnt.Stat("a/inner"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestRetryHaltsOnCtxCancel(t *testing.T) {
	chunk := []byte("never-arrives")
	entry, digests, _ := chunkedFile("x.txt", chunk)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
	f := newFakeFetcher()
	primeFetcher(t, f, digests[0], chunk)
	f.failPerm[digests[0]] = true
	f.failError = context.Canceled
	mnt, _ := newMountWithCAS(t, manifest, f, daftfuse.Options{
		FetchRetries: 5, FetchBackoff: 5 * time.Millisecond,
	})
	// Even with 5 retries configured, the first context.Canceled
	// from the fetcher short-circuits the retry loop.
	if _, err := mnt.ReadAt(context.Background(), "x.txt", make([]byte, 4), 0); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if got := f.callsFor(digests[0]); got != 1 {
		t.Errorf("calls = %d, want 1 (no retries on ctx error)", got)
	}
}

func TestDuplicateFileEntryRejected(t *testing.T) {
	// Two file entries at the same path → reject as duplicate.
	// Two dir entries at the same path are fine (the second
	// declared dir just overrides the implicit one's perms).
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatalf("cas.Open: %v", err)
	}
	dupFile := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{
		{Path: "x", Kind: workspacev1.EntryKind_ENTRY_KIND_FILE, Mode: 0o644, Size: 0},
		{Path: "x", Kind: workspacev1.EntryKind_ENTRY_KIND_FILE, Mode: 0o600, Size: 0},
	}}
	if _, err := daftfuse.New(dupFile, store, nil, daftfuse.Options{}); err == nil {
		t.Error("expected error for duplicate file entry")
	}
	dupDir := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{
		{Path: "d/f", Kind: workspacev1.EntryKind_ENTRY_KIND_FILE, Mode: 0o644, Size: 0},
		{Path: "d", Kind: workspacev1.EntryKind_ENTRY_KIND_DIR, Mode: 0o700},
	}}
	mnt, err := daftfuse.New(dupDir, store, nil, daftfuse.Options{})
	if err != nil {
		t.Fatalf("dup-dir manifest should succeed: %v", err)
	}
	st, err := mnt.Stat("d")
	if err != nil {
		t.Fatalf("Stat d: %v", err)
	}
	if st.Mode.Perm() != 0o700 {
		t.Errorf("declared dir entry did not override implicit perms: %o", st.Mode.Perm())
	}
}

func TestBackoffBudgetCancel(t *testing.T) {
	// Force the backoff sleep to outlive the parent ctx so the
	// time.After branch is exercised.
	chunk := []byte("y")
	entry, digests, _ := chunkedFile("y.txt", chunk)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{entry}}
	f := newFakeFetcher()
	primeFetcher(t, f, digests[0], chunk)
	f.failOnce[digests[0]] = 5
	mnt, _ := newMountWithCAS(t, manifest, f, daftfuse.Options{
		FetchRetries: 5, FetchBackoff: 500 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := mnt.ReadAt(ctx, "y.txt", make([]byte, 1), 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}
