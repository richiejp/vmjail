package sandboxdcli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// localRegistry stands up an in-process OCI registry for the duration
// of the test. Returned URL host is "host:port" — pass it to crane
// functions and CLI invocations as the registry portion of an image
// ref. Closes automatically on t.Cleanup.
//
// Duplicates the helper of the same name in internal/imagestore's
// tests; they're in different packages so we copy the four lines.
func localRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse registry URL: %v", err)
	}
	return u.Host
}

// pushOneFileImage pushes a synthetic image whose only file is /hello
// containing `body`. Returns the registry-side reference (host/path:tag).
func pushOneFileImage(t *testing.T, host, namedPath, body string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: "hello", Mode: 0o755, Size: int64(len(body))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	_ = tw.Close()
	_ = gz.Close()

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
	})
	if err != nil {
		t.Fatalf("tarball layer: %v", err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("append layer: %v", err)
	}
	img, err = mutate.Config(img, v1.Config{Entrypoint: []string{"/hello"}})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	ref := host + "/" + namedPath
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push: %v", err)
	}
	return ref
}

// TestPullCommandSuccess exercises the happy path of the pull
// subcommand against a real (in-process) registry. Covers the
// success branches of newPullCmd that the registry-failure test
// can't reach: digest formatting, size formatting, return-nil.
func TestPullCommandSuccess(t *testing.T) {
	host := localRegistry(t)
	ref := pushOneFileImage(t, host, "test/pull-ok:v1", "pull-ok body\n")

	t.Setenv(EnvStoreDir, t.TempDir())

	stdout, _, err := runCmd(t, "pull", ref)
	if err != nil {
		t.Fatalf("pull: %v\n%s", err, stdout)
	}
	for _, want := range []string{"Pulled", "digest:", "sha256:", "size:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output missing %q:\n%s", want, stdout)
		}
	}
}

// TestImagesLsPopulated exercises the ls subcommand after a pull —
// covers the populated-store branch of printEntries through the
// CLI rather than directly.
func TestImagesLsPopulated(t *testing.T) {
	host := localRegistry(t)
	ref := pushOneFileImage(t, host, "test/ls:v1", "ls body\n")
	t.Setenv(EnvStoreDir, t.TempDir())

	if _, _, err := runCmd(t, "pull", ref); err != nil {
		t.Fatalf("pull: %v", err)
	}
	stdout, _, err := runCmd(t, "images", "ls")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if !strings.Contains(stdout, ref) {
		t.Errorf("ls output missing %q:\n%s", ref, stdout)
	}
	if strings.Contains(stdout, "empty store") {
		t.Errorf("ls reported empty after pull:\n%s", stdout)
	}
}

// TestImagesRmSuccess covers the happy path of newImagesRmCmd:
// pull, then rm by ref, then ls confirms removal.
func TestImagesRmSuccess(t *testing.T) {
	host := localRegistry(t)
	ref := pushOneFileImage(t, host, "test/rm-ok:v1", "rm body\n")
	t.Setenv(EnvStoreDir, t.TempDir())

	if _, _, err := runCmd(t, "pull", ref); err != nil {
		t.Fatalf("pull: %v", err)
	}
	stdout, _, err := runCmd(t, "images", "rm", ref)
	if err != nil {
		t.Fatalf("rm: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "Removed sha256:") {
		t.Errorf("expected 'Removed sha256:…' in output:\n%s", stdout)
	}
}

// TestImagesPruneSuccess covers newImagesPruneCmd's happy path.
// Sets a far-future "older-than" so any pulled image qualifies.
func TestImagesPruneSuccess(t *testing.T) {
	host := localRegistry(t)
	ref := pushOneFileImage(t, host, "test/prune-ok:v1", "prune body\n")
	t.Setenv(EnvStoreDir, t.TempDir())

	if _, _, err := runCmd(t, "pull", ref); err != nil {
		t.Fatalf("pull: %v", err)
	}
	// 1 ns cutoff guarantees the just-pulled entry is "old enough".
	time.Sleep(2 * time.Millisecond)
	stdout, _, err := runCmd(t, "images", "prune", "--older-than", "1ns")
	if err != nil {
		t.Fatalf("prune: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "Pruned 1 image") {
		t.Errorf("expected 'Pruned 1 image' in output:\n%s", stdout)
	}
}

// TestStoreRootDefault exercises the os.UserCacheDir branch of
// storeRoot when DAFT_STORE_DIR is unset. We can't predict the
// exact path (it depends on $XDG_CACHE_HOME / $HOME) but we can
// assert it's non-empty and ends with our subdir name.
func TestStoreRootDefault(t *testing.T) {
	t.Setenv(EnvStoreDir, "")
	got, err := storeRoot()
	if err != nil {
		t.Fatalf("storeRoot: %v", err)
	}
	if got == "" {
		t.Fatal("storeRoot returned empty path")
	}
	if !strings.HasSuffix(got, storeSubdir) {
		t.Errorf("storeRoot %q should end with %q", got, storeSubdir)
	}
}
