package sandboxboot

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/bundled"
	"github.com/richiejp/darkaifactory/internal/imagestore"
)

const testDigest = "sha256:abc"

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
				CloudHypervisor: "/ch",
				Kernel:          "/kernel",
				Initramfs:       "/initramfs",
				Virtiofsd:       "/virtiofsd",
			}, nil
		},
	}
}

func TestBuildBundleConfigHappyPath(t *testing.T) {
	var unlockCalls int
	deps := happyPathDeps(t, &unlockCalls)

	cfg, unlock, err := BuildBundleConfig(t.Context(), "alpine:3", deps)
	if err != nil {
		t.Fatalf("BuildBundleConfig: %v", err)
	}
	if cfg.CloudHypervisor != "/ch" || cfg.Kernel != "/kernel" || cfg.Initramfs != "/initramfs" || cfg.Virtiofsd != "/virtiofsd" {
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
	_, _, err := BuildBundleConfig(t.Context(), "", Deps{})
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
		_, _, err := BuildBundleConfig(t.Context(), "alpine:3", deps)
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
	_, _, err := BuildBundleConfig(t.Context(), "alpine:3", deps)
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
	_, _, err := BuildBundleConfig(t.Context(), "alpine:3", deps)
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
	_, _, err := BuildBundleConfig(t.Context(), "alpine:3", deps)
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
	_, _, err := BuildBundleConfig(t.Context(), "alpine:3", deps)
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
	_, _, err := BuildBundleConfig(t.Context(), "alpine:3", deps)
	if err == nil {
		t.Fatal("expected error")
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
	_, _, err := BuildBundleConfig(t.Context(), "alpine:3", deps)
	if err == nil {
		t.Fatal("expected error")
	}
}
