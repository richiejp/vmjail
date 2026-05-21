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
	"os"
	"path/filepath"

	"github.com/localai-org/darkaifactory/internal/bundled"
	"github.com/localai-org/darkaifactory/internal/imagestore"
	"github.com/localai-org/darkaifactory/internal/sandbox"
)

// In-VM mount points the executor and CLI agree on. Stable so the
// daft-agent inside the VM can find them.
const (
	BundlesMount = "/bundles"
	BundlesTag   = "bundles"
)

// workspaceMountTagPrefix prefixes each spec.Mount's virtiofs Tag
// — distinct from BundlesTag so the agent's share-mount loop
// never collides with the bundle share.
const workspaceMountTagPrefix = "wsmnt"

// Mount is the per-mount slice of a sandbox spec the executor
// hands BuildBundleConfig. Target/Source/ReadOnly mirror the proto
// schema; the executor projects sandboxv1.Mount onto this so the
// bootassembler doesn't import the wire package.
type Mount struct {
	// Target is the in-container path the bind mount lands at. The
	// VM mounts the share at the same path internally so the OCI
	// bundle can bind-mount it straight through.
	Target string
	// Source is the absolute host-side path the share exports. The
	// executor's caller is responsible for ensuring the path exists
	// and is reachable from the sandboxd process.
	Source string
	// ReadOnly toggles between rw and ro bind options.
	ReadOnly bool
}

// BootOptions carry the spec-derived knobs BuildBundleConfig
// honours alongside the image ref.
type BootOptions struct {
	Mounts []Mount
	// HostUID / HostGID forward to imagestore.BundleOptions; see
	// that type for the user-namespace rationale.
	HostUID int
	HostGID int
}

// HostProcessIDs returns the sandboxd process's effective uid/gid
// — the values to plumb into BootOptions / BundleOptions so the
// container's uid 0 maps to virtiofsd's host uid. Single seam so
// callers don't each reinvent the os.Geteuid/Getegid pair.
func HostProcessIDs() (uid, gid int) {
	return os.Geteuid(), os.Getegid()
}

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
// opts.Mounts becomes one virtiofs share per entry plus a matching
// OCI bind mount in the bundle's config.json. Each share's tag is
// `wsmntN` (declaration-order) so the agent's share-mount loop
// finds it via the cmdline.
//
// On any error the returned unlock is the no-op (nil-safe to
// call), and partial state in the imagestore is left alone so a
// retry can pick up where this one stopped.
func BuildBundleConfig(
	ctx context.Context,
	ref string,
	opts BootOptions,
	deps Deps,
) (cfg sandbox.Config, unlock func(), err error) {
	unlock = func() {}
	if ref == "" {
		return sandbox.Config{}, unlock, errors.New("sandboxboot: empty image ref")
	}
	if err := validateDeps(deps); err != nil {
		return sandbox.Config{}, unlock, err
	}
	if err := validateMounts(opts.Mounts); err != nil {
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
	bundle, err := store.Bundle(ctx, img.Digest, imagestore.BundleOptions{
		ExtraMounts: bundleMountsFromSpec(opts.Mounts),
		HostUID:     opts.HostUID,
		HostGID:     opts.HostGID,
	})
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
	for i, m := range opts.Mounts {
		cfg.Shares = append(cfg.Shares, &sandbox.ShareConfig{
			HostDir:    m.Source,
			Tag:        mountTag(i),
			Mountpoint: m.Target,
		})
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

// validateMounts rejects malformed entries before we touch the
// store, so a bad spec fails fast rather than leaking a partial
// bundle. Empty Source/Target and non-absolute paths are the only
// shapes that can break downstream — duplicates on the same
// target are rejected too because they'd collide on the agent's
// mount loop (same path, two shares).
func validateMounts(mounts []Mount) error {
	seen := make(map[string]struct{}, len(mounts))
	for i, m := range mounts {
		if !filepath.IsAbs(m.Target) {
			return fmt.Errorf(
				"sandboxboot: mounts[%d].Target %q must be absolute",
				i, m.Target,
			)
		}
		if !filepath.IsAbs(m.Source) {
			return fmt.Errorf(
				"sandboxboot: mounts[%d].Source %q must be absolute",
				i, m.Source,
			)
		}
		if _, dup := seen[m.Target]; dup {
			return fmt.Errorf(
				"sandboxboot: mounts[%d].Target %q duplicated",
				i, m.Target,
			)
		}
		seen[m.Target] = struct{}{}
	}
	return nil
}

// bundleMountsFromSpec projects the executor's view of the mounts
// onto the imagestore's bundle-side type. Each spec entry becomes
// one bind mount in the OCI runtime spec, with Source == Target
// because the VM has already mounted the share at Target via
// virtiofs (one path, no rewrites needed).
func bundleMountsFromSpec(mounts []Mount) []imagestore.BundleMount {
	if len(mounts) == 0 {
		return nil
	}
	out := make([]imagestore.BundleMount, 0, len(mounts))
	for _, m := range mounts {
		bindOpt := "rw"
		if m.ReadOnly {
			bindOpt = "ro"
		}
		out = append(out, imagestore.BundleMount{
			Destination: m.Target,
			Type:        "bind",
			Source:      m.Target,
			Options:     []string{"rbind", bindOpt},
		})
	}
	return out
}

func mountTag(i int) string {
	return fmt.Sprintf("%s%d", workspaceMountTagPrefix, i)
}
