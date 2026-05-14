// Package sandboxboot assembles a sandbox.Config that boots an
// OCI image: it pulls the image into the local imagestore, builds
// a runtime bundle, locks the bundle for the boot, and resolves
// the cached artefact paths (kernel, initramfs, cloud-hypervisor,
// virtiofsd).
//
// The output is everything `internal/sandbox` needs to boot a VM
// that runs the image's entrypoint inside a container — the same
// pipeline that `daft-sandboxd run` drives from CLI flags, but
// factored so both the dispatch executor (`internal/sandboxexec`)
// and the legacy CLI can call it.
//
// Mount constants are exported so the orchestrator and tests can
// reason about where shares land inside the VM.
package sandboxboot

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/richiejp/darkaifactory/internal/bundled"
	"github.com/richiejp/darkaifactory/internal/imagestore"
	"github.com/richiejp/darkaifactory/internal/sandbox"
)

// In-VM mount points the executor and CLI agree on. Stable so the
// daft-agent inside the VM can find them.
const (
	BundlesMount = "/bundles"
	BundlesTag   = "bundles"
)

// Store is the imagestore surface BuildBundleConfig needs. Kept as
// an interface so tests can swap in fakes without standing up a
// registry.
type Store interface {
	Pull(ctx context.Context, ref string) (*imagestore.Image, error)
	Bundle(ctx context.Context, digest string, opts imagestore.BundleOptions) (*imagestore.Bundle, error)
	Lock(digest string) (func(), error)
}

// Deps bundles the moving parts of BuildBundleConfig. Production
// callers pass DefaultDeps(); tests inject fakes.
type Deps struct {
	// OpenStore returns the imagestore the boot pulls into. Called
	// once per BuildBundleConfig invocation; the caller owns
	// closing the returned store (none of the Store methods need
	// it today, so the closure happens implicitly at process exit).
	OpenStore func() (Store, error)
	// CacheDir is the on-disk directory the bundled artefacts get
	// extracted into. Stable across invocations so the kernel /
	// initramfs / VMM binaries unpack once.
	CacheDir func() (string, error)
	// Extract unpacks the bundled artefacts into cacheDir and
	// returns their absolute paths.
	Extract func(cacheDir string) (bundled.Paths, error)
}

// BuildBundleConfig pulls ref, builds a runtime bundle, locks it,
// and returns a sandbox.Config ready to hand to sandbox.Hello.
// Unlock must be deferred by the caller; it releases the bundle's
// imagestore lock once the boot is over so the LRU can evict.
//
// On any error the returned unlock is the no-op (nil-safe to
// call), and partial state in the imagestore is left alone so a
// retry can pick up where this one stopped.
func BuildBundleConfig(
	ctx context.Context,
	ref string,
	deps Deps,
) (cfg sandbox.Config, unlock func(), err error) {
	unlock = func() {}
	if ref == "" {
		return sandbox.Config{}, unlock, errors.New("sandboxboot: empty image ref")
	}
	if err := validateDeps(deps); err != nil {
		return sandbox.Config{}, unlock, err
	}

	store, err := deps.OpenStore()
	if err != nil {
		return sandbox.Config{}, unlock, fmt.Errorf("sandboxboot: open store: %w", err)
	}
	img, err := store.Pull(ctx, ref)
	if err != nil {
		return sandbox.Config{}, unlock, fmt.Errorf("sandboxboot: pull %s: %w", ref, err)
	}
	bundle, err := store.Bundle(ctx, img.Digest, imagestore.BundleOptions{})
	if err != nil {
		return sandbox.Config{}, unlock, fmt.Errorf("sandboxboot: bundle %s: %w", img.Digest, err)
	}
	bundleUnlock, err := store.Lock(img.Digest)
	if err != nil {
		return sandbox.Config{}, unlock, fmt.Errorf("sandboxboot: lock bundle: %w", err)
	}
	// From here on, any error must release the lock.
	unlockNow := unlock
	defer func() {
		if err != nil {
			bundleUnlock()
			unlock = unlockNow
		}
	}()

	cacheDir, err := deps.CacheDir()
	if err != nil {
		return sandbox.Config{}, unlock, fmt.Errorf("sandboxboot: resolve cache dir: %w", err)
	}
	paths, err := deps.Extract(cacheDir)
	if err != nil {
		return sandbox.Config{}, unlock, fmt.Errorf("sandboxboot: extract artefacts: %w", err)
	}

	cfg = sandbox.Config{
		CloudHypervisor: paths.CloudHypervisor,
		Kernel:          paths.Kernel,
		Initramfs:       paths.Initramfs,
		Virtiofsd:       paths.Virtiofsd,
		Shares: []*sandbox.ShareConfig{{
			HostDir:    filepath.Dir(bundle.Path),
			Tag:        BundlesTag,
			Mountpoint: BundlesMount,
		}},
		Bundle: filepath.Join(BundlesMount, filepath.Base(bundle.Path)),
	}
	return cfg, bundleUnlock, nil
}

func validateDeps(d Deps) error {
	switch {
	case d.OpenStore == nil:
		return errors.New("sandboxboot: nil OpenStore")
	case d.CacheDir == nil:
		return errors.New("sandboxboot: nil CacheDir")
	case d.Extract == nil:
		return errors.New("sandboxboot: nil Extract")
	}
	return nil
}
