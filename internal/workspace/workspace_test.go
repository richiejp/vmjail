package workspace

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/localai-org/darkaifactory/internal/cas"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
)

// randomBytes returns sz bytes from a seeded PRNG so tests are
// deterministic across runs and across architectures. Not for
// crypto use — these are test fixtures.
func randomBytes(seed1, seed2 uint64, sz int) []byte {
	rng := rand.New(rand.NewPCG(seed1, seed2)) //nolint:gosec // G404: test fixture, determinism > unpredictability
	out := make([]byte, sz)
	for i := range out {
		out[i] = byte(rng.Uint32()) //nolint:gosec // G115: deliberate truncation to byte for test fixture
	}
	return out
}

const osWindows = "windows"

func mustOpenStore(t *testing.T) *cas.Store {
	t.Helper()
	s, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatalf("cas.Open: %v", err)
	}
	return s
}

// readBack pulls every chunk for a FILE entry out of the store and
// concatenates them. Tests use this to round-trip file contents
// through the manifest.
func readBack(t *testing.T, store *cas.Store, entry *workspacev1.FileEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	for _, c := range entry.GetChunks() {
		rc, err := store.Get(c.GetDigest())
		if err != nil {
			t.Fatalf("Get %q: %v", c.GetDigest(), err)
		}
		if _, err := io.Copy(&out, rc); err != nil {
			t.Fatalf("Copy: %v", err)
		}
		if err := rc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	return out.Bytes()
}

func TestFixedSizeChunkerSplitsAtBoundary(t *testing.T) {
	store := mustOpenStore(t)
	payload := bytes.Repeat([]byte("A"), 4096)
	c := &FixedSizeChunker{ChunkSize: 1024}
	refs, total, err := c.Chunk(bytes.NewReader(payload), store)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if total != uint64(len(payload)) {
		t.Errorf("total = %d, want %d", total, len(payload))
	}
	if len(refs) != 4 {
		t.Errorf("chunks = %d, want 4", len(refs))
	}
	for _, r := range refs {
		if r.Size != 1024 {
			t.Errorf("chunk size = %d, want 1024", r.Size)
		}
	}
}

func TestFixedSizeChunkerHandlesPartialFinalChunk(t *testing.T) {
	store := mustOpenStore(t)
	payload := bytes.Repeat([]byte("B"), 1500) // 1024 + 476
	c := &FixedSizeChunker{ChunkSize: 1024}
	refs, total, err := c.Chunk(bytes.NewReader(payload), store)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if total != 1500 {
		t.Errorf("total = %d, want 1500", total)
	}
	if len(refs) != 2 {
		t.Fatalf("chunks = %d, want 2", len(refs))
	}
	if refs[0].Size != 1024 || refs[1].Size != 476 {
		t.Errorf("sizes = [%d, %d], want [1024, 476]", refs[0].Size, refs[1].Size)
	}
}

func TestFixedSizeChunkerEmptyStream(t *testing.T) {
	store := mustOpenStore(t)
	c := &FixedSizeChunker{ChunkSize: 1024}
	refs, total, err := c.Chunk(bytes.NewReader(nil), store)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if total != 0 || len(refs) != 0 {
		t.Errorf("expected empty result, got refs=%d total=%d", len(refs), total)
	}
}

func TestFixedSizeChunkerDefaultSize(t *testing.T) {
	store := mustOpenStore(t)
	c := &FixedSizeChunker{} // size 0 → DefaultChunkSize
	payload := bytes.Repeat([]byte("x"), DefaultChunkSize+1)
	refs, _, err := c.Chunk(bytes.NewReader(payload), store)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if len(refs) != 2 {
		t.Errorf("chunks = %d, want 2", len(refs))
	}
}

func TestFixedSizeChunkerRejectsNegativeSize(t *testing.T) {
	store := mustOpenStore(t)
	c := &FixedSizeChunker{ChunkSize: -1}
	if _, _, err := c.Chunk(bytes.NewReader(nil), store); err == nil {
		t.Fatal("expected error for negative chunk size")
	}
}

func TestFixedSizeChunkerRejectsNilStore(t *testing.T) {
	c := &FixedSizeChunker{}
	if _, _, err := c.Chunk(bytes.NewReader(nil), nil); err == nil {
		t.Fatal("expected error for nil store")
	}
}

// failingReader fails after returning n bytes. Lets us verify Chunk
// surfaces the underlying I/O error.
type failingReader struct {
	src io.Reader
	n   int
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, errors.New("synthetic read failure")
	}
	if len(p) > r.n {
		p = p[:r.n]
	}
	got, err := r.src.Read(p)
	r.n -= got
	return got, err
}

func TestFixedSizeChunkerPropagatesReadError(t *testing.T) {
	store := mustOpenStore(t)
	c := &FixedSizeChunker{ChunkSize: 1024}
	r := &failingReader{src: bytes.NewReader(bytes.Repeat([]byte("y"), 4096)), n: 100}
	if _, _, err := c.Chunk(r, store); err == nil {
		t.Fatal("expected read error")
	}
}

func TestBuildManifestRoundTrip(t *testing.T) {
	root := t.TempDir()
	store := mustOpenStore(t)

	mustWriteFile(t, filepath.Join(root, "a.txt"), []byte("alpha"))
	mustMkdir(t, filepath.Join(root, "sub"))
	mustWriteFile(t, filepath.Join(root, "sub", "b.bin"),
		bytes.Repeat([]byte("Z"), 3*1024))

	if runtime.GOOS != osWindows {
		if err := os.Symlink("a.txt", filepath.Join(root, "link-a")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
	}

	m, err := BuildManifest(root, store, BuildOptions{
		Chunker: &FixedSizeChunker{ChunkSize: 1024},
	})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if m.GetVersion() != ManifestVersion {
		t.Errorf("version = %d, want %d", m.GetVersion(), ManifestVersion)
	}

	// Entries are sorted by path — first entry should be "a.txt"
	// because it's lex-smaller than "link-a" and "sub".
	if len(m.GetEntries()) < 3 {
		t.Fatalf("entries = %d, want >= 3", len(m.GetEntries()))
	}

	byPath := map[string]*workspacev1.FileEntry{}
	for _, e := range m.GetEntries() {
		byPath[e.GetPath()] = e
	}

	a := byPath["a.txt"]
	if a == nil || a.GetKind() != workspacev1.EntryKind_ENTRY_KIND_FILE {
		t.Fatalf("a.txt entry missing or wrong kind: %+v", a)
	}
	if got := readBack(t, store, a); !bytes.Equal(got, []byte("alpha")) {
		t.Errorf("a.txt contents = %q, want %q", got, "alpha")
	}

	sub := byPath["sub"]
	if sub == nil || sub.GetKind() != workspacev1.EntryKind_ENTRY_KIND_DIR {
		t.Errorf("sub dir entry missing or wrong kind: %+v", sub)
	}

	bbin := byPath["sub/b.bin"]
	if bbin == nil || bbin.GetKind() != workspacev1.EntryKind_ENTRY_KIND_FILE {
		t.Fatalf("sub/b.bin entry missing or wrong kind: %+v", bbin)
	}
	if got := uint64(3 * 1024); bbin.GetSize() != got {
		t.Errorf("size = %d, want %d", bbin.GetSize(), got)
	}

	if runtime.GOOS != osWindows {
		link := byPath["link-a"]
		if link == nil || link.GetKind() != workspacev1.EntryKind_ENTRY_KIND_SYMLINK {
			t.Fatalf("link-a entry missing or wrong kind: %+v", link)
		}
		if link.GetSymlinkTarget() != "a.txt" {
			t.Errorf("symlink target = %q, want a.txt", link.GetSymlinkTarget())
		}
	}
}

func TestBuildManifestEntriesAreSorted(t *testing.T) {
	root := t.TempDir()
	store := mustOpenStore(t)

	for _, name := range []string{"z", "m", "a", "p"} {
		mustWriteFile(t, filepath.Join(root, name), []byte(name))
	}
	m, err := BuildManifest(root, store, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	got := make([]string, len(m.GetEntries()))
	for i, e := range m.GetEntries() {
		got[i] = e.GetPath()
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("entries not sorted: %v", got)
	}
}

func TestBuildManifestSkipPath(t *testing.T) {
	root := t.TempDir()
	store := mustOpenStore(t)
	mustWriteFile(t, filepath.Join(root, "keep"), []byte("k"))
	mustMkdir(t, filepath.Join(root, "node_modules"))
	mustWriteFile(t, filepath.Join(root, "node_modules", "huge"),
		bytes.Repeat([]byte("x"), 8192))

	m, err := BuildManifest(root, store, BuildOptions{
		SkipPath: func(rel string) bool { return rel == "node_modules" },
	})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	for _, e := range m.GetEntries() {
		if e.GetPath() == "node_modules" || strings.HasPrefix(e.GetPath(), "node_modules/") {
			t.Errorf("entry %q should have been skipped", e.GetPath())
		}
	}
}

func TestBuildManifestRejectsEmptyRoot(t *testing.T) {
	if _, err := BuildManifest("", mustOpenStore(t), BuildOptions{}); err == nil {
		t.Fatal("expected error for empty root")
	}
}

func TestBuildManifestRejectsNilStore(t *testing.T) {
	if _, err := BuildManifest(t.TempDir(), nil, BuildOptions{}); err == nil {
		t.Fatal("expected error for nil store")
	}
}

func TestBuildManifestRejectsMissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := BuildManifest(missing, mustOpenStore(t), BuildOptions{}); err == nil {
		t.Fatal("expected error for missing root")
	}
}

// TestBuildManifestPropagatesChunkerError uses a chunker that
// always errors so we exercise buildFileEntry's chunker-error
// branch.
func TestBuildManifestPropagatesChunkerError(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "a.txt"), []byte("a"))
	_, err := BuildManifest(root, mustOpenStore(t), BuildOptions{
		Chunker: errChunker{},
	})
	if err == nil {
		t.Fatal("expected error from chunker")
	}
}

type errChunker struct{}

func (errChunker) Chunk(io.Reader, *cas.Store) ([]*workspacev1.ChunkRef, uint64, error) {
	return nil, 0, errors.New("synthetic chunk error")
}

// TestFixedSizeChunkerPropagatesPutError: pre-plant a regular file
// at <store-root>/sha256 so the CAS Put's MkdirAll fails. Exercises
// putChunk's error-wrap branch.
func TestFixedSizeChunkerPropagatesPutError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sha256"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store, err := cas.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := &FixedSizeChunker{ChunkSize: 8}
	if _, _, err := c.Chunk(bytes.NewReader([]byte("hello")), store); err == nil {
		t.Fatal("expected error when CAS is poisoned")
	}
}

// TestBuildManifestPropagatesOpenError unreadable file → buildFileEntry
// open-error branch. Skipped if running as root (root can read
// 0o000 files).
func TestBuildManifestPropagatesOpenError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read 0o000 files; skip")
	}
	root := t.TempDir()
	bad := filepath.Join(root, "no-read")
	if err := os.WriteFile(bad, []byte("x"), 0o000); err != nil {
		t.Fatalf("seed: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o600) })
	_, err := BuildManifest(root, mustOpenStore(t), BuildOptions{})
	if err == nil {
		t.Fatal("expected error opening unreadable file")
	}
}

// TestBuildManifestRejectsFIFO uses syscall.Mkfifo (no privileges
// required) to plant a FIFO and verify buildEntry's
// unsupported-kind branch fires.
func TestBuildManifestRejectsFIFO(t *testing.T) {
	if runtime.GOOS == osWindows {
		t.Skip("FIFO not supported on Windows")
	}
	root := t.TempDir()
	fifoPath := filepath.Join(root, "pipe")
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		t.Skipf("Mkfifo: %v", err)
	}
	_, err := BuildManifest(root, mustOpenStore(t), BuildOptions{})
	if !errors.Is(err, ErrUnsupportedFileType) {
		t.Errorf("err = %v, want ErrUnsupportedFileType", err)
	}
}

func TestBuildManifestRejectsSocket(t *testing.T) {
	if runtime.GOOS == osWindows {
		t.Skip("Unix sockets not supported on Windows")
	}
	root := t.TempDir()
	sockPath := filepath.Join(root, "sock")
	if err := syscall.Mknod(sockPath, syscall.S_IFSOCK|0o600, 0); err != nil {
		t.Skipf("mknod: %v (likely needs CAP_MKNOD)", err)
	}
	_, err := BuildManifest(root, mustOpenStore(t), BuildOptions{})
	if !errors.Is(err, ErrUnsupportedFileType) {
		t.Errorf("err = %v, want ErrUnsupportedFileType", err)
	}
}

func TestSanitisePath(t *testing.T) {
	good := []string{"a", "a/b", "a/b/c.txt", "deeply/nested/path"}
	bad := []string{
		"",
		"/abs",
		"a//b",
		"./a",
		"a/./b",
		"a/../b",
		"..",
		"a\x00b",
	}
	for _, p := range good {
		if !SanitisePath(p) {
			t.Errorf("SanitisePath(%q) = false, want true", p)
		}
	}
	for _, p := range bad {
		if SanitisePath(p) {
			t.Errorf("SanitisePath(%q) = true, want false", p)
		}
	}
}

func FuzzSanitisePath(f *testing.F) {
	f.Add("a/b/c")
	f.Add("../etc/passwd")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		// Contract: must not panic. Survivors of SanitisePath must
		// also survive a round-trip through SplitSeq+Join.
		if SanitisePath(s) {
			joined := filepath.ToSlash(filepath.Clean(s))
			if joined != s {
				t.Fatalf("SanitisePath accepted %q but Clean = %q", s, joined)
			}
		}
	})
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}
