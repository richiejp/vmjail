package egresscache

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
)

// TestPushPullRoundTrip packs a mock egress-cache tree, pushes it to a
// throwaway in-process registry, pulls it back, and asserts the tree
// round-trips byte-for-byte — proving the pack/extract path the
// `make update-egress-cache` / `make integration-build` steps rely on,
// without touching ghcr.
func TestPushPullRoundTrip(t *testing.T) {
	src := t.TempDir()
	// Mirror the httpcache layout: index.json + a fanned-out CAS blob.
	writeFile(t, filepath.Join(src, "index.json"), `{"GET https://x/y":{"digest":"sha256:ab","size":3}}`)
	writeFile(t, filepath.Join(src, "blobs", "sha256", "ab", "cd", "abcd"), "\x28\xb5\x2f\xfd body")

	ref := registryRef(t) + ":t1"
	if err := Push(src, ref, nil, crane.Insecure); err != nil {
		t.Fatalf("push: %v", err)
	}

	dst := t.TempDir()
	if err := Pull(ref, dst, nil, crane.Insecure); err != nil {
		t.Fatalf("pull: %v", err)
	}

	assertSameTree(t, src, dst)
}

// TestSafeJoin rejects tar paths that escape the destination.
func TestSafeJoin(t *testing.T) {
	dst := "/tmp/dest"
	for _, bad := range []string{"../escape", "/etc/passwd", "..", "a/../../b"} {
		if _, err := safeJoin(dst, bad); err == nil {
			t.Errorf("safeJoin(%q) = nil err, want rejection", bad)
		}
	}
	got, err := safeJoin(dst, "blobs/sha256/ab/cd/abcd")
	if err != nil {
		t.Fatalf("safeJoin(good): %v", err)
	}
	if want := filepath.Join(dst, "blobs", "sha256", "ab", "cd", "abcd"); got != want {
		t.Errorf("safeJoin = %q, want %q", got, want)
	}
}

// TestPushMissingDir surfaces a walk error from a nonexistent source.
func TestPushMissingDir(t *testing.T) {
	if err := Push(filepath.Join(t.TempDir(), "nope"), "x/y:z", nil); err == nil {
		t.Error("push of a missing dir: want error")
	}
}

// TestPullBadRef surfaces a pull error for an unreachable ref.
func TestPullBadRef(t *testing.T) {
	if err := Pull("127.0.0.1:0/no/such:ref", t.TempDir(), nil, crane.Insecure); err == nil {
		t.Error("pull of an unreachable ref: want error")
	}
}

// TestPushBadRef covers the crane.Push error arm: a well-formed local
// dir pushed at an unreachable registry.
func TestPushBadRef(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "index.json"), `{}`)
	if err := Push(src, "127.0.0.1:0/no/such:ref", nil, crane.Insecure); err == nil {
		t.Error("push to an unreachable ref: want error")
	}
}

// TestPullMkdirError covers Pull's destDir-mkdir failure: a valid image
// is pulled, but the destination is nested under a regular file.
func TestPullMkdirError(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "index.json"), `{}`)
	ref := registryRef(t) + ":mk"
	if err := Push(src, ref, nil, crane.Insecure); err != nil {
		t.Fatalf("push: %v", err)
	}
	blocker := filepath.Join(t.TempDir(), "file")
	writeFile(t, blocker, "x")
	if err := Pull(ref, filepath.Join(blocker, "sub"), nil, crane.Insecure); err == nil {
		t.Error("pull into a path under a file: want mkdir error")
	}
}

// TestCopyFileIntoDir covers copyFileInto's read error: opening a
// directory succeeds but copying from it fails.
func TestCopyFileIntoDir(t *testing.T) {
	if err := copyFileInto(io.Discard, t.TempDir()); err == nil {
		t.Error("copyFileInto of a directory: want error")
	}
}

// TestCopyFileIntoMissing covers copyFileInto's open error.
func TestCopyFileIntoMissing(t *testing.T) {
	if err := copyFileInto(io.Discard, filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("copyFileInto of a missing file: want error")
	}
}

// TestUntarOpenConflict covers extractEntry's OpenFile failure: a file
// entry whose target name is already a directory.
func TestUntarOpenConflict(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mustWriteHdr(t, tw, &tar.Header{Name: "blobs/", Typeflag: tar.TypeDir, Mode: 0o755})
	mustWriteHdr(t, tw, &tar.Header{Name: "blobs", Typeflag: tar.TypeReg, Size: 0, Mode: 0o644})
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := untar(&buf, t.TempDir()); err == nil {
		t.Error("untar of a file over a dir: want OpenFile error")
	}
}

// TestUntarMkdirConflict covers extractEntry's mkdir failure: a file
// entry whose parent path is already a file.
func TestUntarMkdirConflict(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mustWriteHdr(t, tw, &tar.Header{Name: "a", Typeflag: tar.TypeReg, Size: 0, Mode: 0o644})
	mustWriteHdr(t, tw, &tar.Header{Name: "a/b", Typeflag: tar.TypeReg, Size: 0, Mode: 0o644})
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := untar(&buf, t.TempDir()); err == nil {
		t.Error("untar with a file-as-parent: want mkdir error")
	}
}

// TestUntarDirAndFile extracts a hand-built tar with a dir entry and a
// nested file, covering both extractEntry branches + the mkdir path.
func TestUntarDirAndFile(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mustWriteHdr(t, tw, &tar.Header{Name: "blobs/", Typeflag: tar.TypeDir, Mode: 0o755})
	body := []byte("content")
	mustWriteHdr(t, tw, &tar.Header{Name: "blobs/x", Typeflag: tar.TypeReg, Size: int64(len(body)), Mode: 0o644})
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	dst := t.TempDir()
	if err := untar(&buf, dst); err != nil {
		t.Fatalf("untar: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "blobs", "x")) //nolint:gosec // G304: test reads a path it just wrote under its TempDir.
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("extracted body = %q, want %q", got, body)
	}
}

// TestUntarRejectsEscape stops a zip-slip entry mid-stream.
func TestUntarRejectsEscape(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mustWriteHdr(t, tw, &tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Size: 0, Mode: 0o644})
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := untar(&buf, t.TempDir()); err == nil {
		t.Error("untar of an escaping entry: want error")
	}
}

func mustWriteHdr(t *testing.T, tw *tar.Writer, hdr *tar.Header) {
	t.Helper()
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write header %s: %v", hdr.Name, err)
	}
}

func registryRef(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse registry url: %v", err)
	}
	return u.Host + "/test/egress-cache"
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// assertSameTree checks every regular file under want exists under got
// with identical bytes.
func assertSameTree(t *testing.T, want, got string) {
	t.Helper()
	err := filepath.Walk(want, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(want, path)
		wantBytes, _ := os.ReadFile(path)                      //nolint:gosec // G304: test-local path under TempDir.
		gotBytes, rerr := os.ReadFile(filepath.Join(got, rel)) //nolint:gosec // G304: test-local path under TempDir.
		if rerr != nil {
			t.Errorf("%s missing after round-trip: %v", rel, rerr)
			return nil
		}
		if !bytes.Equal(wantBytes, gotBytes) {
			t.Errorf("%s differs after round-trip", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}
