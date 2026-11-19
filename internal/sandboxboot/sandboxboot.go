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

	"github.com/richiejp/darkaifactory/internal/agentcredential"
	"github.com/richiejp/darkaifactory/internal/bundled"
	"github.com/richiejp/darkaifactory/internal/imagestore"
	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/sandbox"
)

// In-VM mount points the executor and CLI agree on. Stable so the
// daft-agent inside the VM can find them.
const (
	BundlesMount            = "/bundles"
	BundlesTag              = "bundles"
	credentialProviderCodex = "codex"
	bundleMountTypeBind     = "bind"
	persistentBinarySource  = "/bin/daft-agent"
	persistentBinaryTarget  = "/daft-workspace-keepalive"
	persistentBinaryFlag    = "--container-keepalive"
)

// PersistentContainerArgs is an image-independent keepalive command. The
// assembler bind-mounts the guest's static daft-agent at this target, so
// scratch and distroless images do not need /bin/sh or sleep.
func PersistentContainerArgs() []string {
	return []string{persistentBinaryTarget, persistentBinaryFlag}
}

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
	// Args, when non-nil, replaces the image's Entrypoint+Cmd with the
	// container's process args — the seam the in-sandbox image builder
	// (#101 slice 3c) uses to run a generated build script against the
	// static build image instead of baking a per-build image. See
	// imagestore.BundleOptions.Args.
	Args []string
	// ContainerPersistent injects the static guest keepalive executable into
	// the OCI container. Callers may select it with PersistentContainerArgs.
	ContainerPersistent bool
	// Cwd, when non-empty, overrides the image working directory. A
	// devcontainer uses workspaceFolder so lifecycle and attached commands
	// start where its tooling expects the project.
	Cwd string
	// Env is merged over the image's Env (keyed by NAME) in the
	// container's OCI process env. Carries the workspace's own env
	// plus, under Phase J, the injected egress-proxy variables.
	Env []string
	// CACert, when non-empty, is a PEM CA bundle written into the
	// bundle's rootfs at CACertGuestPath (an absolute in-container
	// path) before boot, so the container's TLS clients — pointed at
	// it via NIX_SSL_CERT_FILE / SSL_CERT_FILE — trust the egress
	// proxy's MITM leaf certs.
	CACert          []byte
	CACertGuestPath string
	// Nested grants the workload container nested-virtualisation
	// access (/dev/kvm bind + cgroup device-allow). Off by default;
	// set from SandboxSpec.nested_virtualization.enable by the
	// executor. See imagestore.BundleOptions.Nested.
	Nested bool
	// PrivilegedFuseMounts grants the workload container FUSE-mount
	// access (/dev/fuse bind + cgroup device-allow + CAP_SYS_ADMIN).
	// Off by default; set from SandboxSpec.privileged_fuse_mounts.enable
	// by the executor. See imagestore.BundleOptions.PrivilegedFuseMounts.
	PrivilegedFuseMounts bool
	// PrivilegedBuild grants the workload container the standard
	// container capability set so an in-sandbox buildah can build images.
	// Off by default; set from SandboxSpec.privileged_build.enable by the
	// executor. See imagestore.BundleOptions.PrivilegedBuild.
	PrivilegedBuild bool
	// PostStartCommands are the devcontainer lifecycle commands the in-VM
	// agent runs inside the container after it has started, in order (the
	// resolved postStartCommand). Each entry is a full argv. Empty leaves
	// the agent's post-start step a no-op. Set by the executor from the
	// node-resolved devcontainer; carried through to sandbox.Config →
	// GuestConfig.
	PostStartCommands [][]string
	// CredentialProvider enables one built-in, tmpfs-backed agent home. The
	// only supported value is "codex"; arbitrary paths and environment names
	// are intentionally not part of this surface.
	CredentialProvider string
}

// Store is the imagestore surface BuildBundleConfig needs. Kept as
// an interface so tests can swap in fakes without standing up a
// registry.
type Store interface {
	Pull(ctx context.Context, ref string) (*imagestore.Image, error)
	Bundle(ctx context.Context, digest string, opts imagestore.BundleOptions) (*imagestore.Bundle, error)
	// CloneBundle returns an ephemeral copy-on-write clone of a cached
	// bundle plus a cleanup that removes it, so a dispatch mutates its
	// own rootfs instead of the shared per-digest cache.
	CloneBundle(context.Context, *imagestore.Bundle) (*imagestore.Bundle, func(), error)
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
	// CacheDir is a legacy loader hint. The sealed embedded loader ignores it.
	CacheDir func() (string, error)
	// Extract loads artifacts and returns their paths and owned descriptors.
	// BuildBundleConfig includes descriptor cleanup in its returned release.
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
	extraMounts, env, err := credentialBundleOptions(opts)
	if err != nil {
		return sandbox.Config{}, unlock, err
	}
	bundle, err := store.Bundle(ctx, img.Digest, imagestore.BundleOptions{
		ExtraMounts:          extraMounts,
		Args:                 opts.Args,
		Cwd:                  opts.Cwd,
		Env:                  env,
		Nested:               opts.Nested,
		PrivilegedFuseMounts: opts.PrivilegedFuseMounts,
		PrivilegedBuild:      opts.PrivilegedBuild,
	})
	if err != nil {
		return sandbox.Config{}, unlock, fmt.Errorf("sandboxboot: bundle %s: %w", img.Digest, err)
	}
	bundle, bundleTeardown, err := lockAndCloneBundle(ctx, store, bundle, img.Digest)
	if err != nil {
		return sandbox.Config{}, unlock, err
	}
	// From here on, any error must remove the clone and release the lock.
	unlockNow := unlock
	defer func() {
		if err != nil {
			bundleTeardown()
			unlock = unlockNow
		}
	}()

	if len(opts.CACert) > 0 {
		if err = writeBundleCACert(bundle.Path, opts.CACertGuestPath, opts.CACert); err != nil {
			return sandbox.Config{}, unlock, fmt.Errorf("sandboxboot: write CA cert: %w", err)
		}
		// Tools like nix read a fixed CA bundle path (and a login
		// shell's profile re-points *_SSL_CERT_FILE at it), not our
		// injected env, so also install the proxy CA at the image's
		// standard bundle paths.
		installProxyCA(bundle.Path, opts.CACert)
	}

	cacheDir, err := deps.CacheDir()
	if err != nil {
		return sandbox.Config{}, unlock, fmt.Errorf("sandboxboot: resolve cache dir: %w", err)
	}
	paths, err := deps.Extract(cacheDir)
	if err != nil {
		return sandbox.Config{}, unlock, fmt.Errorf("sandboxboot: extract artefacts: %w", err)
	}

	cfg = sandbox.Config{
		ArtifactFiles:      paths.Files,
		CloudHypervisor:    paths.CloudHypervisor,
		Kernel:             paths.Kernel,
		Initramfs:          paths.Initramfs,
		Virtiofsd:          paths.Virtiofsd,
		Shares:             buildShares(bundle, opts.Mounts),
		Bundle:             filepath.Join(BundlesMount, filepath.Base(bundle.Path)),
		PostStartCommands:  opts.PostStartCommands,
		CredentialProvider: opts.CredentialProvider,
	}
	return cfg, func() { paths.Close(); bundleTeardown() }, nil
}

func credentialBundleOptions(opts BootOptions) ([]imagestore.BundleMount, []string, error) {
	mounts := bundleMountsFromSpec(opts.Mounts)
	env := opts.Env
	if opts.ContainerPersistent {
		mounts = append(mounts, imagestore.BundleMount{
			Destination: persistentBinaryTarget,
			Type:        bundleMountTypeBind,
			Source:      persistentBinarySource,
			Options:     []string{"bind", "ro", "nosuid", "nodev"},
		})
	}
	if opts.CredentialProvider == "" {
		return mounts, env, nil
	}
	if opts.CredentialProvider != credentialProviderCodex {
		return nil, nil, fmt.Errorf("sandboxboot: unsupported credential provider %q", opts.CredentialProvider)
	}
	mounts = append(mounts, imagestore.BundleMount{Destination: agentcredential.CodexHome, Type: bundleMountTypeBind, Source: agentcredential.CodexHome, Options: []string{"rbind", "rw", "nosuid", "nodev", "noexec"}})
	env = append(env, "CODEX_HOME="+agentcredential.CodexHome)
	return mounts, env, nil
}

// lockAndCloneBundle locks the digest's cache entry and returns an
// ephemeral copy-on-write clone of bundle plus a teardown that removes
// the clone and releases the lock. The lock is held across the clone so
// the cached template can't be evicted mid-copy. The clone is what the
// dispatch mounts and mutates — its host-side provisioning (CA
// injection) and the guest's rootfs writes (e.g. nix realising a closure
// into /nix/store) land on the clone, never the shared per-digest cache.
func lockAndCloneBundle(ctx context.Context, store Store, bundle *imagestore.Bundle, digest string) (*imagestore.Bundle, func(), error) {
	bundleUnlock, err := store.Lock(digest)
	if err != nil {
		return nil, nil, fmt.Errorf("sandboxboot: lock bundle: %w", err)
	}
	clone, cloneCleanup, err := store.CloneBundle(ctx, bundle)
	if err != nil {
		bundleUnlock()
		return nil, nil, fmt.Errorf("sandboxboot: clone bundle: %w", err)
	}
	return clone, func() {
		cloneCleanup()
		bundleUnlock()
	}, nil
}

// buildShares assembles the virtio-fs shares: the bundle/rootfs share
// (identity passthrough — its tree legitimately carries many uids)
// followed by one share per user mount.
//
// A non-root container (image USER uid:gid) can't read or write a
// host-backed mount whose files belong to the dev: sandboxd's userns
// presents them as ns-0 (root), leaving the container in `other`. So
// each user mount is presented as owned by the container user via
// virtiofsd id-translation. A root container already owns ns-0's
// files and needs none, so its mounts stay untranslated.
func buildShares(bundle *imagestore.Bundle, mounts []Mount) []*sandbox.ShareConfig {
	shares := make([]*sandbox.ShareConfig, 0, 1+len(mounts))
	shares = append(shares, &sandbox.ShareConfig{
		HostDir:    filepath.Dir(bundle.Path),
		Tag:        BundlesTag,
		Mountpoint: BundlesMount,
	})
	var translate *sandbox.ShareTranslate
	if bundle.UID != 0 || bundle.GID != 0 {
		translate = &sandbox.ShareTranslate{UID: bundle.UID, GID: bundle.GID}
	}
	for i, m := range mounts {
		shares = append(shares, &sandbox.ShareConfig{
			HostDir:    m.Source,
			Tag:        mountTag(i),
			Mountpoint: m.Target,
			Translate:  translate,
		})
	}
	return shares
}

// writeBundleCACert writes pem into the bundle's rootfs at the
// absolute in-container path guestPath (e.g. /etc/daft-egress-ca.pem),
// creating parent dirs. Written before boot and 0644 because a CA
// cert is public; the container's TLS clients read it via
// NIX_SSL_CERT_FILE / SSL_CERT_FILE. Atomic (tmp+rename) so a bundle
// shared by a concurrently-booting VM never sees a partial file.
func writeBundleCACert(bundlePath, guestPath string, pem []byte) error {
	if guestPath == "" {
		return errors.New("sandboxboot: empty CA cert guest path")
	}
	dst := filepath.Join(bundlePath, "rootfs", filepath.FromSlash(guestPath))
	//nolint:gosec // G301: /etc must be world-traversable so the container's non-root user can reach the CA.
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir for CA cert: %w", err)
	}
	tmp := dst + ".tmp"
	//nolint:gosec // G306: a CA cert is public; container TLS clients must read it.
	if err := os.WriteFile(tmp, pem, 0o644); err != nil {
		return fmt.Errorf("write CA cert: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("rename CA cert: %w", err)
	}
	return nil
}

// standardCABundlePaths are the in-container CA-bundle locations
// common base images use. The injected *_SSL_CERT_FILE env covers
// tools that honour it (Go's net/http), but others — notably nix's
// fetcher, which reads a fixed path — need the proxy CA folded into
// the bundle they actually open.
//
//nolint:gochecknoglobals // immutable lookup table of well-known CA bundle paths.
var standardCABundlePaths = []string{
	"/etc/ssl/certs/ca-bundle.crt",       // nixos/nix, RHEL
	"/etc/ssl/certs/ca-certificates.crt", // debian/ubuntu/alpine
	"/etc/ssl/cert.pem",                  // alpine/openssl
	"/etc/pki/tls/certs/ca-bundle.crt",   // RHEL/CentOS
}

// installProxyCA overwrites the image's standard CA bundles with the
// proxy CA. Under Phase J a configured proxy implies default-deny, so
// the proxy is the guest's only egress path: the guest validates the
// proxy's MITM leaves and nothing else (the proxy itself validates
// real upstream certs host-side). Replacing rather than appending
// also sidesteps reading the image's existing bundle, which is
// usually an absolute symlink farm into the read-only nix store that
// won't resolve from the host. Each target is removed (dropping the
// symlink) and rewritten as a regular file. Best-effort: absent paths
// are skipped; a path we can't replace is logged.
func installProxyCA(bundlePath string, proxyCA []byte) {
	rootfs := filepath.Join(bundlePath, "rootfs")
	for _, p := range standardCABundlePaths {
		dst := filepath.Join(rootfs, filepath.FromSlash(p))
		if _, err := os.Lstat(dst); err != nil {
			continue // absent in this image
		}
		// Atomic replace: write a sibling temp file, then rename over
		// the (usually symlink) entry. rename(2) replaces a symlink in
		// place, and a failed write leaves the original bundle intact
		// rather than deleting it first — so a partial failure can't
		// strip the guest of all CAs.
		tmp := dst + ".tmp"
		//nolint:gosec // G306: a CA bundle is public; container TLS clients must read it.
		if err := os.WriteFile(tmp, proxyCA, 0o644); err != nil {
			oops.Log("sandboxboot: write CA bundle "+p, err)
			continue
		}
		if err := os.Rename(tmp, dst); err != nil {
			oops.Skip("sandboxboot: clean tmp CA bundle", os.Remove(tmp), os.ErrNotExist)
			oops.Log("sandboxboot: replace CA bundle "+p, err)
		}
	}
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
