package ociregistry

import (
	"compress/gzip"
	"crypto/tls"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

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
		name := "raw"
		if gzipped {
			name = "gz"
		}
		t.Run(name, func(t *testing.T) {
			tarPath := filepath.Join(t.TempDir(), "image.tar")
			saveTarball(t, tarPath, gzipped)
			ref, err := Push(tarPath, "test/"+name, "v1", rt)
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
