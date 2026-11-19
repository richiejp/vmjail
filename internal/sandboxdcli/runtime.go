package sandboxdcli

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/richiejp/vmjail/internal/bundled"
	"github.com/richiejp/vmjail/internal/imagestore"
	"github.com/richiejp/vmjail/internal/ociregistry"
	"github.com/richiejp/vmjail/internal/sandbox"
)

// envBuildImageRef names the orchestrator-hosted ref of the static image
// builder VM (buildah + dash + a CA bundle). A build-based devcontainer is
// built in an ephemeral sandbox from this ref before the workload boots
// (#101 slice 3c). It is OPTIONAL: unset, the node defaults to the
// orchestrator's auto-seeded built-in builder (ociregistry.BuiltinBuildRef);
// set it only to override with a custom builder image.
const envBuildImageRef = "VMJAIL_BUILD_IMAGE_REF"

// resolveBuildImageRef resolves the build image ref: the
// VMJAIL_BUILD_IMAGE_REF override when set, else the orchestrator's
// auto-seeded built-in builder. The built-in default is a hosted ref
// (sentinel host) pulled over the same mTLS transport as any workspace
// image, so it is only meaningful when that transport exists — without it
// we leave the ref empty (build path off, as before) rather than hand
// sandboxexec a hosted ref it can't reach. On a non-bundled orchestrator
// the seed is simply absent and the pull fails loudly at build dispatch.
func resolveBuildImageRef(hostedRT http.RoundTripper) string {
	if ref := os.Getenv(envBuildImageRef); ref != "" {
		return ref
	}
	if hostedRT == nil {
		return ""
	}
	return ociregistry.BuiltinBuildRef()
}

// envObserveSummary gates the Tier-2 observe share (open-snoop gadget +
// build-progress summary) on every image-backed dispatch. It is ON BY
// DEFAULT so the `vmjail sandbox observe` progress summary works out of the
// box; an operator who does not want the gadget load + extra virtiofs
// device per VM sets it to a falsy value ("0"/"false"/"no"/"off",
// case-insensitive) to opt a node out.
const envObserveSummary = "VMJAIL_OBSERVE_SUMMARY"

func observeSummaryFromEnv() bool {
	return envEnabledByDefault(envObserveSummary)
}

// envEnabledByDefault reports whether the env var named key is on. On by
// default; only an explicit falsy value ("0"/"false"/"no"/"off",
// case-insensitive) returns false. Used for observation-summary opt-out.
func envEnabledByDefault(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// extractWithEnvFallback wraps bundled.Extract: when the binary
// has the artefacts embedded (Nix build) it returns those;
// otherwise (plain `go build`) it falls back to the artefact
// paths sandbox.ConfigFromEnv resolved into base. This is the
// "callers should fall back to env-var-supplied paths" pattern
// the bundled package's doc comment specifies. Returns the
// embed error untouched when neither source is available, so a
// genuinely misconfigured daemon still produces a clean
// ErrNotBundled at dispatch time.
func extractWithEnvFallback(base sandbox.Config) func(dir string) (bundled.Paths, error) {
	return func(dir string) (bundled.Paths, error) {
		paths, err := bundled.Extract(dir)
		if !errors.Is(err, bundled.ErrNotBundled) {
			return paths, err
		}
		envPaths := bundled.Paths{
			CloudHypervisor: base.CloudHypervisor,
			Kernel:          base.Kernel,
			Initramfs:       base.Initramfs,
			Virtiofsd:       base.Virtiofsd,
		}
		if envPaths.CloudHypervisor == "" || envPaths.Kernel == "" ||
			envPaths.Initramfs == "" || envPaths.Virtiofsd == "" {
			return bundled.Paths{}, err
		}
		return envPaths, nil
	}
}

// resolveVsockDir returns the absolute, mkdir'd parent directory for
// per-Sandbox hybrid-vsock UDS files and a cleanup function. An explicit
// override must already be absolute and remains caller-owned. Otherwise a
// short application directory is selected below systemd's RUNTIME_DIRECTORY
// or the user's XDG_RUNTIME_DIR. The unmanaged os.TempDir fallback is private
// to this process and removed at clean shutdown. Socket listeners pre-clean
// their exact stale paths before binding.
func resolveVsockDir(override string) (dir string, cleanup func(), err error) {
	dir = override
	owned := false
	if dir == "" {
		dir, owned = defaultVsockDir()
	}
	if !filepath.IsAbs(dir) {
		return "", nil, fmt.Errorf("vmjail-sandboxd: --vsock-dir %q must be absolute", dir)
	}
	if err := validateVsockDirLength(dir); err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("vmjail-sandboxd: mkdir vsock dir: %w", err)
	}
	cleanup = func() {}
	if owned {
		cleanup = func() {
			// The directory contains only daemon-owned Unix sockets. Removal is
			// best-effort during shutdown; startup always selects a fresh path.
			_ = os.RemoveAll(dir)
		}
	}
	return dir, cleanup, nil
}

func validateVsockDirLength(dir string) error {
	// Linux sockaddr_un.sun_path is 108 bytes including its terminating NUL.
	// The longest current listener path is a 36-byte Sandbox UUID plus the
	// devcontainer-build and hybrid-vsock port suffixes.
	const maxUnixPathBytes = 107
	const longestSocketLeaf = "00000000-0000-0000-0000-000000000000-build.sock_7000"
	path := filepath.Join(dir, longestSocketLeaf)
	if len(path) > maxUnixPathBytes {
		return fmt.Errorf("vmjail-sandboxd: vsock directory %q leaves no room for Unix socket names (%d-byte path exceeds %d)",
			dir, len(path), maxUnixPathBytes)
	}
	return nil
}

func defaultVsockDir() (string, bool) {
	if dir := firstAbsolutePath(os.Getenv("RUNTIME_DIRECTORY")); dir != "" {
		return filepath.Join(dir, "vsock"), false
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "vmjail-vsock"), false
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("vmjail-vsock-%d", os.Getpid())), true
}

// firstAbsolutePath selects the first absolute entry from a systemd
// RUNTIME_DIRECTORY value, which may contain a colon-separated list when a
// unit declares more than one RuntimeDirectory.
func firstAbsolutePath(value string) string {
	for _, dir := range filepath.SplitList(value) {
		if filepath.IsAbs(dir) {
			return dir
		}
	}
	return ""
}

// openSandboxbootStore opens the same production imagestore the legacy `run`
// command uses. The concrete type is retained so daemon startup can sweep
// orphan clones before exposing it to the executor through sandboxboot.Store.
// When hostedRT is non-nil it is the mTLS transport for orchestrator-hosted
// image refs.
func openSandboxbootStore(hostedRT http.RoundTripper) (*imagestore.Store, error) {
	st, err := openStore()
	if err != nil {
		return nil, err
	}
	if hostedRT != nil {
		st.UseHostedRegistry(hostedRT)
	}
	return st, nil
}

// formatBytes is a small KiB/MiB/GiB renderer for the startup
// banner. Production traffic doesn't read this output, so a
// best-effort, three-significant-digit rendering is fine.
func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	suffix := "KMGTPE"[exp]
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), suffix)
}
