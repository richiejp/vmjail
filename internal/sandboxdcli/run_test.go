package sandboxdcli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/bundled"
	"github.com/richiejp/darkaifactory/internal/imagestore"
	"github.com/richiejp/darkaifactory/internal/sandbox"
)

func TestRunIsRegistered(t *testing.T) {
	stdout, _, err := runCmd(t, "--help")
	if err != nil {
		t.Fatalf("daft-sandboxd --help: %v", err)
	}
	if !strings.Contains(stdout, "run ") {
		t.Errorf("root help missing run command:\n%s", stdout)
	}
}

func TestRunRequiresImageRef(t *testing.T) {
	_, _, err := runCmd(t, "run")
	if err == nil {
		t.Fatal("expected error when image ref is missing")
	}
}

// fakeStore implements storeAPI without touching a real registry.
type fakeStore struct {
	pullRef       string
	pullErr       error
	pulledImage   *imagestore.Image
	bundleDigest  string
	bundleOptsArg imagestore.BundleOptions
	bundleErr     error
	bundle        *imagestore.Bundle
	lockDigest    string
	lockErr       error
	unlockCalled  bool
}

func (f *fakeStore) Pull(_ context.Context, ref string) (*imagestore.Image, error) {
	f.pullRef = ref
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	return f.pulledImage, nil
}

func (f *fakeStore) Bundle(_ context.Context, digest string, opts imagestore.BundleOptions) (*imagestore.Bundle, error) {
	f.bundleDigest = digest
	f.bundleOptsArg = opts
	if f.bundleErr != nil {
		return nil, f.bundleErr
	}
	return f.bundle, nil
}

func (f *fakeStore) Lock(digest string) (func(), error) {
	f.lockDigest = digest
	if f.lockErr != nil {
		return nil, f.lockErr
	}
	return func() { f.unlockCalled = true }, nil
}

// testDigest is a minimal but well-formed digest used across run
// tests; goconst flagged it when the literal repeated. testBundleDir
// is the corresponding on-disk bundle path the fake store returns.
const (
	testDigest    = "sha256:abc"
	testBundleDir = "/cache/bundles/sha256_abc"
)

// fakePaths is the canonical set of fake artefact paths passed to
// every test that calls bundled.Extract via deps. Hoisted to one
// place to satisfy goconst across tests in this package.
var fakePaths = bundled.Paths{
	CloudHypervisor: "/x/cloud-hypervisor",
	Kernel:          "/x/bzImage",
	Initramfs:       "/x/initramfs.cpio.gz",
	Virtiofsd:       "/x/virtiofsd",
}

// runDepsForTest builds a runDeps with a canned store + extract that
// uses fakePaths. Tests vary the store to drive different code paths.
func runDepsForTest(store storeAPI) runDeps {
	return runDeps{
		openStore: func() (storeAPI, error) { return store, nil },
		cacheDir:  func() (string, error) { return "/tmp/cache", nil },
		extract:   func(string) (bundled.Paths, error) { return fakePaths, nil },
		boot: func(_ context.Context, _ io.Writer, _ sandbox.Config) error {
			return nil
		},
	}
}

func TestRunWiresBundleShareAndBundlePath(t *testing.T) {
	digest := testDigest
	bundlePath := "/var/lib/daft-sandboxd-images/bundles/sha256_abc"
	store := &fakeStore{
		pulledImage: &imagestore.Image{Ref: "img:1", Digest: digest, Size: 1024},
		bundle:      &imagestore.Bundle{Digest: digest, Path: bundlePath},
	}

	deps := runDepsForTest(store)
	bootCfgs := captureBootCfg(&deps)

	if err := runRun(context.Background(), io.Discard, "img:1", nil, "", deps); err != nil {
		t.Fatalf("runRun: %v", err)
	}

	cfg := *bootCfgs
	if len(cfg.Shares) != 1 {
		t.Fatalf("Shares = %d, want 1 (no workspace)", len(cfg.Shares))
	}
	if cfg.Shares[0].HostDir != filepath.Dir(bundlePath) {
		t.Errorf("bundle share HostDir = %q, want %q", cfg.Shares[0].HostDir, filepath.Dir(bundlePath))
	}
	wantBundle := filepath.Join(bundlesMount, filepath.Base(bundlePath))
	if cfg.Bundle != wantBundle {
		t.Errorf("Config.Bundle = %q, want %q", cfg.Bundle, wantBundle)
	}
	if !store.unlockCalled {
		t.Error("bundle unlock was not called")
	}
}

func TestRunAddsWorkspaceShareAndMount(t *testing.T) {
	wsDir := t.TempDir()
	digest := testDigest
	store := &fakeStore{
		pulledImage: &imagestore.Image{Ref: "img:1", Digest: digest},
		bundle:      &imagestore.Bundle{Digest: digest, Path: testBundleDir},
	}

	deps := runDepsForTest(store)
	bootCfgs := captureBootCfg(&deps)

	if err := runRun(context.Background(), io.Discard, "img:1", nil, wsDir, deps); err != nil {
		t.Fatalf("runRun: %v", err)
	}
	cfg := *bootCfgs
	if len(cfg.Shares) != 2 {
		t.Fatalf("Shares = %d, want 2 (bundle + workspace)", len(cfg.Shares))
	}
	if cfg.Shares[1].Mountpoint != workspaceMount {
		t.Errorf("workspace mountpoint = %q, want %q", cfg.Shares[1].Mountpoint, workspaceMount)
	}
	abs, _ := filepath.Abs(wsDir)
	if cfg.Shares[1].HostDir != abs {
		t.Errorf("workspace HostDir = %q, want %q", cfg.Shares[1].HostDir, abs)
	}

	if len(store.bundleOptsArg.ExtraMounts) != 1 ||
		store.bundleOptsArg.ExtraMounts[0].Destination != workspaceMount {
		t.Errorf("bundle ExtraMounts = %+v, want one /workspace bind",
			store.bundleOptsArg.ExtraMounts)
	}
}

func TestRunPropagatesPullError(t *testing.T) {
	want := errors.New("synthetic pull failure")
	store := &fakeStore{pullErr: want}
	if err := runRun(context.Background(), io.Discard, "img:1", nil, "", runDepsForTest(store)); !errors.Is(err, want) {
		t.Fatalf("err = %v, want wrap of %v", err, want)
	}
}

func TestRunPropagatesBundleError(t *testing.T) {
	want := errors.New("synthetic bundle failure")
	store := &fakeStore{
		pulledImage: &imagestore.Image{Digest: testDigest},
		bundleErr:   want,
	}
	if err := runRun(context.Background(), io.Discard, "img:1", nil, "", runDepsForTest(store)); !errors.Is(err, want) {
		t.Fatalf("err = %v, want wrap of %v", err, want)
	}
}

// TestDefaultRunDeps confirms the production deps wiring at least
// constructs without panic. We only call defaultRunDeps to populate
// the function pointers; we don't invoke the bound implementations
// (which would touch the real network / disk).
func TestDefaultRunDeps(t *testing.T) {
	deps := defaultRunDeps()
	if deps.openStore == nil || deps.cacheDir == nil ||
		deps.extract == nil || deps.boot == nil {
		t.Errorf("defaultRunDeps left a hook nil: %+v", deps)
	}
}

// TestRunPropagatesLockError covers the lock-failure branch of
// runRun (post-bundle, pre-boot).
func TestRunPropagatesLockError(t *testing.T) {
	want := errors.New("synthetic lock failure")
	store := &fakeStore{
		pulledImage: &imagestore.Image{Digest: testDigest},
		bundle:      &imagestore.Bundle{Digest: testDigest, Path: testBundleDir},
		lockErr:     want,
	}
	if err := runRun(context.Background(), io.Discard, "img:1", nil, "", runDepsForTest(store)); !errors.Is(err, want) {
		t.Fatalf("err = %v, want wrap of %v", err, want)
	}
}

// TestRunResolveWorkspaceAbsoluteOnRelativeInput confirms the
// filepath.Abs branch is exercised.
func TestRunResolveWorkspaceAbsoluteOnRelativeInput(t *testing.T) {
	store := &fakeStore{
		pulledImage: &imagestore.Image{Digest: testDigest},
		bundle:      &imagestore.Bundle{Digest: testDigest, Path: testBundleDir},
	}
	deps := runDepsForTest(store)
	bootCfgs := captureBootCfg(&deps)

	// "." is a relative workspace; runRun should rewrite it to abs.
	if err := runRun(context.Background(), io.Discard, "img:1", nil, ".", deps); err != nil {
		t.Fatalf("runRun: %v", err)
	}
	cfg := *bootCfgs
	if len(cfg.Shares) != 2 {
		t.Fatalf("Shares = %d, want 2", len(cfg.Shares))
	}
	if !filepath.IsAbs(cfg.Shares[1].HostDir) {
		t.Errorf("workspace HostDir %q is not absolute", cfg.Shares[1].HostDir)
	}
}

func TestRunPassesArgsToBundleOptions(t *testing.T) {
	store := &fakeStore{
		pulledImage: &imagestore.Image{Digest: testDigest},
		bundle:      &imagestore.Bundle{Digest: testDigest, Path: testBundleDir},
	}
	deps := runDepsForTest(store)
	if err := runRun(context.Background(), io.Discard, "img:1",
		[]string{"sh", "-c", "echo hi"}, "", deps); err != nil {
		t.Fatalf("runRun: %v", err)
	}
	if got := store.bundleOptsArg.Args; len(got) != 3 || got[0] != "sh" {
		t.Errorf("Bundle Args = %v, want [sh -c \"echo hi\"]", got)
	}
}

// captureBootCfg replaces deps.boot with a recorder that stores the
// passed Config in *out. Returns the same pointer.
func captureBootCfg(deps *runDeps) *sandbox.Config {
	var captured sandbox.Config
	deps.boot = func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
		captured = cfg
		return nil
	}
	return &captured
}
