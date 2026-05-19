package sandboxboot

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/bundled"
	"github.com/richiejp/darkaifactory/internal/imagestore"
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
)

// fakeStore is a hand-rolled Store fake. Each method's behaviour
// is a func field so individual tests can flip just one.
type fakeStore struct {
	pull   func(context.Context, string) (*imagestore.Image, error)
	bundle func(context.Context, string, imagestore.BundleOptions) (*imagestore.Bundle, error)
	lock   func(string) (func(), error)
}

func (f *fakeStore) Pull(ctx context.Context, ref string) (*imagestore.Image, error) {
	return f.pull(ctx, ref)
}

func (f *fakeStore) Bundle(ctx context.Context, digest string, opts imagestore.BundleOptions) (*imagestore.Bundle, error) {
	return f.bundle(ctx, digest, opts)
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
		CacheDir:  func() (string, error) { return "/tmp/cache", nil },
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
		CacheDir:  func() (string, error) { return "/tmp/cache", nil },
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
		seenOpts.ExtraMounts[0].Type != "bind" ||
		seenOpts.ExtraMounts[0].Options[1] != "rw" {
		t.Errorf("rw mount wrong: %+v", seenOpts.ExtraMounts[0])
	}
	if seenOpts.ExtraMounts[1].Destination != testMountTarget2 ||
		seenOpts.ExtraMounts[1].Options[1] != "ro" {
		t.Errorf("ro mount wrong: %+v", seenOpts.ExtraMounts[1])
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
