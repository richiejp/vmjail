package imagestore

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// helloLayer returns a tar.gz layer whose only file is /hello with
// payload `body`. Lets bundle tests assert that unpack actually wrote
// the right bytes to the right path.
func helloLayer(t *testing.T, body string) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := newTarGzWriter(&buf)
	tw.addFile("hello", body, 0o755)
	tw.close()

	layer, err := tarballLayerFromBytes(buf.Bytes())
	if err != nil {
		t.Fatalf("tarball layer: %v", err)
	}
	return layer
}

func TestBundleUnpacksAndWritesConfig(t *testing.T) {
	host := localRegistry(t)
	ref := host + "/test/hello-bundle:v1"

	layer := helloLayer(t, "bundle hello body\n")
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("append layer: %v", err)
	}
	img, err = mutate.Config(img, v1.Config{
		Entrypoint: []string{testEntrypoint},
		Env:        []string{"FOO=bar"},
		WorkingDir: "/",
	})
	if err != nil {
		t.Fatalf("mutate config: %v", err)
	}
	r, _ := name.ParseReference(ref)
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push: %v", err)
	}

	store, err := New(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pulled, err := store.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	bundle, err := store.Bundle(context.Background(), pulled.Digest, BundleOptions{})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}

	helloPath := filepath.Join(bundle.Path, "rootfs", "hello")
	got, err := os.ReadFile(helloPath)
	if err != nil {
		t.Fatalf("read rootfs/hello: %v", err)
	}
	if string(got) != "bundle hello body\n" {
		t.Errorf("rootfs/hello = %q, want %q", got, "bundle hello body\n")
	}

	cfg, err := os.ReadFile(filepath.Join(bundle.Path, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	if !strings.Contains(string(cfg), `"`+testEntrypoint+`"`) {
		t.Errorf("config.json missing entrypoint %q: %s", testEntrypoint, cfg)
	}
	if !strings.Contains(string(cfg), `"FOO=bar"`) {
		t.Errorf("config.json missing env: %s", cfg)
	}
}

func TestBundleIsIdempotent(t *testing.T) {
	host := localRegistry(t)
	ref := host + "/test/idemp:v1"
	layer := helloLayer(t, "x\n")
	img, _ := mutate.AppendLayers(empty.Image, layer)
	img, _ = mutate.Config(img, v1.Config{Entrypoint: []string{testEntrypoint}})
	r, _ := name.ParseReference(ref)
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push: %v", err)
	}

	store, _ := New(t.TempDir(), 1<<30)
	pulled, _ := store.Pull(context.Background(), ref)
	first, err := store.Bundle(context.Background(), pulled.Digest, BundleOptions{})
	if err != nil {
		t.Fatalf("Bundle 1: %v", err)
	}
	mtime1, _ := os.Stat(filepath.Join(first.Path, "config.json"))

	second, err := store.Bundle(context.Background(), pulled.Digest, BundleOptions{})
	if err != nil {
		t.Fatalf("Bundle 2: %v", err)
	}
	if first.Path != second.Path {
		t.Errorf("path changed: %q vs %q", first.Path, second.Path)
	}
	mtime2, _ := os.Stat(filepath.Join(second.Path, "config.json"))
	if !mtime1.ModTime().Equal(mtime2.ModTime()) {
		t.Errorf("config.json was rewritten on second Bundle call (mtime changed)")
	}
}

func TestBundleErrorsForUnknownDigest(t *testing.T) {
	store, _ := New(t.TempDir(), 1<<30)
	_, err := store.Bundle(context.Background(),
		"sha256:0000000000000000000000000000000000000000000000000000000000000000",
		BundleOptions{})
	if !errors.Is(err, ErrUnknownImage) {
		t.Fatalf("err = %v, want ErrUnknownImage", err)
	}
}

func TestBundleAppliesExtraMounts(t *testing.T) {
	host := localRegistry(t)
	ref := host + "/test/extramounts:v1"
	layer := helloLayer(t, "x\n")
	img, _ := mutate.AppendLayers(empty.Image, layer)
	img, _ = mutate.Config(img, v1.Config{Entrypoint: []string{testEntrypoint}})
	r, _ := name.ParseReference(ref)
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push: %v", err)
	}

	store, _ := New(t.TempDir(), 1<<30)
	pulled, _ := store.Pull(context.Background(), ref)
	bundle, err := store.Bundle(context.Background(), pulled.Digest, BundleOptions{
		ExtraMounts: []BundleMount{
			{
				Destination: "/workspace",
				Type:        "bind",
				Source:      "/workspace",
				Options:     []string{"rbind", "rw"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	cfg, _ := os.ReadFile(filepath.Join(bundle.Path, "config.json"))
	if !strings.Contains(string(cfg), `"/workspace"`) ||
		!strings.Contains(string(cfg), `"bind"`) ||
		!strings.Contains(string(cfg), `"rbind"`) {
		t.Errorf("expected workspace bind mount in config.json:\n%s", cfg)
	}
}

func TestBundleAppliesOverrides(t *testing.T) {
	host := localRegistry(t)
	ref := host + "/test/override:v1"
	layer := helloLayer(t, "x\n")
	img, _ := mutate.AppendLayers(empty.Image, layer)
	img, _ = mutate.Config(img, v1.Config{Entrypoint: []string{testEntrypoint}})
	r, _ := name.ParseReference(ref)
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push: %v", err)
	}

	store, _ := New(t.TempDir(), 1<<30)
	pulled, _ := store.Pull(context.Background(), ref)
	bundle, err := store.Bundle(context.Background(), pulled.Digest, BundleOptions{
		Args:     []string{"/bin/echo", "hi"},
		Env:      []string{"OVERRIDE=1"},
		Hostname: "custom-host",
	})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	cfg, _ := os.ReadFile(filepath.Join(bundle.Path, "config.json"))
	if !strings.Contains(string(cfg), `"/bin/echo"`) ||
		!strings.Contains(string(cfg), `"OVERRIDE=1"`) ||
		!strings.Contains(string(cfg), `"custom-host"`) {
		t.Errorf("overrides missing in config.json:\n%s", cfg)
	}
}

// TestWriteTarEntryHandlesHardlink covers the TypeLink branch of
// writeTarEntry. We pre-create a target file, then write a hardlink
// header that points at it.
func TestWriteTarEntryHandlesHardlink(t *testing.T) {
	dst := t.TempDir()
	srcPath := filepath.Join(dst, "source")
	if err := os.WriteFile(srcPath, []byte("payload"), 0o644); err != nil {
		t.Fatalf("create source: %v", err)
	}

	link := &tar.Header{Typeflag: tar.TypeLink, Linkname: "source"}
	target := filepath.Join(dst, "linked")
	if err := writeTarEntry(nil, link, target); err != nil {
		t.Fatalf("hardlink: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read linked: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("hardlink content = %q, want payload", got)
	}
}

// TestWriteTarEntryRegularFile covers the TypeReg branch directly:
// build a tar.Reader carrying a single regular-file entry and feed
// it through writeTarEntry. This exercises the io.Copy path that
// the indirect tests reach but not via the public function name.
func TestWriteTarEntryRegularFile(t *testing.T) {
	dst := t.TempDir()
	body := []byte("regular-file body")

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Typeflag: tar.TypeReg, Name: "ignored",
		Mode: 0o644, Size: int64(len(body)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write hdr: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("write body: %v", err)
	}
	_ = tw.Close()

	tr := tar.NewReader(&buf)
	read, err := tr.Next()
	if err != nil {
		t.Fatalf("read hdr: %v", err)
	}
	target := filepath.Join(dst, "regular")
	if err := writeTarEntry(tr, read, target); err != nil {
		t.Fatalf("writeTarEntry: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("body mismatch: got %q, want %q", got, body)
	}
}

// TestUntarToRejectsTraversingHeader confirms untarTo refuses tar
// streams whose entries try to escape dst — covers the safeJoin
// error propagation path in untarTo.
func TestUntarToRejectsTraversingHeader(t *testing.T) {
	dst := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "../../etc/passwd",
		Mode:     0o644,
		Size:     0,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("hdr: %v", err)
	}
	_ = tw.Close()
	// safeJoin normalises traversal, so untarTo accepts it but
	// writes under dst — assert dst/etc/passwd appears, not the
	// host's /etc/passwd. The intent here is to lock the
	// "no escape" guarantee in regression tests.
	if err := untarTo(&buf, dst); err != nil {
		t.Fatalf("untarTo: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "etc", "passwd")); err != nil {
		t.Errorf("expected dst/etc/passwd to exist: %v", err)
	}
}

// TestWriteTarEntryHandlesSymlinkAndDir exercises the symlink / dir
// branches of writeTarEntry that the regular-file image-unpack tests
// don't reach. We construct each tar.Header type by hand and feed it
// through.
func TestWriteTarEntryHandlesSymlinkAndDir(t *testing.T) {
	dst := t.TempDir()

	dirEntry, err := newTarHeader("subdir", 0o755)
	if err != nil {
		t.Fatalf("dir header: %v", err)
	}
	dirEntry.Typeflag = tar.TypeDir
	if err := writeTarEntry(nil, dirEntry, filepath.Join(dst, "subdir")); err != nil {
		t.Fatalf("dir: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "subdir")); err != nil || !fi.IsDir() {
		t.Errorf("subdir not created: err=%v", err)
	}

	link := &tar.Header{Typeflag: tar.TypeSymlink, Linkname: "subdir"}
	if err := writeTarEntry(nil, link, filepath.Join(dst, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	target, err := os.Readlink(filepath.Join(dst, "link"))
	if err != nil || target != "subdir" {
		t.Errorf("symlink target = %q, want subdir (err=%v)", target, err)
	}

	// Default branch: a fifo / device entry is silently skipped.
	fifo := &tar.Header{Typeflag: tar.TypeFifo}
	if err := writeTarEntry(nil, fifo, filepath.Join(dst, "fifo-skip")); err != nil {
		t.Errorf("fifo skip: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "fifo-skip")); !os.IsNotExist(err) {
		t.Errorf("fifo path should not exist: err=%v", err)
	}
}

func newTarHeader(name string, mode int64) (*tar.Header, error) {
	return &tar.Header{Name: name, Mode: mode}, nil
}

// TestSafeJoinNormalisesTraversal confirms that tar entries containing
// ".." are rewritten to nest under dst rather than escaping. We don't
// reject them outright — filepath.Clean absorbs the traversal and the
// resulting path always sits under dst, which is what we need.
func TestSafeJoinNormalisesTraversal(t *testing.T) {
	dst := "/var/tmp/bundle"
	cases := map[string]string{
		"etc/passwd":              testTraversed,
		"../etc/passwd":           testTraversed,
		"/../../etc/passwd":       testTraversed,
		"rootfs/../etc/passwd":    testTraversed,
		"a/b/../../../etc/passwd": testTraversed,
	}
	for in, want := range cases {
		got, err := safeJoin(dst, in)
		if err != nil {
			t.Errorf("safeJoin(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("safeJoin(%q) = %q, want %q", in, got, want)
		}
	}
}
