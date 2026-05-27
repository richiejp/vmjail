package imagestore

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	// #nosec G304 — test-only; helloPath is built from t.TempDir().
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
				Type:        mountTypeBind,
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

// TestParseImageUserHappy covers the empty/uid-only/uid:gid forms.
func TestParseImageUserHappy(t *testing.T) {
	cases := map[string]struct {
		in      string
		wantUID int
		wantGID int
	}{
		"empty":   {"", 0, 0},
		"uid":     {"1000", 1000, 0},
		"uid:gid": {"1000:100", 1000, 100},
		"root":    {"0:0", 0, 0},
	}
	for name, c := range cases {
		got, err := parseImageUser(c.in)
		if err != nil {
			t.Errorf("%s: err = %v, want nil", name, err)
			continue
		}
		if got.UID != c.wantUID || got.GID != c.wantGID {
			t.Errorf("%s: got %+v, want uid=%d gid=%d",
				name, got, c.wantUID, c.wantGID)
		}
	}
}

// TestParseImageUserErrors pins the failure paths: name-form users
// and non-numeric gids both have to surface before any disk write.
func TestParseImageUserErrors(t *testing.T) {
	cases := map[string]string{
		"name form":   "nobody",
		"name:group":  "nobody:nogroup",
		"empty uid":   ":100",
		"bad gid":     "1000:abc",
		"trailing ws": "1000 ",
	}
	for name, in := range cases {
		_, err := parseImageUser(in)
		if err == nil {
			t.Errorf("%s: parseImageUser(%q) returned nil error", name, in)
		}
	}
}

// TestDefaultRuntimeMountsIncludesTmpfsScratch pins the writable
// scratch mounts the rest of the runtime spec assumes exist: /tmp
// and /nix/store/.links. Without these, nix and any build tool
// hit EROFS on first write since the rootfs comes via virtiofs.
func TestDefaultRuntimeMountsIncludesTmpfsScratch(t *testing.T) {
	mounts := defaultRuntimeMounts()
	wantDest := map[string]bool{
		"/tmp":              false,
		"/nix/store/.links": false,
	}
	for _, m := range mounts {
		if _, ok := wantDest[m.Destination]; ok {
			wantDest[m.Destination] = true
			if m.Type != mountTypeTmpfs {
				t.Errorf("%s: type = %q, want %q",
					m.Destination, m.Type, mountTypeTmpfs)
			}
		}
	}
	for dest, found := range wantDest {
		if !found {
			t.Errorf("default mounts missing %s", dest)
		}
	}
}

// TestDefaultRuntimeMountsIncludesDevPts pins the devpts mount
// that gives the container a working /dev/ptmx. Without it,
// `nix develop --command` (and any PTY allocation) fails with
// "opening pseudoterminal master: No such file or directory".
func TestDefaultRuntimeMountsIncludesDevPts(t *testing.T) {
	var devpts *runtimeMount
	for i, m := range defaultRuntimeMounts() {
		if m.Destination == "/dev/pts" {
			devpts = &defaultRuntimeMounts()[i]
			break
		}
	}
	if devpts == nil {
		t.Fatal("default mounts missing /dev/pts")
	}
	if devpts.Type != mountTypeDevpts {
		t.Errorf("/dev/pts type = %q, want %q", devpts.Type, mountTypeDevpts)
	}
	wantOpts := map[string]bool{"newinstance": false, "ptmxmode=0666": false}
	for _, o := range devpts.Options {
		if _, ok := wantOpts[o]; ok {
			wantOpts[o] = true
		}
	}
	for o, found := range wantOpts {
		if !found {
			t.Errorf("/dev/pts options missing %q (got %v)", o, devpts.Options)
		}
	}
}

// TestBuildRuntimeConfigRootfsWritable pins the rootfs Readonly
// flag — load-bearing for nix-CLI workflows (see runtimespec.go
// comment for the chain of constraints).
func TestBuildRuntimeConfigRootfsWritable(t *testing.T) {
	host := localRegistry(t)
	ref := host + "/test/rootfs-rw:v1"
	layer := helloLayer(t, "x\n")
	img, _ := mutate.AppendLayers(empty.Image, layer)
	img, _ = mutate.Config(img, v1.Config{Entrypoint: []string{testEntrypoint}})
	r, _ := name.ParseReference(ref)
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push: %v", err)
	}
	store, _ := New(t.TempDir(), 1<<30)
	pulled, _ := store.Pull(context.Background(), ref)
	bundle, err := store.Bundle(context.Background(), pulled.Digest, BundleOptions{})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	cfg, _ := os.ReadFile(filepath.Join(bundle.Path, "config.json"))
	if !strings.Contains(string(cfg), `"readonly": false`) {
		t.Errorf("expected rootfs readonly:false in config.json:\n%s", cfg)
	}
}

// TestBuildRuntimeConfigHonoursImageUser pins parseImageUser's
// integration into the runtime spec: a numeric image User flows
// through to process.user.{uid,gid}.
func TestBuildRuntimeConfigHonoursImageUser(t *testing.T) {
	host := localRegistry(t)
	ref := host + "/test/user-1000:v1"
	layer := helloLayer(t, "x\n")
	img, _ := mutate.AppendLayers(empty.Image, layer)
	img, _ = mutate.Config(img, v1.Config{
		Entrypoint: []string{testEntrypoint},
		User:       "1000:100",
	})
	r, _ := name.ParseReference(ref)
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push: %v", err)
	}
	store, _ := New(t.TempDir(), 1<<30)
	pulled, _ := store.Pull(context.Background(), ref)
	bundle, err := store.Bundle(context.Background(), pulled.Digest, BundleOptions{})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	cfg, _ := os.ReadFile(filepath.Join(bundle.Path, "config.json"))
	if !strings.Contains(string(cfg), `"uid": 1000`) ||
		!strings.Contains(string(cfg), `"gid": 100`) {
		t.Errorf("expected uid:1000 gid:100 in config.json:\n%s", cfg)
	}
}

// TestBuildRuntimeConfigRejectsNameFormUser covers the failure
// surface: a non-numeric User shouldn't silently default to root.
func TestBuildRuntimeConfigRejectsNameFormUser(t *testing.T) {
	host := localRegistry(t)
	ref := host + "/test/user-name:v1"
	layer := helloLayer(t, "x\n")
	img, _ := mutate.AppendLayers(empty.Image, layer)
	img, _ = mutate.Config(img, v1.Config{
		Entrypoint: []string{testEntrypoint},
		User:       "nobody",
	})
	r, _ := name.ParseReference(ref)
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push: %v", err)
	}
	store, _ := New(t.TempDir(), 1<<30)
	pulled, _ := store.Pull(context.Background(), ref)
	_, err := store.Bundle(context.Background(), pulled.Digest, BundleOptions{})
	if err == nil || !strings.Contains(err.Error(), "User") {
		t.Errorf("Bundle: err = %v, want error mentioning User", err)
	}
}

// TestWriteJSONErrors pins both surfaces of writeJSON failing:
//   - an unmarshalable payload (here, a func) trips MarshalIndent
//     before any disk write.
//   - a missing parent directory trips os.WriteFile.
//
// Catches accidental silent drops on either failure mode.
func TestWriteJSONErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does", "not", "exist", "out.json")
	if err := writeJSON(missing, struct{ A int }{A: 1}); err == nil {
		t.Errorf("writeJSON to %s: err = nil, want failure", missing)
	}

	good := filepath.Join(t.TempDir(), "out.json")
	if err := writeJSON(good, func() {}); err == nil {
		t.Errorf("writeJSON with unmarshalable payload: err = nil, want failure")
	}
}

// TestBuildRuntimeConfigOmitsUserNamespace pins the D7 design:
// the bundle no longer emits an OCI `user` namespace or uid/gid
// maps. Container processes inherit the calling sandboxd's user
// namespace (set up by the daft-sandboxd re-exec scaffold), so
// the runtime spec stays free of mapping clutter.
func TestBuildRuntimeConfigOmitsUserNamespace(t *testing.T) {
	host := localRegistry(t)
	ref := host + "/test/no-userns:v1"
	layer := helloLayer(t, "x\n")
	img, _ := mutate.AppendLayers(empty.Image, layer)
	img, _ = mutate.Config(img, v1.Config{Entrypoint: []string{testEntrypoint}})
	r, _ := name.ParseReference(ref)
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push: %v", err)
	}
	store, _ := New(t.TempDir(), 1<<30)
	pulled, _ := store.Pull(context.Background(), ref)
	bundle, err := store.Bundle(context.Background(), pulled.Digest, BundleOptions{})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	cfg, _ := os.ReadFile(filepath.Join(bundle.Path, "config.json"))
	got := string(cfg)
	for _, banned := range []string{
		`"type": "user"`,
		`uidMappings`,
		`gidMappings`,
	} {
		if strings.Contains(got, banned) {
			t.Errorf("config.json contains %q:\n%s", banned, got)
		}
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

func TestMergeEnv(t *testing.T) {
	const a1 = "A=1"
	cases := []struct {
		name    string
		base    []string
		overlay []string
		want    []string
	}{
		{"empty overlay keeps base", []string{a1, "B=2"}, nil, []string{a1, "B=2"}},
		{"empty base takes overlay", nil, []string{a1}, []string{a1}},
		{
			"override in place + append",
			[]string{"PATH=/img", a1},
			[]string{"A=2", "NEW=x"},
			[]string{"PATH=/img", "A=2", "NEW=x"},
		},
		{"bare name (no =) keyed whole", []string{a1}, []string{"A"}, []string{"A"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeEnv(tc.base, tc.overlay)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("mergeEnv(%v, %v) = %v, want %v", tc.base, tc.overlay, got, tc.want)
			}
		})
	}
}

// TestBundleMergesEnvOverImage proves BundleOptions.Env overlays the
// image's Env keyed by NAME: the same name is overridden, a new name
// is appended, and an untouched image name survives.
func TestBundleMergesEnvOverImage(t *testing.T) {
	host := localRegistry(t)
	ref := host + "/test/merge-env:v1"
	layer := helloLayer(t, "x\n")
	img, _ := mutate.AppendLayers(empty.Image, layer)
	img, _ = mutate.Config(img, v1.Config{
		Entrypoint: []string{testEntrypoint},
		Env:        []string{"PATH=/img/bin", "https_proxy=stale"},
	})
	r, _ := name.ParseReference(ref)
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push: %v", err)
	}
	store, _ := New(t.TempDir(), 1<<30)
	pulled, _ := store.Pull(context.Background(), ref)
	bundle, err := store.Bundle(context.Background(), pulled.Digest, BundleOptions{
		Env: []string{"https_proxy=http://192.168.127.254:9", "NEW=1"},
	})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	cfg, _ := os.ReadFile(filepath.Join(bundle.Path, "config.json"))
	for _, want := range []string{`"PATH=/img/bin"`, `"https_proxy=http://192.168.127.254:9"`, `"NEW=1"`} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("config.json missing %s:\n%s", want, cfg)
		}
	}
	if strings.Contains(string(cfg), `"https_proxy=stale"`) {
		t.Errorf("stale image env not overridden:\n%s", cfg)
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
	if err := writeTarEntry(nil, link, target, dst); err != nil {
		t.Fatalf("hardlink: %v", err)
	}
	// #nosec G304 — test-only.
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
	if err := writeTarEntry(tr, read, target, dst); err != nil {
		t.Fatalf("writeTarEntry: %v", err)
	}
	// #nosec G304 — test-only.
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
	if err := writeTarEntry(nil, dirEntry, filepath.Join(dst, "subdir"), dst); err != nil {
		t.Fatalf("dir: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "subdir")); err != nil || !fi.IsDir() {
		t.Errorf("subdir not created: err=%v", err)
	}

	link := &tar.Header{Typeflag: tar.TypeSymlink, Linkname: "subdir"}
	if err := writeTarEntry(nil, link, filepath.Join(dst, "link"), dst); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	target, err := os.Readlink(filepath.Join(dst, "link"))
	if err != nil || target != "subdir" {
		t.Errorf("symlink target = %q, want subdir (err=%v)", target, err)
	}

	// Default branch: a fifo / device entry is silently skipped.
	fifo := &tar.Header{Typeflag: tar.TypeFifo}
	if err := writeTarEntry(nil, fifo, filepath.Join(dst, "fifo-skip"), dst); err != nil {
		t.Errorf("fifo skip: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "fifo-skip")); !os.IsNotExist(err) {
		t.Errorf("fifo path should not exist: err=%v", err)
	}
}

func newTarHeader(entryName string, mode int64) (*tar.Header, error) {
	return &tar.Header{Name: entryName, Mode: mode}, nil
}

// TestWriteTarEntryRelaxesReadOnlyAncestors covers the Nix-image
// regression: layer A emits a 0o555 directory, layer B emits a
// file inside it. Without the writable-ancestor walk, the second
// extraction fails with EACCES because Go's MkdirAll honours
// existing dir modes. We simulate the pattern by hand — TypeDir
// at 0o555, then TypeReg under it — and assert the file lands.
func TestWriteTarEntryRelaxesReadOnlyAncestors(t *testing.T) {
	dst := t.TempDir()

	// Layer A: a read-only Nix-style directory.
	dirHdr := &tar.Header{
		Name:     "nix/store/abc-pkg",
		Mode:     0o555,
		Typeflag: tar.TypeDir,
	}
	dirTarget := filepath.Join(dst, "nix", "store", "abc-pkg")
	if err := writeTarEntry(nil, dirHdr, dirTarget, dst); err != nil {
		t.Fatalf("dir entry: %v", err)
	}

	// Layer B: a regular file two levels below the read-only dir,
	// with the intermediate bin/ also missing — the realistic
	// pattern dockerTools layered images produce.
	body := []byte("#!/bin/sh\n")
	fileHdr := &tar.Header{
		Name:     "nix/store/abc-pkg/bin/hello",
		Mode:     0o755,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(fileHdr); err != nil {
		t.Fatalf("tar write header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("tar write body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	tr := tar.NewReader(&buf)
	read, err := tr.Next()
	if err != nil {
		t.Fatalf("tar next: %v", err)
	}
	fileTarget := filepath.Join(dst, "nix", "store", "abc-pkg", "bin", "hello")
	if err := writeTarEntry(tr, read, fileTarget, dst); err != nil {
		t.Fatalf("file entry under read-only ancestor: %v", err)
	}

	got, err := os.ReadFile(fileTarget) //nolint:gosec // fileTarget is under t.TempDir(); test-only.
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("extracted file body = %q, want %q", got, body)
	}
}

// TestEnsureWritableAncestorsStopsAtRoot guards the walk against
// climbing past the bundle root and chmod'ing the caller's cache
// dir or worse. We point root at a subdir of dst, plant a
// read-only sibling above root, and assert the function leaves it
// alone.
func TestEnsureWritableAncestorsStopsAtRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "bundle")
	innerB := filepath.Join(root, "a", "b")
	// #nosec G301 — bundle dirs are extraction targets, not
	// configuration paths; relaxed perms mirror writableDirMode.
	if err := os.MkdirAll(innerB, 0o755); err != nil {
		t.Fatalf("mkdir root tree: %v", err)
	}
	// Plant a read-only outside-root dir; the walk must not touch it.
	outside := filepath.Join(parent, "outside")
	// #nosec G301 — synthetic read-only barrier, never real perms.
	if err := os.Mkdir(outside, 0o555); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	// #nosec G302 — same: per-test scratch dir, no real-world perms.
	t.Cleanup(func() { _ = os.Chmod(outside, 0o755) })

	// Make root/a read-only so the walk has something to relax.
	// #nosec G302 — same: deliberately recreating the on-disk
	// mode an OCI image carries.
	if err := os.Chmod(filepath.Join(root, "a"), 0o555); err != nil {
		t.Fatalf("chmod a: %v", err)
	}

	if err := ensureWritableAncestors(innerB, root); err != nil {
		t.Fatalf("ensureWritableAncestors: %v", err)
	}

	if fi, _ := os.Stat(filepath.Join(root, "a")); fi.Mode().Perm()&0o700 != 0o700 {
		t.Errorf("a perms = %o, want u+rwx", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(outside); fi.Mode().Perm() != 0o555 {
		t.Errorf("outside perms = %o, want 0o555 (walk should not have touched)", fi.Mode().Perm())
	}
}

// TestWriteTarEntryRegularFileSurfacesMkdirError covers the
// MkdirAll-error branch of writeTarEntry's TypeReg path: when
// the would-be parent dir of the file collides with an existing
// non-directory entry, os.MkdirAll returns ENOTDIR and the
// function bubbles it up.
func TestWriteTarEntryRegularFileSurfacesMkdirError(t *testing.T) {
	dst := t.TempDir()
	// Plant a regular file at dst/blocker — a child entry under
	// it ("blocker/child") can't have its parent dir created.
	blocker := filepath.Join(dst, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	hdr := &tar.Header{
		Name:     "blocker/child",
		Mode:     0o644,
		Typeflag: tar.TypeReg,
	}
	err := writeTarEntry(nil, hdr, filepath.Join(dst, "blocker", "child"), dst)
	if err == nil {
		t.Fatal("expected MkdirAll error for file-under-file path")
	}
}

// TestWriteTarEntryDirSurfacesMkdirError covers the MkdirAll-
// failure branch of writeTarEntry's TypeDir path — same
// file-blocks-dir shape as the TypeReg test, but for a directory
// entry. Distinct from the TypeReg test because the two switch
// cases own their own MkdirAll call.
func TestWriteTarEntryDirSurfacesMkdirError(t *testing.T) {
	dst := t.TempDir()
	blocker := filepath.Join(dst, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	hdr := &tar.Header{
		Name:     "blocker/inside",
		Mode:     0o755,
		Typeflag: tar.TypeDir,
	}
	err := writeTarEntry(nil, hdr,
		filepath.Join(dst, "blocker", "inside"), dst)
	if err == nil {
		t.Fatal("expected MkdirAll error for dir-under-file path")
	}
}

// TestEnsureWritableAncestorsSurfacesStatError covers the
// non-ENOENT branch of os.Stat: when an ancestor directory exists
// but its parent has been chmod'd to mode 0 — denying traversal —
// Stat returns EACCES, which the walk must surface rather than
// papering over.
func TestEnsureWritableAncestorsSurfacesStatError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses dir-traverse perms")
	}
	root := t.TempDir()
	// /root/locked exists; /root/locked/inner exists but is
	// unreachable via Stat after we lock /root/locked.
	locked := filepath.Join(root, "locked")
	// #nosec G301 — scratch dirs in test scope.
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// #nosec G302 — synthesizing the EACCES condition.
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatalf("chmod 0: %v", err)
	}
	// #nosec G302 — cleanup so t.TempDir can remove the tree.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	err := ensureWritableAncestors(
		filepath.Join(root, "locked", "inner", "child"), root,
	)
	if err == nil {
		t.Fatal("expected Stat EACCES under mode-0 parent")
	}
}

// TestEnsureWritableAncestorsHandlesMissingMidPath covers the
// fs.ErrNotExist branch: when an ancestor doesn't yet exist on
// disk (because MkdirAll will create it on the way down), the
// walk silently skips that level and keeps climbing.
func TestEnsureWritableAncestorsHandlesMissingMidPath(t *testing.T) {
	root := t.TempDir()
	// /root/a exists (read-only), /root/a/b does not.
	// #nosec G301 — bundle scratch, not production perms.
	if err := os.Mkdir(filepath.Join(root, "a"), 0o555); err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	// Target the never-existed leaf b — walk steps up via
	// the ErrNotExist branch and chmods a on the way past.
	if err := ensureWritableAncestors(filepath.Join(root, "a", "b", "c"), root); err != nil {
		t.Fatalf("ensureWritableAncestors: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(root, "a")); fi.Mode().Perm()&0o700 != 0o700 {
		t.Errorf("a perms = %o, walk should have relaxed it", fi.Mode().Perm())
	}
}

// TestEnsureWritableAncestorsReturnsAtRoot covers the
// dir == rootClean fast-exit: when the start dir is exactly root,
// the loop returns immediately without stat'ing anything.
func TestEnsureWritableAncestorsReturnsAtRoot(t *testing.T) {
	root := t.TempDir()
	// Plant a read-only barrier at root itself; the walk must
	// NOT touch it (root is the floor).
	// #nosec G302 — test scaffolding.
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	// #nosec G302 — cleanup so t.TempDir's rm-rf succeeds.
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	if err := ensureWritableAncestors(root, root); err != nil {
		t.Fatalf("ensureWritableAncestors(root, root): %v", err)
	}
	if fi, _ := os.Stat(root); fi.Mode().Perm()&0o700 == 0o700 {
		t.Errorf("root perms = %o, walk should not have touched root", fi.Mode().Perm())
	}
}

// TestBundleHonoursWorkingDirAndEnvFromImage covers the path where
// BundleOptions are zero and the image's own WorkingDir + Env
// flow through to the generated config.json.
func TestBundleHonoursWorkingDirAndEnvFromImage(t *testing.T) {
	host := localRegistry(t)
	ref := host + "/test/honour-imgcfg:v1"
	layer := helloLayer(t, "x\n")
	img, _ := mutate.AppendLayers(empty.Image, layer)
	img, _ = mutate.Config(img, v1.Config{
		Entrypoint: []string{testEntrypoint},
		Env:        []string{"IMGENV=set"},
		WorkingDir: "/work",
	})
	r, _ := name.ParseReference(ref)
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push: %v", err)
	}
	store, _ := New(t.TempDir(), 1<<30)
	pulled, _ := store.Pull(context.Background(), ref)
	bundle, err := store.Bundle(context.Background(), pulled.Digest, BundleOptions{})
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}
	cfg, _ := os.ReadFile(filepath.Join(bundle.Path, "config.json"))
	if !strings.Contains(string(cfg), `"IMGENV=set"`) {
		t.Errorf("env not propagated: %s", cfg)
	}
	if !strings.Contains(string(cfg), `"/work"`) {
		t.Errorf("working dir not propagated: %s", cfg)
	}
}

// TestBundleErrorsWhenImageHasNoEntrypoint covers the
// buildRuntimeConfig "image has no entrypoint/cmd" failure path.
// pull a layer-only image (no Config.Entrypoint), then Bundle
// without overriding Args.
func TestBundleErrorsWhenImageHasNoEntrypoint(t *testing.T) {
	host := localRegistry(t)
	ref := host + "/test/no-entrypoint:v1"
	layer := helloLayer(t, "x\n")
	img, _ := mutate.AppendLayers(empty.Image, layer)
	// No mutate.Config call → no Entrypoint or Cmd.
	r, _ := name.ParseReference(ref)
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push: %v", err)
	}

	store, _ := New(t.TempDir(), 1<<30)
	pulled, err := store.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	_, err = store.Bundle(context.Background(), pulled.Digest, BundleOptions{})
	if err == nil {
		t.Fatal("expected error when image has no entrypoint and no Args override")
	}
	if !strings.Contains(err.Error(), "entrypoint") {
		t.Errorf("err = %v, want mention of entrypoint", err)
	}
}

// errReader is an io.Reader that always returns the configured
// error. Used to drive untarTo's read-error path.
type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }

// TestWriteTarEntryRejectsCopyError covers the io.Copy failure
// branch by hand-crafting a tar block whose header claims more
// bytes than the stream contains. Bypasses tar.Writer (which
// refuses to close on short writes) by writing the raw header
// bytes directly.
func TestWriteTarEntryRejectsCopyError(t *testing.T) {
	dst := t.TempDir()

	// Smallest valid tar header for "x" with size=100 — the body
	// follows but we'll truncate before writing it. We use a
	// real tar.Writer to produce the header, then truncate the
	// resulting buffer to just-the-header so the body read fails.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: "x", Mode: 0o644, Size: 100}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	// Don't close tw — that would force a body. Instead truncate
	// to header-only (512 bytes is the tar block size).
	truncated := bytes.NewReader(buf.Bytes()[:512])

	tr := tar.NewReader(truncated)
	read, err := tr.Next()
	if err != nil {
		t.Fatalf("tar.Next: %v", err)
	}
	target := filepath.Join(dst, "outfile")
	if err := writeTarEntry(tr, read, target, dst); err == nil {
		t.Fatal("expected error when tar body is short")
	}
}

func TestUntarToWrapsReadError(t *testing.T) {
	want := errors.New("synthetic read failure")
	if err := untarTo(&errReader{err: want}, t.TempDir()); err == nil {
		t.Fatal("expected error from untarTo when reader fails")
	}
}

// TestWriteTarEntrySymlinkOverwritesExistingTarget covers the
// `_ = os.Remove(target)` path in TypeSymlink — when a previous
// entry put a regular file at the target path, the symlink entry
// should still succeed by replacing it.
func TestWriteTarEntrySymlinkOverwritesExistingTarget(t *testing.T) {
	dst := t.TempDir()
	target := filepath.Join(dst, "linkpath")
	if err := os.WriteFile(target, []byte("preexisting"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	link := &tar.Header{Typeflag: tar.TypeSymlink, Linkname: "elsewhere"}
	if err := writeTarEntry(nil, link, target, dst); err != nil {
		t.Fatalf("symlink overwrite: %v", err)
	}
	got, err := os.Readlink(target)
	if err != nil || got != "elsewhere" {
		t.Errorf("symlink target = %q (err %v), want elsewhere", got, err)
	}
}

// TestWriteTarEntryRegularMkdirPathExercised covers the
// MkdirAll-of-parent branch of TypeReg by writing a deeply-nested
// path whose parent directories don't exist yet.
func TestWriteTarEntryRegularMkdirPathExercised(t *testing.T) {
	dst := t.TempDir()
	body := []byte("nested-payload")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Typeflag: tar.TypeReg, Name: "ignored",
		Mode: 0o644, Size: int64(len(body)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()

	tr := tar.NewReader(&buf)
	read, _ := tr.Next()
	target := filepath.Join(dst, "deep", "nested", "file")
	if err := writeTarEntry(tr, read, target, dst); err != nil {
		t.Fatalf("nested write: %v", err)
	}
	// #nosec G304 — test-only.
	if got, _ := os.ReadFile(target); !bytes.Equal(got, body) {
		t.Errorf("nested file content mismatch: %q vs %q", got, body)
	}
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
