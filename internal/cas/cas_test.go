package cas

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustOpen(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func digestOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestOpenRejectsEmptyDir(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Fatal("expected error for empty dir")
	}
	if _, err := Open("   "); err == nil {
		t.Fatal("expected error for whitespace-only dir")
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	s := mustOpen(t)
	payload := []byte("hello, world\n")

	digest, size, err := s.Put(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if digest != digestOf(payload) {
		t.Errorf("digest = %q, want %q", digest, digestOf(payload))
	}
	if size != uint64(len(payload)) {
		t.Errorf("size = %d, want %d", size, len(payload))
	}

	rc, err := s.Get(digest)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("readback = %q, want %q", got, payload)
	}
}

func TestPutOfEmptyPayloadIsStable(t *testing.T) {
	s := mustOpen(t)
	d1, _, err := s.Put(bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	d2, _, err := s.Put(bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if d1 != d2 {
		t.Errorf("digest of empty drifted: %q vs %q", d1, d2)
	}
	// Standard sha256 of empty.
	if d1 != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("empty digest = %q, want canonical", d1)
	}
}

func TestPutIsIdempotentForSameContent(t *testing.T) {
	s := mustOpen(t)
	payload := bytes.Repeat([]byte("x"), 4096)
	d1, _, err := s.Put(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	d2, _, err := s.Put(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if d1 != d2 {
		t.Errorf("digests differ across calls")
	}
}

func TestHas(t *testing.T) {
	s := mustOpen(t)
	missing := digestOf([]byte("nope"))
	ok, err := s.Has(missing)
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if ok {
		t.Error("Has on missing returned true")
	}

	digest, _, err := s.Put(bytes.NewReader([]byte("present")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	ok, err = s.Has(digest)
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if !ok {
		t.Error("Has on present returned false")
	}
}

func TestDeleteRemovesEntry(t *testing.T) {
	s := mustOpen(t)
	digest, _, err := s.Put(bytes.NewReader([]byte("delete me")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(digest); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	ok, _ := s.Has(digest)
	if ok {
		t.Error("entry still present after Delete")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	s := mustOpen(t)
	if err := s.Delete(digestOf([]byte("never existed"))); err != nil {
		t.Errorf("Delete of missing = %v, want nil", err)
	}
}

func TestRootReturnsSeededDir(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if s.Root() != dir {
		t.Errorf("Root = %q, want %q", s.Root(), dir)
	}
}

func TestPutGetForLargePayload(t *testing.T) {
	s := mustOpen(t)
	// 1 MiB of varying bytes — exercises zstd's frame logic and
	// makes sure the streaming readback assembles correctly.
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i * 31) //nolint:gosec // G115: deliberate truncation to byte for test fixture
	}
	digest, _, err := s.Put(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := s.Get(digest)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("readback differs from input (len got=%d want=%d)", len(got), len(payload))
	}
}

func TestGetMissingReturnsNotExist(t *testing.T) {
	s := mustOpen(t)
	missing := digestOf([]byte("nope"))
	_, err := s.Get(missing)
	if err == nil {
		t.Fatal("expected error for missing digest")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want os.ErrNotExist", err)
	}
}

func TestValidateDigestRejectsBadInputs(t *testing.T) {
	cases := []string{
		"",
		"abc",
		strings.Repeat("Z", 64),          // non-hex
		strings.Repeat("A", 64),          // uppercase rejected
		strings.Repeat("a", 63),          // too short
		strings.Repeat("a", 65),          // too long
		strings.Repeat("a", 60) + "ghij", // non-hex tail
	}
	for _, c := range cases {
		if err := validateDigest(c); err == nil {
			t.Errorf("validateDigest(%q) = nil, want error", c)
		}
	}
}

func TestPathForRejectsBadDigest(t *testing.T) {
	s := mustOpen(t)
	if _, err := s.pathFor("nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestHasRejectsBadDigest(t *testing.T) {
	s := mustOpen(t)
	if _, err := s.Has("nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestGetRejectsBadDigest(t *testing.T) {
	s := mustOpen(t)
	if _, err := s.Get("nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestDeleteRejectsBadDigest(t *testing.T) {
	s := mustOpen(t)
	if err := s.Delete("nope"); err == nil {
		t.Fatal("expected error")
	}
}

// TestGetDetectsCorruption corrupts an on-disk chunk and verifies
// the verifying reader catches the digest mismatch on read.
func TestGetDetectsCorruption(t *testing.T) {
	s := mustOpen(t)
	digest, _, err := s.Put(bytes.NewReader([]byte("genuine")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	path, _ := s.pathFor(digest)

	// Replace the on-disk file with the zstd-compressed encoding
	// of *different* bytes — Open will succeed, decompress will
	// succeed, but the running hash won't match.
	other, _, err := s.Put(bytes.NewReader([]byte("counterfeit")))
	if err != nil {
		t.Fatalf("Put counterfeit: %v", err)
	}
	otherPath, _ := s.pathFor(other)
	if err := os.Rename(otherPath, path); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	rc, err := s.Get(digest)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = rc.Close() }()
	_, err = io.ReadAll(rc)
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("err = %v, want ErrCorrupt", err)
	}
}

// failingReader fails after returning n bytes. Forces Put's
// copy-error branch.
type failingReader struct{ n int }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, errors.New("synthetic read failure")
	}
	if len(p) > r.n {
		p = p[:r.n]
	}
	for i := range p {
		p[i] = byte(i)
	}
	r.n -= len(p)
	return len(p), nil
}

func TestPutPropagatesReadError(t *testing.T) {
	s := mustOpen(t)
	if _, _, err := s.Put(&failingReader{n: 100}); err == nil {
		t.Fatal("expected error from failing reader")
	}
}

// TestPutFailsWhenAlgoPathIsAFile pre-plants a regular file at
// `<root>/sha256` so the lazy MkdirAll call inside Put fails with
// ENOTDIR. Exercises the mkdir error branch.
func TestPutFailsWhenAlgoPathIsAFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sha256"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, _, err := s.Put(bytes.NewReader([]byte("hello"))); err == nil {
		t.Fatal("expected error when algo path is a file")
	}
}

// TestPutFailsWhenDestPathIsADir hits the rename error branch by
// pre-creating a non-empty directory at the destination chunk path.
// io.Copy/zstd succeed; only the final Rename fails.
func TestPutFailsWhenDestPathIsADir(t *testing.T) {
	s := mustOpen(t)
	payload := []byte("destination-clash")
	dest, err := s.pathFor(digestOf(payload))
	if err != nil {
		t.Fatalf("pathFor: %v", err)
	}
	// #nosec G301 — test-only.
	if err := os.MkdirAll(filepath.Join(dest, "child"), 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := s.Put(bytes.NewReader(payload)); err == nil {
		t.Fatal("expected rename error when dest is a non-empty dir")
	}
}

// TestGetRejectsNonZstdContent plants raw bytes at a chunk path so
// Get's zstd.NewReader call fails on the first read. Covers Read's
// zstd-error branch.
func TestGetRejectsNonZstdContent(t *testing.T) {
	s := mustOpen(t)
	payload := []byte("plant")
	digest := digestOf(payload)
	dest, err := s.pathFor(digest)
	if err != nil {
		t.Fatalf("pathFor: %v", err)
	}
	// #nosec G301 — test-only.
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dest, []byte("not-zstd"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	rc, err := s.Get(digest)
	if err != nil {
		// Some zstd impls fail at NewReader, others on first Read.
		// Either is acceptable here.
		return
	}
	defer func() { _ = rc.Close() }()
	_, err = io.ReadAll(rc)
	if err == nil {
		t.Fatal("expected error reading non-zstd content")
	}
}

func TestOpenFailsForUncreatable(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := Open(filepath.Join(blocker, "child")); err == nil {
		t.Fatal("expected error when parent is a file")
	}
}

func FuzzValidateDigest(f *testing.F) {
	f.Add("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	f.Add("garbage")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		// Only contract: must not panic; valid digest implies the
		// path is round-trip clean (no separators inserted).
		if err := validateDigest(s); err == nil {
			st := &Store{root: "/tmp/fuzz"}
			path, err := st.pathFor(s)
			if err != nil {
				t.Fatalf("pathFor reproducer: validate accepted %q but pathFor rejected: %v", s, err)
			}
			if !strings.HasSuffix(path, s) {
				t.Fatalf("pathFor(%q) doesn't end in digest: %q", s, path)
			}
		}
	})
}
