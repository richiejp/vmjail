package sandboxboot

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/agentcredential"
	"github.com/richiejp/darkaifactory/internal/bundled"
	"github.com/richiejp/darkaifactory/internal/imagestore"
	"github.com/richiejp/darkaifactory/internal/sandbox"
)

const (
	testDigest = "sha256:abc"

	// Canned paths used across happy-path Deps fakes. Pulled out as
	// constants so the goconst linter doesn't flag the literals.
	testCHPath        = "/ch"
	testKernelPath    = "/kernel"
	testInitramfsPath = "/initramfs"
	testVirtiofsdPath = "/virtiofsd"
	testMountSource   = "/host/ws"
	testMountTarget2  = "/data"
	testMountTarget1  = "/workspace"
	testCacheDir      = "/tmp/cache"
)

// fakeStore is a hand-rolled Store fake. Each method's behaviour
// is a func field so individual tests can flip just one.
type fakeStore struct {
	pull   func(context.Context, string) (*imagestore.Image, error)
	bundle func(context.Context, string, imagestore.BundleOptions) (*imagestore.Bundle, error)
	clone  func(*imagestore.Bundle) (*imagestore.Bundle, func(), error)
	lock   func(string) (func(), error)
}

func TestGuestOnlyWorkspaceMountSkipsVirtiofsButKeepsOCIBind(t *testing.T) {
	mounts := []Mount{
		{Target: testMountTarget1, GuestOnly: true},
		{Target: testMountTarget2, Source: testMountSource, ReadOnly: true},
	}
	if err := validateMounts(mounts); err != nil {
		t.Fatal(err)
	}
	shares := buildShares(&imagestore.Bundle{Path: "/bundles/digest"}, mounts)
	if len(shares) != 2 || shares[1].Mountpoint != testMountTarget2 {
		t.Fatalf("shares = %#v", shares)
	}
	bundleMounts := bundleMountsFromSpec(mounts)
	if len(bundleMounts) != 2 || bundleMounts[0].Destination != testMountTarget1 || bundleMounts[0].Source != testMountTarget1 {
		t.Fatalf("OCI mounts = %#v", bundleMounts)
	}
}

func (f *fakeStore) Pull(ctx context.Context, ref string) (*imagestore.Image, error) {
	return f.pull(ctx, ref)
}

func (f *fakeStore) Bundle(ctx context.Context, digest string, opts imagestore.BundleOptions) (*imagestore.Bundle, error) {
	return f.bundle(ctx, digest, opts)
}

// CloneBundle defaults to identity (return the same bundle, no-op
// cleanup) so tests that don't care about CoW behave as before; a test
// can set f.clone to assert the dispatch uses the clone's path.
func (f *fakeStore) CloneBundle(_ context.Context, b *imagestore.Bundle) (*imagestore.Bundle, func(), error) {
	if f.clone != nil {
		return f.clone(b)
	}
	return b, func() {}, nil
}

func (f *fakeStore) Lock(digest string) (func(), error) {
	return f.lock(digest)
}

// happyPathDeps returns a Deps that emulates a successful boot:
// pull returns a canned image, bundle returns a canned bundle,
// lock returns a counter, cacheDir / extract return canned paths.
// unlockCalls and observed inputs are returned via pointers so the
// caller can assert.
func happyPathDeps(t *testing.T, unlockCalls *int) Deps {
	t.Helper()
	store := &fakeStore{
		pull: func(_ context.Context, ref string) (*imagestore.Image, error) {
			if ref == "" {
				return nil, errors.New("pull saw empty ref")
			}
			return &imagestore.Image{Digest: testDigest}, nil
		},
		bundle: func(_ context.Context, digest string, _ imagestore.BundleOptions) (*imagestore.Bundle, error) {
			return &imagestore.Bundle{Path: "/tmp/bundles/" + digest}, nil
		},
		lock: func(string) (func(), error) {
			return func() { *unlockCalls++ }, nil
		},
	}
	return Deps{
		OpenStore: func() (Store, error) { return store, nil },
		CacheDir:  func() (string, error) { return testCacheDir, nil },
		Extract: func(string) (bundled.Paths, error) {
			return bundled.Paths{
				CloudHypervisor: testCHPath,
				Kernel:          testKernelPath,
				Initramfs:       testInitramfsPath,
				Virtiofsd:       testVirtiofsdPath,
			}, nil
		},
	}
}

// tempBundleDeps is happyPathDeps but with bundle.Path under a real
// temp dir so host-side rootfs writes (the CA cert) land somewhere.
func tempBundleDeps(t *testing.T, bundlePath string) Deps {
	t.Helper()
	store := &fakeStore{
		pull: func(context.Context, string) (*imagestore.Image, error) {
			return &imagestore.Image{Digest: testDigest}, nil
		},
		bundle: func(context.Context, string, imagestore.BundleOptions) (*imagestore.Bundle, error) {
			return &imagestore.Bundle{Path: bundlePath}, nil
		},
		lock: func(string) (func(), error) { return func() {}, nil },
	}
	return Deps{
		OpenStore: func() (Store, error) { return store, nil },
		CacheDir:  func() (string, error) { return t.TempDir(), nil },
		Extract: func(string) (bundled.Paths, error) {
			return bundled.Paths{CloudHypervisor: testCHPath, Kernel: testKernelPath, Initramfs: testInitramfsPath, Virtiofsd: testVirtiofsdPath}, nil
		},
	}
}

func TestBuildBundleConfigWritesCACert(t *testing.T) {
	bundlePath := filepath.Join(t.TempDir(), "bundle")
	deps := tempBundleDeps(t, bundlePath)
	pem := []byte("-----BEGIN CERTIFICATE-----\nxyz\n-----END CERTIFICATE-----\n")
	_, unlock, err := BuildBundleConfig(t.Context(), "alpine:3", BootOptions{
		CACert:          pem,
		CACertGuestPath: "/etc/daft-egress-ca.pem",
	}, deps)
	if err != nil {
		t.Fatalf("BuildBundleConfig: %v", err)
	}
	defer unlock()
	// #nosec G304 — test-only path under t.TempDir().
	got, err := os.ReadFile(filepath.Join(bundlePath, "rootfs", "etc", "daft-egress-ca.pem"))
	if err != nil {
		t.Fatalf("read CA cert: %v", err)
	}
	if !bytes.Equal(got, pem) {
		t.Errorf("CA cert content = %q, want %q", got, pem)
	}
}

// TestBuildBundleConfigCACertEmptyPath covers the guard + lock
// release when CACert is set but its guest path is empty.
func TestBuildBundleConfigCACertEmptyPath(t *testing.T) {
	var unlockCalls int
	deps := happyPathDeps(t, &unlockCalls)
	_, _, err := BuildBundleConfig(t.Context(), "alpine:3", BootOptions{
		CACert: []byte("pem"),
	}, deps)
	if err == nil {
		t.Fatal("want error for empty CA cert guest path")
	}
	if unlockCalls != 1 {
		t.Errorf("lock not released on CA write failure: unlockCalls=%d", unlockCalls)
	}
}

// TestBuildBundleConfigCACertWriteError covers writeBundleCACert's
// filesystem-error path: a bundle.Path that is a regular file means
// MkdirAll of rootfs/etc beneath it fails.
func TestBuildBundleConfigCACertWriteError(t *testing.T) {
	bundleFile := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(bundleFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	deps := tempBundleDeps(t, bundleFile)
	_, _, err := BuildBundleConfig(t.Context(), "alpine:3", BootOptions{
		CACert:          []byte("pem"),
		CACertGuestPath: "/etc/daft-egress-ca.pem",
	}, deps)
	if err == nil {
		t.Fatal("want error when rootfs dir cannot be created under a file")
	}
}

// TestInstallProxyCA covers replacing an image's CA bundle with the
// proxy CA, including the common case where the bundle is an absolute
// symlink into a read-only store: the symlink must be removed and
// rewritten as a regular file holding the proxy CA, without writing
// through into the store.
func TestInstallProxyCA(t *testing.T) {
	bundlePath := t.TempDir()
	rootfs := filepath.Join(bundlePath, "rootfs")
	certsDir := filepath.Join(rootfs, "etc", "ssl", "certs")
	if err := os.MkdirAll(certsDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Simulate the nixos/nix layout: ca-bundle.crt is an ABSOLUTE
	// symlink (-> /nix/store/...) into a read-only store. installProxyCA
	// must drop the symlink and write a regular file, never writing
	// through into the store.
	storeDir := filepath.Join(rootfs, "nix", "store", "x-nss-cacert")
	if err := os.MkdirAll(storeDir, 0o750); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}
	orig := []byte("REAL-CA\n")
	if err := os.WriteFile(filepath.Join(storeDir, "ca-bundle.crt"), orig, 0o444); err != nil {
		t.Fatalf("write store file: %v", err)
	}
	link := filepath.Join(certsDir, "ca-bundle.crt")
	if err := os.Symlink("/nix/store/x-nss-cacert/ca-bundle.crt", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	storeFile := filepath.Join(storeDir, "ca-bundle.crt")

	proxyCA := []byte("PROXY-CA\n")
	installProxyCA(bundlePath, proxyCA)

	// The link is now a regular file holding exactly the proxy CA.
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("ca-bundle.crt is still a symlink; want regular file")
	}
	//nolint:gosec // G304 — test-only path under t.TempDir().
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("read installed bundle: %v", err)
	}
	if !bytes.Equal(got, proxyCA) {
		t.Fatalf("installed bundle = %q, want %q", got, proxyCA)
	}
	// The read-only store file must be untouched (we removed the
	// symlink rather than writing through it).
	//nolint:gosec // G304 — test-only path under t.TempDir().
	store, err := os.ReadFile(storeFile)
	if err != nil {
		t.Fatalf("read store file: %v", err)
	}
	if !bytes.Equal(store, orig) {
		t.Errorf("store file mutated = %q, want %q", store, orig)
	}
}

// TestInstallProxyCAPreservesOnWriteFailure pins the atomicity fix:
// if the temp write can't be created (here: a read-only certs dir),
// the original CA bundle must survive intact rather than being
// deleted first and left missing.
func TestInstallProxyCAPreservesOnWriteFailure(t *testing.T) {
	bundlePath := t.TempDir()
	certsDir := filepath.Join(bundlePath, "rootfs", "etc", "ssl", "certs")
	if err := os.MkdirAll(certsDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	bundle := filepath.Join(certsDir, "ca-bundle.crt")
	orig := []byte("REAL-CA\n")
	if err := os.WriteFile(bundle, orig, 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	// Read-only dir → the temp write fails, so the replace can't
	// proceed. Restore perms at cleanup so t.TempDir can remove it.
	if err := os.Chmod(certsDir, 0o500); err != nil { //nolint:gosec // G302: test needs a read-only-but-traversable dir.
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(certsDir, 0o700) }) //nolint:gosec // G302: restore so TempDir cleanup can remove it.

	installProxyCA(bundlePath, []byte("PROXY-CA\n"))

	//nolint:gosec // G304 — test-only path under t.TempDir().
	got, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatalf("original bundle missing after failed replace: %v", err)
	}
	if !bytes.Equal(got, orig) {
		t.Errorf("original bundle = %q, want preserved %q", got, orig)
	}
}

func TestBuildBundleConfigHappyPath(t *testing.T) {
	var unlockCalls int
	deps := happyPathDeps(t, &unlockCalls)

	cfg, unlock, err := BuildBundleConfig(t.Context(), "alpine:3", BootOptions{}, deps)
	if err != nil {
		t.Fatalf("BuildBundleConfig: %v", err)
	}
	if cfg.CloudHypervisor != testCHPath || cfg.Kernel != testKernelPath || cfg.Initramfs != testInitramfsPath || cfg.Virtiofsd != testVirtiofsdPath {
		t.Errorf("artefact paths not propagated: %+v", cfg)
	}
	if len(cfg.Shares) != 1 || cfg.Shares[0].Tag != BundlesTag || cfg.Shares[0].Mountpoint != BundlesMount {
		t.Errorf("bundle share wrong: %+v", cfg.Shares)
	}
	if cfg.Bundle == "" || !strings.HasPrefix(cfg.Bundle, BundlesMount) {
		t.Errorf("Bundle path = %q, want prefix %q", cfg.Bundle, BundlesMount)
	}

	unlock()
	if unlockCalls != 1 {
		t.Errorf("unlock invoked %d times, want 1", unlockCalls)
	}
}

// TestBuildBundleConfigUsesCloneNotCache pins the CoW isolation wiring:
// the dispatch must mount the per-dispatch clone (not the shared cached
// template), and teardown must release both the clone and the lock.
func TestBuildBundleConfigUsesCloneNotCache(t *testing.T) {
	var unlockCalls, cloneCleanups int
	store := &fakeStore{
		pull: func(context.Context, string) (*imagestore.Image, error) {
			return &imagestore.Image{Digest: testDigest}, nil
		},
		bundle: func(context.Context, string, imagestore.BundleOptions) (*imagestore.Bundle, error) {
			return &imagestore.Bundle{Path: "/cache/bundles/" + testDigest}, nil
		},
		clone: func(b *imagestore.Bundle) (*imagestore.Bundle, func(), error) {
			// A distinct per-dispatch path under a clones parent, keeping
			// the template's basename (mirrors CloneBundle).
			return &imagestore.Bundle{
				Path: "/cache/clones/d-xyz/" + filepath.Base(b.Path),
				UID:  b.UID, GID: b.GID,
			}, func() { cloneCleanups++ }, nil
		},
		lock: func(string) (func(), error) { return func() { unlockCalls++ }, nil },
	}
	deps := Deps{
		OpenStore: func() (Store, error) { return store, nil },
		CacheDir:  func() (string, error) { return t.TempDir(), nil },
		Extract: func(string) (bundled.Paths, error) {
			return bundled.Paths{CloudHypervisor: testCHPath, Kernel: testKernelPath, Initramfs: testInitramfsPath, Virtiofsd: testVirtiofsdPath}, nil
		},
	}

	cfg, teardown, err := BuildBundleConfig(t.Context(), "alpine:3", BootOptions{}, deps)
	if err != nil {
		t.Fatalf("BuildBundleConfig: %v", err)
	}
	// The exported share's host dir is the clone's parent, and the guest
	// bundle path resolves to the clone — never the cached template.
	if cfg.Shares[0].HostDir != "/cache/clones/d-xyz" {
		t.Errorf("share HostDir = %q, want the clone parent", cfg.Shares[0].HostDir)
	}
	if cfg.Bundle != BundlesMount+"/"+testDigest {
		t.Errorf("Bundle = %q, want clone basename under %q", cfg.Bundle, BundlesMount)
	}

	teardown()
	if cloneCleanups != 1 {
		t.Errorf("clone cleanup invoked %d times, want 1", cloneCleanups)
	}
	if unlockCalls != 1 {
		t.Errorf("lock released %d times, want 1", unlockCalls)
	}
}

// TestBuildBundleConfigCloneErrorReleasesLock covers the clone-failure
// branch: a CloneBundle error must release the bundle lock and surface.
func TestBuildBundleConfigCloneErrorReleasesLock(t *testing.T) {
	var unlockCalls int
	store := &fakeStore{
		pull: func(context.Context, string) (*imagestore.Image, error) {
			return &imagestore.Image{Digest: testDigest}, nil
		},
		bundle: func(context.Context, string, imagestore.BundleOptions) (*imagestore.Bundle, error) {
			return &imagestore.Bundle{Path: "/cache/bundles/" + testDigest}, nil
		},
		clone: func(*imagestore.Bundle) (*imagestore.Bundle, func(), error) {
			return nil, nil, errors.New("reflink failed")
		},
		lock: func(string) (func(), error) { return func() { unlockCalls++ }, nil },
	}
	deps := Deps{
		OpenStore: func() (Store, error) { return store, nil },
		CacheDir:  func() (string, error) { return t.TempDir(), nil },
		Extract: func(string) (bundled.Paths, error) {
			return bundled.Paths{CloudHypervisor: testCHPath, Kernel: testKernelPath, Initramfs: testInitramfsPath, Virtiofsd: testVirtiofsdPath}, nil
		},
	}
	_, _, err := BuildBundleConfig(t.Context(), "alpine:3", BootOptions{}, deps)
	if err == nil {
		t.Fatal("want error when CloneBundle fails")
	}
	if unlockCalls != 1 {
		t.Errorf("lock not released on clone failure: unlockCalls=%d", unlockCalls)
	}
}

// TestBuildBundleConfigTranslatesUserMountsForNonRoot pins the #100
// wiring: a non-root container (bundle resolves a non-zero USER) gets
// each host-backed user mount presented as its own uid/gid via
// virtiofsd id-translation, while the bundle/rootfs share keeps
// identity passthrough. A root container leaves every share untranslated.
func TestBuildBundleConfigTranslatesUserMountsForNonRoot(t *testing.T) {
	mounts := []Mount{{Target: "/workspace", Source: "/srv/work"}}

	tests := []struct {
		name     string
		uid, gid int
		want     *sandbox.ShareTranslate
	}{
		{name: "non-root", uid: 1000, gid: 100, want: &sandbox.ShareTranslate{UID: 1000, GID: 100}},
		{name: "root", uid: 0, gid: 0, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var unlockCalls int
			deps := happyPathDeps(t, &unlockCalls)
			store := &fakeStore{
				pull: func(context.Context, string) (*imagestore.Image, error) {
					return &imagestore.Image{Digest: testDigest}, nil
				},
				bundle: func(_ context.Context, digest string, _ imagestore.BundleOptions) (*imagestore.Bundle, error) {
					return &imagestore.Bundle{Path: "/tmp/bundles/" + digest, UID: tt.uid, GID: tt.gid}, nil
				},
				lock: func(string) (func(), error) { return func() { unlockCalls++ }, nil },
			}
			deps.OpenStore = func() (Store, error) { return store, nil }

			cfg, unlock, err := BuildBundleConfig(t.Context(), "alpine:3", BootOptions{Mounts: mounts}, deps)
			if err != nil {
				t.Fatalf("BuildBundleConfig: %v", err)
			}
			defer unlock()

			// Shares[0] is the bundle share — never translated.
			if cfg.Shares[0].Translate != nil {
				t.Errorf("bundle share Translate = %+v, want nil", cfg.Shares[0].Translate)
			}
			// Shares[1] is the /workspace user mount.
			got := cfg.Shares[1].Translate
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Errorf("user mount Translate = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestBuildBundleConfigEmptyRef(t *testing.T) {
	_, _, err := BuildBundleConfig(t.Context(), "", BootOptions{}, Deps{})
	if err == nil || !strings.Contains(err.Error(), "empty image ref") {
		t.Errorf("err = %v, want 'empty image ref'", err)
	}
}

func TestBuildBundleConfigRejectsNilDep(t *testing.T) {
	cases := map[string]Deps{
		"no OpenStore": {CacheDir: func() (string, error) { return "", nil }, Extract: func(string) (bundled.Paths, error) { return bundled.Paths{}, nil }},
		"no CacheDir":  {OpenStore: func() (Store, error) { return &fakeStore{}, nil }, Extract: func(string) (bundled.Paths, error) { return bundled.Paths{}, nil }},
		"no Extract":   {OpenStore: func() (Store, error) { return &fakeStore{}, nil }, CacheDir: func() (string, error) { return "", nil }},
	}
	for name, deps := range cases {
		_, _, err := BuildBundleConfig(t.Context(), "alpine:3", BootOptions{}, deps)
		if err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestBuildBundleConfigOpenStoreError(t *testing.T) {
	want := errors.New("store open failed")
	deps := Deps{
		OpenStore: func() (Store, error) { return nil, want },
		CacheDir:  func() (string, error) { return "", nil },
		Extract:   func(string) (bundled.Paths, error) { return bundled.Paths{}, nil },
	}
	_, _, err := BuildBundleConfig(t.Context(), "alpine:3", BootOptions{}, deps)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrapping %v", err, want)
	}
}

func TestBuildBundleConfigPullError(t *testing.T) {
	want := errors.New("registry unreachable")
	store := &fakeStore{
		pull: func(context.Context, string) (*imagestore.Image, error) { return nil, want },
	}
	deps := Deps{
		OpenStore: func() (Store, error) { return store, nil },
		CacheDir:  func() (string, error) { return "", nil },
		Extract:   func(string) (bundled.Paths, error) { return bundled.Paths{}, nil },
	}
	_, _, err := BuildBundleConfig(t.Context(), "alpine:3", BootOptions{}, deps)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrapping %v", err, want)
	}
}

func TestBuildBundleConfigReleasesLockOnExtractError(t *testing.T) {
	var unlockCalls int
	deps := happyPathDeps(t, &unlockCalls)
	deps.Extract = func(string) (bundled.Paths, error) {
		return bundled.Paths{}, errors.New("extract failed")
	}
	_, _, err := BuildBundleConfig(t.Context(), "alpine:3", BootOptions{}, deps)
	if err == nil {
		t.Fatal("expected error")
	}
	if unlockCalls != 1 {
		t.Errorf("lock leaked on extract failure: unlockCalls = %d, want 1", unlockCalls)
	}
}

func TestBuildBundleConfigReleasesLockOnCacheDirError(t *testing.T) {
	var unlockCalls int
	deps := happyPathDeps(t, &unlockCalls)
	deps.CacheDir = func() (string, error) { return "", errors.New("xdg") }
	_, _, err := BuildBundleConfig(t.Context(), "alpine:3", BootOptions{}, deps)
	if err == nil {
		t.Fatal("expected error")
	}
	if unlockCalls != 1 {
		t.Errorf("lock leaked on cacheDir failure: unlockCalls = %d, want 1", unlockCalls)
	}
}

func TestBuildBundleConfigBundleError(t *testing.T) {
	store := &fakeStore{
		pull: func(context.Context, string) (*imagestore.Image, error) {
			return &imagestore.Image{Digest: testDigest}, nil
		},
		bundle: func(context.Context, string, imagestore.BundleOptions) (*imagestore.Bundle, error) {
			return nil, errors.New("bundle build failed")
		},
	}
	deps := Deps{
		OpenStore: func() (Store, error) { return store, nil },
		CacheDir:  func() (string, error) { return "", nil },
		Extract:   func(string) (bundled.Paths, error) { return bundled.Paths{}, nil },
	}
	_, _, err := BuildBundleConfig(t.Context(), "alpine:3", BootOptions{}, deps)
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestBuildBundleConfigAppliesMounts pins the D6c.1 mount-wiring:
// each opts.Mounts entry becomes one virtiofs share (with the
// per-index `wsmntN` tag) plus one bind mount in the OCI bundle.
// The bundle's ExtraMounts are observed via the fakeStore's Bundle
// callback so the test catches both halves in one shot.
func TestBuildBundleConfigAppliesMounts(t *testing.T) {
	var seenOpts imagestore.BundleOptions
	store := &fakeStore{
		pull: func(context.Context, string) (*imagestore.Image, error) {
			return &imagestore.Image{Digest: testDigest}, nil
		},
		bundle: func(_ context.Context, _ string, opts imagestore.BundleOptions) (*imagestore.Bundle, error) {
			seenOpts = opts
			return &imagestore.Bundle{Path: "/tmp/bundles/x"}, nil
		},
		lock: func(string) (func(), error) {
			return func() {}, nil
		},
	}
	deps := Deps{
		OpenStore: func() (Store, error) { return store, nil },
		CacheDir:  func() (string, error) { return testCacheDir, nil },
		Extract: func(string) (bundled.Paths, error) {
			return bundled.Paths{
				CloudHypervisor: testCHPath,
				Kernel:          testKernelPath,
				Initramfs:       testInitramfsPath,
				Virtiofsd:       testVirtiofsdPath,
			}, nil
		},
	}

	opts := BootOptions{Mounts: []Mount{
		{Target: testMountTarget1, Source: testMountSource},
		{Target: testMountTarget2, Source: "/host/data", ReadOnly: true},
	}}
	cfg, _, err := BuildBundleConfig(t.Context(), "alpine:3", opts, deps)
	if err != nil {
		t.Fatalf("BuildBundleConfig: %v", err)
	}

	// Bundle + 2 mount shares.
	if len(cfg.Shares) != 3 {
		t.Fatalf("cfg.Shares = %d, want 3 (bundle + 2 mounts)", len(cfg.Shares))
	}
	wantShares := []struct{ tag, mp, src string }{
		{"wsmnt0", testMountTarget1, testMountSource},
		{"wsmnt1", testMountTarget2, "/host/data"},
	}
	for i, w := range wantShares {
		got := cfg.Shares[i+1]
		if got.Tag != w.tag || got.Mountpoint != w.mp || got.HostDir != w.src {
			t.Errorf("Shares[%d] = %+v, want tag=%s mp=%s src=%s",
				i+1, got, w.tag, w.mp, w.src)
		}
	}

	if len(seenOpts.ExtraMounts) != 2 {
		t.Fatalf("BundleOptions.ExtraMounts = %d, want 2", len(seenOpts.ExtraMounts))
	}
	if seenOpts.ExtraMounts[0].Destination != testMountTarget1 ||
		seenOpts.ExtraMounts[0].Type != bundleMountTypeBind ||
		seenOpts.ExtraMounts[0].Options[1] != "rw" {
		t.Errorf("rw mount wrong: %+v", seenOpts.ExtraMounts[0])
	}
	if seenOpts.ExtraMounts[1].Destination != testMountTarget2 ||
		seenOpts.ExtraMounts[1].Options[1] != "ro" {
		t.Errorf("ro mount wrong: %+v", seenOpts.ExtraMounts[1])
	}
}

func TestBuildBundleConfigAppliesFixedCodexCredentialHome(t *testing.T) {
	var seen imagestore.BundleOptions
	store := &fakeStore{pull: func(context.Context, string) (*imagestore.Image, error) {
		return &imagestore.Image{Digest: testDigest}, nil
	}, bundle: func(_ context.Context, _ string, opts imagestore.BundleOptions) (*imagestore.Bundle, error) {
		seen = opts
		return &imagestore.Bundle{Path: "/tmp/bundles/x"}, nil
	}, lock: func(string) (func(), error) { return func() {}, nil }}
	deps := Deps{OpenStore: func() (Store, error) { return store, nil }, CacheDir: func() (string, error) { return testCacheDir, nil }, Extract: func(string) (bundled.Paths, error) {
		return bundled.Paths{CloudHypervisor: testCHPath, Kernel: testKernelPath, Initramfs: testInitramfsPath, Virtiofsd: testVirtiofsdPath}, nil
	}}
	cfg, _, err := BuildBundleConfig(t.Context(), "alpine:3", BootOptions{CredentialProvider: credentialProviderCodex}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CredentialProvider != credentialProviderCodex || len(seen.ExtraMounts) != 1 {
		t.Fatalf("cfg=%+v opts=%+v", cfg, seen)
	}
	m := seen.ExtraMounts[0]
	if m.Source != agentcredential.CodexHome || m.Destination != agentcredential.CodexHome || m.Type != bundleMountTypeBind {
		t.Fatalf("mount=%+v", m)
	}
	found := false
	for _, v := range seen.Env {
		if v == "CODEX_HOME="+agentcredential.CodexHome {
			found = true
		}
	}
	if !found {
		t.Fatalf("env=%v", seen.Env)
	}
	if _, _, err := BuildBundleConfig(t.Context(), "alpine:3", BootOptions{CredentialProvider: "claude"}, deps); err == nil {
		t.Fatal("unsupported provider succeeded")
	}
}

func TestPersistentContainerUsesGuestAgentKeepalive(t *testing.T) {
	args := PersistentContainerArgs()
	if len(args) != 2 || args[0] != persistentBinaryTarget || args[1] != persistentBinaryFlag {
		t.Fatalf("PersistentContainerArgs = %q", args)
	}
	args[0] = "mutated"
	if PersistentContainerArgs()[0] != persistentBinaryTarget {
		t.Fatal("PersistentContainerArgs returned mutable package storage")
	}
	mounts, _, err := credentialBundleOptions(BootOptions{ContainerPersistent: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 {
		t.Fatalf("mounts = %+v, want one keepalive mount", mounts)
	}
	m := mounts[0]
	if m.Source != persistentBinarySource || m.Destination != persistentBinaryTarget || m.Type != bundleMountTypeBind {
		t.Fatalf("keepalive mount = %+v", m)
	}
}

// TestBuildBundleConfigRejectsBadMounts covers the validation
// gate: relative paths, empty paths, and duplicate targets all
// fail before any store side effect.
func TestBuildBundleConfigRejectsBadMounts(t *testing.T) {
	deps := Deps{
		OpenStore: func() (Store, error) {
			t.Fatal("OpenStore must not run before validation")
			return nil, errors.New("unreachable")
		},
		CacheDir: func() (string, error) { return "", nil },
		Extract:  func(string) (bundled.Paths, error) { return bundled.Paths{}, nil },
	}
	cases := map[string]Mount{
		"relative target": {Target: "workspace", Source: testMountSource},
		"empty target":    {Target: "", Source: testMountSource},
		"relative source": {Target: testMountTarget1, Source: "host/ws"},
		"empty source":    {Target: testMountTarget1, Source: ""},
	}
	for name, m := range cases {
		_, _, err := BuildBundleConfig(t.Context(), "alpine:3",
			BootOptions{Mounts: []Mount{m}}, deps)
		if err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}

	_, _, err := BuildBundleConfig(t.Context(), "alpine:3",
		BootOptions{Mounts: []Mount{
			{Target: testMountTarget1, Source: "/a"},
			{Target: testMountTarget1, Source: "/b"},
		}}, deps)
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Errorf("duplicate target: err = %v, want 'duplicated'", err)
	}
}

func TestBuildBundleConfigLockError(t *testing.T) {
	store := &fakeStore{
		pull: func(context.Context, string) (*imagestore.Image, error) {
			return &imagestore.Image{Digest: testDigest}, nil
		},
		bundle: func(context.Context, string, imagestore.BundleOptions) (*imagestore.Bundle, error) {
			return &imagestore.Bundle{Path: "/x"}, nil
		},
		lock: func(string) (func(), error) { return nil, errors.New("lock contention") },
	}
	deps := Deps{
		OpenStore: func() (Store, error) { return store, nil },
		CacheDir:  func() (string, error) { return "", nil },
		Extract:   func(string) (bundled.Paths, error) { return bundled.Paths{}, nil },
	}
	_, _, err := BuildBundleConfig(t.Context(), "alpine:3", BootOptions{}, deps)
	if err == nil {
		t.Fatal("expected error")
	}
}
