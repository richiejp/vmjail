package buildimage

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// TestStubAssetNotBundled covers the plain-`go build` path: the committed
// asset is a zero-byte stub, so IsBundled is false and Image surfaces
// ErrNotBundled. (The flake overwrites the stub with the real image; that
// path is exercised by imageFrom below without needing the embed populated.)
func TestStubAssetNotBundled(t *testing.T) {
	if IsBundled() {
		t.Fatal("IsBundled() = true with the stub asset; expected false")
	}
	if _, err := Image(); !errors.Is(err, ErrNotBundled) {
		t.Fatalf("Image() err = %v, want ErrNotBundled", err)
	}
}

// TestImageFromStubBytes covers the size-gate: anything below the stub
// threshold is treated as not-bundled.
func TestImageFromStubBytes(t *testing.T) {
	if _, err := imageFrom([]byte("tiny")); !errors.Is(err, ErrNotBundled) {
		t.Fatalf("imageFrom(small) err = %v, want ErrNotBundled", err)
	}
	if _, err := imageFrom(nil); !errors.Is(err, ErrNotBundled) {
		t.Fatalf("imageFrom(nil) err = %v, want ErrNotBundled", err)
	}
}

// TestImageFromRealTarball covers the bundled path: real (>= threshold)
// image-tarball bytes load into a usable v1.Image.
func TestImageFromRealTarball(t *testing.T) {
	raw := tarballBytes(t)
	if len(raw) < minBundledSize {
		t.Fatalf("test tarball %d bytes < threshold %d; bump random.Image size", len(raw), minBundledSize)
	}
	img, err := imageFrom(raw)
	if err != nil {
		t.Fatalf("imageFrom(real) err = %v", err)
	}
	if _, err := img.Digest(); err != nil {
		t.Fatalf("loaded image Digest() err = %v", err)
	}
}

// tarballBytes builds a small but >threshold docker-archive image tarball.
func tarballBytes(t *testing.T) []byte {
	t.Helper()
	img, err := random.Image(2048, 2)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	ref, err := name.ParseReference("daft-builtin/build:test")
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	var buf bytes.Buffer
	if err := tarball.Write(ref, img, &buf); err != nil {
		t.Fatalf("tarball.Write: %v", err)
	}
	return buf.Bytes()
}
