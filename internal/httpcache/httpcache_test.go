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
	c, err := httpcache.Open(dir, offline)
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
	if _, err := httpcache.Open("", false); err == nil {
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
	if _, err := httpcache.Open(dir, false); err == nil {
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
	if _, err := httpcache.Open(dir, false); err == nil {
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
	if _, err := httpcache.Open(filepath.Join(parent, "cache"), false); err == nil {
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
	if _, err := httpcache.Open(dir, false); err == nil {
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
