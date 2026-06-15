package ociregistry

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func TestBuiltinBuildRef(t *testing.T) {
	ref := BuiltinBuildRef()
	if want := Ref(BuiltinBuildRepo, BuiltinBuildTag); ref != want {
		t.Errorf("BuiltinBuildRef = %q, want %q", ref, want)
	}
	if !IsHosted(ref) {
		t.Errorf("BuiltinBuildRef %q should be a hosted ref", ref)
	}
}

// TestImageFromReader covers loading an image from raw and gzipped tar
// bytes (the embedded/bundled path used by internal/buildimage), plus the
// not-a-tar error branch.
func TestImageFromReader(t *testing.T) {
	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	ref, err := name.ParseReference("test/img:v1")
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	var raw bytes.Buffer
	if err := tarball.Write(ref, img, &raw); err != nil {
		t.Fatalf("tarball.Write: %v", err)
	}
	want, err := img.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	assertSameDigest := func(label string, in []byte) {
		got, err := ImageFromReader(bytes.NewReader(in))
		if err != nil {
			t.Fatalf("%s: ImageFromReader: %v", label, err)
		}
		d, err := got.Digest()
		if err != nil {
			t.Fatalf("%s: digest: %v", label, err)
		}
		if d != want {
			t.Errorf("%s: digest = %s, want %s", label, d, want)
		}
	}
	assertSameDigest("raw tar", raw.Bytes())

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write(raw.Bytes()); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	assertSameDigest("gzipped tar", gz.Bytes())

	if _, err := ImageFromReader(strings.NewReader("not a tar archive")); err == nil {
		t.Error("ImageFromReader(garbage) = nil error, want a parse error")
	}

	// Gzip magic (1f 8b) but a truncated/invalid stream exercises the
	// decompress-error branch.
	if _, err := ImageFromReader(bytes.NewReader([]byte{0x1f, 0x8b, 0x08, 0x00, 0x00})); err == nil {
		t.Error("ImageFromReader(bad gzip) = nil error, want a decompress error")
	}
}

func TestRefAndIsHosted(t *testing.T) {
	ref := Ref("team/app", "v3")
	if ref != Host+"/team/app:v3" {
		t.Errorf("Ref = %q", ref)
	}
	if !IsHosted(ref) {
		t.Errorf("IsHosted(%q) = false, want true", ref)
	}
	for _, notHosted := range []string{
		"docker.io/library/alpine:3.19",
		"ghcr.io/owner/repo:tag",
		"127.0.0.1:5050/x:v1",
		"::not a ref::",
	} {
		if IsHosted(notHosted) {
			t.Errorf("IsHosted(%q) = true, want false", notHosted)
		}
	}
}

// saveTarball writes img as a docker-format tarball at path; if gzipIt,
// it gzips the bytes (mirroring the Nix .tar.gz image outputs).
func saveTarball(t *testing.T, path string, gzipIt bool) {
	t.Helper()
	img, err := random.Image(512, 2)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	raw := path
	if gzipIt {
		raw = path + ".raw"
	}
	if err := crane.Save(img, "local/demo:v1", raw); err != nil {
		t.Fatalf("crane.Save: %v", err)
	}
	if !gzipIt {
		return
	}
	in, err := os.ReadFile(raw) //nolint:gosec // test temp path.
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	out, err := os.Create(path) //nolint:gosec // test temp path.
	if err != nil {
		t.Fatalf("create gz: %v", err)
	}
	gz := gzip.NewWriter(out)
	if _, err := gz.Write(in); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close gz: %v", err)
	}
}

func TestPushRoundTrip(t *testing.T) {
	// A TLS registry stands in for the orchestrator's mTLS endpoint; the
	// Transport dials it for the sentinel-host ref. InsecureSkipVerify
	// substitutes for the TOFU peer verification the real mTLS config does.
	srv := httptest.NewTLSServer(registry.New())
	t.Cleanup(srv.Close)
	rt := Transport(srv.Listener.Addr().String(), &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // test stand-in for TOFU mTLS.

	for _, gzipped := range []bool{false, true} {
		label := "raw"
		if gzipped {
			label = "gz"
		}
		t.Run(label, func(t *testing.T) {
			tarPath := filepath.Join(t.TempDir(), "image.tar")
			saveTarball(t, tarPath, gzipped)
			ref, err := Push(tarPath, "test/"+label, "v1", rt)
			if err != nil {
				t.Fatalf("Push: %v", err)
			}
			if !IsHosted(ref) {
				t.Errorf("pushed ref %q is not hosted", ref)
			}
			if _, err := crane.Pull(ref, crane.WithTransport(rt)); err != nil {
				t.Fatalf("pull back %q: %v", ref, err)
			}
		})
	}
}

func TestPushCorruptTarball(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.tar")
	if err := os.WriteFile(path, []byte("this is not a tar archive"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	rt := Transport("127.0.0.1:1", &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // load fails before any dial.
	if _, err := Push(path, "x", "v1", rt); err == nil {
		t.Fatal("expected an error loading a non-tar file")
	}
}

func TestPushMissingTarball(t *testing.T) {
	rt := Transport("127.0.0.1:1", &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // unused; load fails first.
	if _, err := Push(filepath.Join(t.TempDir(), "nope.tar"), "x", "v1", rt); err == nil {
		t.Fatal("expected an error loading a missing tarball")
	}
}
