package httpcache_test

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/localai-org/darkaifactory/internal/httpcache"
)

func mustOpen(t *testing.T, dir string, offline bool) *httpcache.Cache {
	t.Helper()
	c, err := httpcache.Open(dir, offline, 0)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	return c
}

// TestPutLookupBody is the core record/replay round-trip: store a
// body under a key, then look it up and read the bytes back intact.
func TestPutLookupBody(t *testing.T) {
	c := mustOpen(t, t.TempDir(), false)
	body := "hello from the cached upstream\n"
	hdr := http.Header{"Content-Type": {"text/plain"}}

	entry, err := c.Put("GET https://example/x", http.StatusOK, hdr, strings.NewReader(body))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if entry.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", entry.Status)
	}
	if entry.Size != uint64(len(body)) {
		t.Errorf("size = %d, want %d", entry.Size, len(body))
	}

	got, ok := c.Lookup("GET https://example/x")
	if !ok {
		t.Fatal("Lookup miss for stored key")
	}
	if got.Digest != entry.Digest {
		t.Errorf("digest = %q, want %q", got.Digest, entry.Digest)
	}
	if got.Header.Get("Content-Type") != "text/plain" {
		t.Errorf("content-type = %q, want text/plain", got.Header.Get("Content-Type"))
	}

	rc, err := c.Body(got.Digest)
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	defer func() { _ = rc.Close() }()
	read, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(read) != body {
		t.Errorf("body = %q, want %q", read, body)
	}
}

// TestLookupMiss confirms an unrecorded key is a clean miss.
func TestLookupMiss(t *testing.T) {
	c := mustOpen(t, t.TempDir(), false)
	if _, ok := c.Lookup("GET https://example/never"); ok {
		t.Error("Lookup hit for never-stored key")
	}
}

// TestPersistenceAcrossOpen is the property the bun test depends on:
// a recording written by one process is visible to the next. Open a
// second Cache on the same dir and confirm the entry + body survive.
func TestPersistenceAcrossOpen(t *testing.T) {
	dir := t.TempDir()
	body := "persisted body bytes"
	first := mustOpen(t, dir, false)
	if _, err := first.Put("k1", 200, http.Header{}, strings.NewReader(body)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	second := mustOpen(t, dir, true)
	if !second.Offline() {
		t.Error("Offline() = false, want true")
	}
	got, ok := second.Lookup("k1")
	if !ok {
		t.Fatal("entry not visible after reopen")
	}
	rc, err := second.Body(got.Digest)
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	defer func() { _ = rc.Close() }()
	read, _ := io.ReadAll(rc)
	if string(read) != body {
		t.Errorf("body = %q, want %q", read, body)
	}
}

// TestPutDropsHopByHopHeaders pins the one transformation Put makes:
// framing/connection headers must not be replayed (they describe one
// wire response, not the payload), while payload headers survive.
func TestPutDropsHopByHopHeaders(t *testing.T) {
	c := mustOpen(t, t.TempDir(), false)
	hdr := http.Header{
		"Content-Type":      {"application/gzip"},
		"Transfer-Encoding": {"chunked"},
		"content-length":    {"999"}, // lowercase: must still be dropped
		"Connection":        {"keep-alive"},
		"Etag":              {"\"abc\""},
	}
	entry, err := c.Put("k", 200, hdr, strings.NewReader("x"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, drop := range []string{"Transfer-Encoding", "Content-Length", "Connection"} {
		if v := entry.Header.Get(drop); v != "" {
			t.Errorf("header %q = %q, want dropped", drop, v)
		}
	}
	if entry.Header.Get("Content-Type") != "application/gzip" {
		t.Error("Content-Type dropped; should survive")
	}
	if entry.Header.Get("ETag") != "\"abc\"" {
		t.Error("ETag dropped or not canonicalised; should survive")
	}
}

// TestOpenRejectsEmptyDir pins the guard.
func TestOpenRejectsEmptyDir(t *testing.T) {
	if _, err := httpcache.Open("", false, 0); err == nil {
		t.Error("Open(\"\") = nil err; want error")
	}
}

// TestOpenRejectsMalformedIndex: a corrupt index must fail loudly
// rather than silently start empty and re-record everything.
func TestOpenRejectsMalformedIndex(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed bad index: %v", err)
	}
	if _, err := httpcache.Open(dir, false, 0); err == nil {
		t.Error("Open with malformed index = nil err; want error")
	}
}

// TestBodyMissingDigest: asking for a digest the store never saw is
// an error, not a panic.
func TestBodyMissingDigest(t *testing.T) {
	c := mustOpen(t, t.TempDir(), false)
	missing := strings.Repeat("a", 64)
	if _, err := c.Body(missing); err == nil {
		t.Error("Body(missing) = nil err; want error")
	}
}

// TestOpenStoreError: if the blob store can't be created (here, a
// regular file sits where the blobs dir should go), Open fails
// rather than returning a half-built cache.
func TestOpenStoreError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "blobs"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocking file: %v", err)
	}
	if _, err := httpcache.Open(dir, false, 0); err == nil {
		t.Error("Open with blobs-is-a-file = nil err; want error")
	}
}

// TestPutBodyReadError: a body reader that errors mid-stream must
// surface as a Put error, not a partial recording.
func TestPutBodyReadError(t *testing.T) {
	c := mustOpen(t, t.TempDir(), false)
	if _, err := c.Put("k", 200, http.Header{}, errReader{}); err == nil {
		t.Error("Put with erroring body = nil err; want error")
	}
	if _, ok := c.Lookup("k"); ok {
		t.Error("failed Put left an index entry behind")
	}
}

// TestPutPersistError: when the index can't be written (cache dir
// made unwritable), Put fails so the caller doesn't believe a
// recording is durable when it isn't.
func TestPutPersistError(t *testing.T) {
	dir := t.TempDir()
	c := mustOpen(t, dir, false)
	//nolint:gosec // G302: deliberately read-only dir to force a write error.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	//nolint:gosec // G302: restore writability so t.TempDir cleanup succeeds.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := c.Put("k", 200, http.Header{}, strings.NewReader("x")); err == nil {
		t.Error("Put into unwritable cache dir = nil err; want error")
	}
}

// TestOpenMkdirError: Open under a path whose parent is a regular
// file can't create the cache dir and must error.
func TestOpenMkdirError(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := httpcache.Open(filepath.Join(parent, "cache"), false, 0); err == nil {
		t.Error("Open under a file = nil err; want error")
	}
}

// TestOpenIndexReadError: an index.json that exists but can't be
// read as a file (here it's a directory) is an error, distinct from
// the missing-index fresh-cache case.
func TestOpenIndexReadError(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "index.json"), 0o700); err != nil {
		t.Fatalf("seed index dir: %v", err)
	}
	if _, err := httpcache.Open(dir, false, 0); err == nil {
		t.Error("Open with index.json-as-dir = nil err; want error")
	}
}

// TestPutRenameError covers the index-rename failure branch via the
// renameFn seam: the recording's body is in the CAS but the index
// swap fails, so Put must error rather than claim durability.
func TestPutRenameError(t *testing.T) {
	c := mustOpen(t, t.TempDir(), false)
	httpcache.SetRenameFn(func(string, string) error { return io.ErrClosedPipe })
	t.Cleanup(httpcache.ResetSeams)
	if _, err := c.Put("k", 200, http.Header{}, strings.NewReader("x")); err == nil {
		t.Error("Put with failing rename = nil err; want error")
	}
}

// TestPutWriteError covers the tmp-index write-failure branch: the
// seam hands back an already-closed file so Write fails.
func TestPutWriteError(t *testing.T) {
	c := mustOpen(t, t.TempDir(), false)
	httpcache.SetCreateTempFn(func(dir, pattern string) (*os.File, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		_ = f.Close() // writes to a closed file fail
		return f, nil
	})
	t.Cleanup(httpcache.ResetSeams)
	if _, err := c.Put("k", 200, http.Header{}, strings.NewReader("x")); err == nil {
		t.Error("Put with failing tmp write = nil err; want error")
	}
}

// errReader fails on first Read, exercising the body-copy error path.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// monotonicClock returns a nowNanos stub whose value strictly
// increases by 1 per call, so Put/Lookup recency order is
// deterministic without sleeping.
func monotonicClock() func() int64 {
	var n int64
	return func() int64 { n++; return n }
}

// bodyOf reconstructs the cached body bytes for key, or fails.
func bodyOf(t *testing.T, c *httpcache.Cache, key string) []byte {
	t.Helper()
	e, ok := c.Lookup(key)
	if !ok {
		t.Fatalf("Lookup(%q) miss", key)
	}
	rc, err := c.Body(e.Digest)
	if err != nil {
		t.Fatalf("Body(%q): %v", key, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body(%q): %v", key, err)
	}
	return b
}

// TestEvictionByteCapLRU: with a byte cap, an over-cap Put evicts the
// least-recently-used entry. Three 10-byte bodies into a 25-byte cap
// leaves the two most recent.
func TestEvictionByteCapLRU(t *testing.T) {
	httpcache.SetNowNanos(monotonicClock())
	t.Cleanup(httpcache.ResetSeams)
	c, err := httpcache.Open(t.TempDir(), false, 25)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ten := strings.Repeat("x", 10)
	for _, k := range []string{"k1", "k2", "k3"} {
		mustPut(t, c, k, ten)
	}
	if _, ok := c.Lookup("k1"); ok {
		t.Error("k1 should have been evicted (oldest)")
	}
	for _, k := range []string{"k2", "k3"} {
		if _, ok := c.Lookup(k); !ok {
			t.Errorf("%s should have survived", k)
		}
	}
}

// TestEvictionHonorsLookupRecency: a Lookup bumps recency, so a
// subsequently-Put entry evicts a different (now-oldest) victim.
func TestEvictionHonorsLookupRecency(t *testing.T) {
	httpcache.SetNowNanos(monotonicClock())
	t.Cleanup(httpcache.ResetSeams)
	c, err := httpcache.Open(t.TempDir(), false, 25)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ten := strings.Repeat("x", 10)
	mustPut(t, c, "k1", ten)
	mustPut(t, c, "k2", ten)
	c.Lookup("k1") // bump k1 ahead of k2
	mustPut(t, c, "k3", ten)
	if _, ok := c.Lookup("k2"); ok {
		t.Error("k2 should have been evicted (least recently used)")
	}
	if _, ok := c.Lookup("k1"); !ok {
		t.Error("k1 should have survived (recently looked up)")
	}
}

// TestEvictionSkipsSharedDigest: two keys sharing one body keep the
// blob alive even when one key is evicted.
func TestEvictionSkipsSharedDigest(t *testing.T) {
	httpcache.SetNowNanos(monotonicClock())
	t.Cleanup(httpcache.ResetSeams)
	c, err := httpcache.Open(t.TempDir(), false, 15)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	shared := strings.Repeat("y", 10)
	mustPut(t, c, "k1", shared)
	mustPut(t, c, "k2", shared) // same digest; total counted 20 > 15 → evict k1
	if _, ok := c.Lookup("k1"); ok {
		t.Error("k1 should have been evicted")
	}
	if got := bodyOf(t, c, "k2"); string(got) != shared {
		t.Errorf("k2 body = %q, want %q (shared blob must survive)", got, shared)
	}
}

// TestNoEvictionWhenUnbounded: maxBytes 0 never evicts.
func TestNoEvictionWhenUnbounded(t *testing.T) {
	c, err := httpcache.Open(t.TempDir(), false, 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	keys := []string{"a", "b", "c", "d", "e"}
	for _, k := range keys {
		mustPut(t, c, k, strings.Repeat("z", 1000))
	}
	for _, k := range keys {
		if _, ok := c.Lookup(k); !ok {
			t.Errorf("%s evicted under unbounded cap", k)
		}
	}
}

func mustPut(t *testing.T, c *httpcache.Cache, key, body string) {
	t.Helper()
	if _, err := c.Put(key, 200, http.Header{}, strings.NewReader(body)); err != nil {
		t.Fatalf("Put(%s): %v", key, err)
	}
}
