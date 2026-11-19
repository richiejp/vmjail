//go:build linux && integration

package sandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/richiejp/vmjail/internal/agent"
	"github.com/richiejp/vmjail/internal/imagestore"
)

// newImagestore creates a Store rooted at a fresh temp dir with a
// per-test quota high enough to fit any synthetic image.
func newImagestore(t *testing.T) *imagestore.Store {
	t.Helper()
	store, err := imagestore.New(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatalf("imagestore.New: %v", err)
	}
	return store
}

func storePull(t *testing.T, s *imagestore.Store, ref string) (*imagestore.Image, error) {
	t.Helper()
	return s.Pull(t.Context(), ref)
}

func storeBundle(t *testing.T, s *imagestore.Store, digest string) (*imagestore.Bundle, error) {
	t.Helper()
	return s.Bundle(t.Context(), digest, imagestore.BundleOptions{})
}

// pushHelloIntegrationImage stands up an in-process registry, pushes
// an image whose only file is a static binary that prints
// agent.ContainerBanner, and returns (host, full ref).
//
// Compiling a Go binary at test time would be slow and pull in a
// toolchain dep, so we reuse the Nix-built vmjail-hello-container
// binary that already lives at result-bundle/rootfs/hello.
func pushHelloIntegrationImage(t *testing.T) (host, ref string) {
	t.Helper()

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse registry URL: %v", err)
	}

	helloBin := readHelloBinary(t)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "hello", Mode: 0o755, Size: int64(len(helloBin))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(helloBin); err != nil {
		t.Fatalf("tar body: %v", err)
	}
	_ = tw.Close()
	_ = gz.Close()

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		t.Fatalf("layer: %v", err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	img, err = mutate.Config(img, v1.Config{Entrypoint: []string{"/hello"}})
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	ref = u.Host + "/test/hello-imagestore:v1"
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push: %v", err)
	}
	return u.Host, ref
}

// readHelloBinary slurps the Nix-built vmjail-hello-container binary
// from result-bundle/rootfs/hello, asserts it embeds the banner
// constant we expect (so a stale bundle fails fast), and returns
// the bytes.
func readHelloBinary(t *testing.T) []byte {
	t.Helper()
	helloPath := filepath.Join(resolveBundle(t), "rootfs", "hello")
	b, err := os.ReadFile(helloPath)
	if err != nil {
		t.Fatalf("read hello binary at %s: %v", helloPath, err)
	}
	if len(b) == 0 {
		t.Fatalf("hello binary at %s is empty", helloPath)
	}
	if !bytes.Contains(b, []byte(agent.ContainerBanner)) {
		t.Fatalf("hello binary doesn't contain ContainerBanner %q — stale bundle?",
			agent.ContainerBanner)
	}
	return b
}
