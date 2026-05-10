package bundled

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractReturnsErrNotBundledOnStubs(t *testing.T) {
	_, err := extractTo(t.TempDir(), nil, nil, nil, nil)
	if !errors.Is(err, ErrNotBundled) {
		t.Fatalf("err = %v, want ErrNotBundled", err)
	}

	tiny := []byte{0x7f}
	_, err = extractTo(t.TempDir(), tiny, tiny, tiny, tiny)
	if !errors.Is(err, ErrNotBundled) {
		t.Fatalf("err with sub-threshold blobs = %v, want ErrNotBundled", err)
	}
}

func TestExtractWritesAllArtifacts(t *testing.T) {
	dir := t.TempDir()
	ch := bytes.Repeat([]byte("CH"), minBundledSize)
	kernel := bytes.Repeat([]byte("KERNEL"), minBundledSize)
	initramfs := bytes.Repeat([]byte("INITRD"), minBundledSize)
	virtiofsd := bytes.Repeat([]byte("VFSD"), minBundledSize)

	p, err := extractTo(dir, ch, kernel, initramfs, virtiofsd)
	if err != nil {
		t.Fatalf("extractTo: %v", err)
	}

	for _, want := range []struct {
		path string
		data []byte
		perm os.FileMode
	}{
		{p.CloudHypervisor, ch, 0o755},
		{p.Kernel, kernel, 0o644},
		{p.Initramfs, initramfs, 0o644},
		{p.Virtiofsd, virtiofsd, 0o755},
	} {
		got, err := os.ReadFile(want.path)
		if err != nil {
			t.Errorf("reading %s: %v", want.path, err)
			continue
		}
		if !bytes.Equal(got, want.data) {
			t.Errorf("%s: content mismatch (got %d bytes, want %d)", want.path, len(got), len(want.data))
		}
		fi, err := os.Stat(want.path)
		if err != nil {
			t.Errorf("stat %s: %v", want.path, err)
			continue
		}
		// Mask out non-perm bits; some umask environments add the
		// directory bit etc. We only care that exec is preserved
		// (or not) for the binary vs the data files.
		gotExec := fi.Mode().Perm() & 0o111
		wantExec := want.perm & 0o111
		if gotExec != wantExec {
			t.Errorf("%s: exec bits = %o, want %o", want.path, gotExec, wantExec)
		}
	}
}

func TestExtractIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	ch := bytes.Repeat([]byte("CH"), minBundledSize)
	kernel := bytes.Repeat([]byte("KERNEL"), minBundledSize)
	initramfs := bytes.Repeat([]byte("INITRD"), minBundledSize)
	virtiofsd := bytes.Repeat([]byte("VFSD"), minBundledSize)

	p1, err := extractTo(dir, ch, kernel, initramfs, virtiofsd)
	if err != nil {
		t.Fatalf("first extractTo: %v", err)
	}
	mtime1, err := os.Stat(p1.CloudHypervisor)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Re-running with the same data must not rewrite the file —
	// callers depend on this for caching.
	p2, err := extractTo(dir, ch, kernel, initramfs, virtiofsd)
	if err != nil {
		t.Fatalf("second extractTo: %v", err)
	}
	if p1 != p2 {
		t.Errorf("paths changed between calls: %+v vs %+v", p1, p2)
	}
	mtime2, err := os.Stat(p2.CloudHypervisor)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !mtime1.ModTime().Equal(mtime2.ModTime()) {
		t.Errorf("cloud-hypervisor was rewritten on no-op extract (mtime changed)")
	}
}

func TestExtractRewritesOnSizeChange(t *testing.T) {
	dir := t.TempDir()
	old := bytes.Repeat([]byte("X"), minBundledSize)
	if _, err := extractTo(dir, old, old, old, old); err != nil {
		t.Fatalf("first extractTo: %v", err)
	}

	// Different size → must overwrite.
	newer := bytes.Repeat([]byte("X"), minBundledSize+10)
	if _, err := extractTo(dir, newer, newer, newer, newer); err != nil {
		t.Fatalf("second extractTo: %v", err)
	}
	// #nosec G304 — test-only; dir is t.TempDir().
	got, err := os.ReadFile(filepath.Join(dir, "cloud-hypervisor"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, newer) {
		t.Errorf("file not rewritten on size change")
	}
}

// TestExtractMkdirError covers the error path of os.MkdirAll inside
// extractTo. We point dir at a path whose parent is a regular file,
// which makes MkdirAll fail with ENOTDIR.
func TestExtractMkdirError(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}

	bytesAtSize := bytes.Repeat([]byte("X"), minBundledSize)
	_, err := extractTo(filepath.Join(blocker, "cache"),
		bytesAtSize, bytesAtSize, bytesAtSize, bytesAtSize)
	if err == nil {
		t.Fatal("expected error when parent is a regular file")
	}
}

// TestExtractWriteError covers the error path of writeIfChanged when
// the target path is a directory rather than a file (so the OpenFile
// inside os.WriteFile fails).
func TestExtractWriteError(t *testing.T) {
	dir := t.TempDir()
	// Pre-create a directory at the path where cloud-hypervisor would
	// be written — os.WriteFile then can't replace it.
	// #nosec G301 — test-only; dir is t.TempDir().
	if err := os.MkdirAll(filepath.Join(dir, "cloud-hypervisor"), 0o755); err != nil {
		t.Fatalf("pre-create dir: %v", err)
	}
	bytesAtSize := bytes.Repeat([]byte("X"), minBundledSize)
	_, err := extractTo(dir, bytesAtSize, bytesAtSize, bytesAtSize, bytesAtSize)
	if err == nil {
		t.Fatal("expected error when artifact path is a directory")
	}
}

func TestExtractWrapperPropagatesStubsError(t *testing.T) {
	// The exported Extract wraps extractTo with the package-level
	// embedded byte slices. In a plain `go test` build those slices
	// are 0-byte stubs, so the call should return ErrNotBundled
	// without touching disk. Covers the otherwise-uncovered Extract
	// public surface.
	_, err := Extract(t.TempDir())
	if !errors.Is(err, ErrNotBundled) {
		t.Fatalf("Extract on stub binary = %v, want ErrNotBundled", err)
	}
}

func TestIsBundledReflectsBuildState(t *testing.T) {
	// In `go test` runs (no Nix preBuild), the embedded slices are
	// stubs, so IsBundled should be false. The Nix-built binary
	// inverts this; that path is exercised by the daft-sandboxd
	// integration test, not here.
	if IsBundled() {
		t.Error("IsBundled() = true in plain `go test`; expected stubs")
	}
}
