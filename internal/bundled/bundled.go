// Package bundled embeds the sandbox-host artifacts (cloud-hypervisor,
// the guest kernel bzImage, the initramfs cpio) into the daft-sandboxd
// binary so it can be shipped as a single static executable.
//
// Builds outside the project's Nix flake see empty stub files in
// assets/, so plain `go build` still compiles — but the resulting
// binary's Extract call returns ErrNotBundled. The flake overwrites
// the stubs with the real artifacts at build time.
package bundled

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed assets/cloud-hypervisor
var cloudHypervisorBin []byte

//go:embed assets/bzImage
var kernelBin []byte

//go:embed assets/initramfs.cpio.gz
var initramfsBin []byte

//go:embed assets/virtiofsd
var virtiofsdBin []byte

// Paths is the set of file paths to the extracted artifacts. The
// fields line up 1:1 with sandbox.Config so callers can copy them
// straight in.
type Paths struct {
	CloudHypervisor string
	Kernel          string
	Initramfs       string
	Virtiofsd       string
}

// ErrNotBundled is returned when the binary was built without the
// sandbox artifacts embedded (e.g. plain `go build` outside Nix).
// Callers should fall back to env-var-supplied paths or fail with a
// user-actionable message.
var ErrNotBundled = errors.New("bundled: sandbox artifacts not embedded in this binary")

// minBundledSize is the threshold below which an "embedded" asset is
// treated as a stub. The smallest real artifact is the initramfs
// (~750 KiB); 4 KiB is comfortably above the stub size (0 B) and
// safely below any real artifact.
const minBundledSize = 4 * 1024

// IsBundled reports whether this binary has real artifacts embedded.
// Useful for tests and for emitting better diagnostics on the dev
// `go build` path.
func IsBundled() bool {
	return len(cloudHypervisorBin) >= minBundledSize &&
		len(kernelBin) >= minBundledSize &&
		len(initramfsBin) >= minBundledSize &&
		len(virtiofsdBin) >= minBundledSize
}

// Extract writes the embedded artifacts to dir (creating it if needed)
// and returns their paths. Returns ErrNotBundled if the artifacts are
// stubs.
//
// Re-extraction is idempotent: if a target file already exists with
// the same size, it is not rewritten — letting callers pass a
// persistent cache (e.g. $XDG_CACHE_HOME/daft) to avoid recopying ~20
// MiB on every invocation.
func Extract(dir string) (Paths, error) {
	return extractTo(dir, cloudHypervisorBin, kernelBin, initramfsBin, virtiofsdBin)
}

// extractTo is Extract with the source bytes injected, so tests can
// exercise the file-writing logic with fake data without rebuilding.
func extractTo(dir string, ch, kernel, initramfs, virtiofsd []byte) (Paths, error) {
	if len(ch) < minBundledSize || len(kernel) < minBundledSize ||
		len(initramfs) < minBundledSize || len(virtiofsd) < minBundledSize {
		return Paths{}, ErrNotBundled
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Paths{}, fmt.Errorf("bundled: mkdir cache: %w", err)
	}
	p := Paths{
		CloudHypervisor: filepath.Join(dir, "cloud-hypervisor"),
		Kernel:          filepath.Join(dir, "bzImage"),
		Initramfs:       filepath.Join(dir, "initramfs.cpio.gz"),
		Virtiofsd:       filepath.Join(dir, "virtiofsd"),
	}
	for _, f := range []struct {
		path string
		data []byte
		perm os.FileMode
	}{
		{p.CloudHypervisor, ch, 0o755},
		{p.Kernel, kernel, 0o644},
		{p.Initramfs, initramfs, 0o644},
		{p.Virtiofsd, virtiofsd, 0o755},
	} {
		if err := writeIfChanged(f.path, f.data, f.perm); err != nil {
			return Paths{}, err
		}
	}
	return p, nil
}

// writeIfChanged writes data to path only if path doesn't already
// exist with matching size. Size is a cheap proxy for content match;
// the cache dir's keying (binary version + checksum) is the real
// guarantee against staleness, so a file with the right size at the
// right path is treated as fresh.
func writeIfChanged(path string, data []byte, perm os.FileMode) error {
	fi, err := os.Stat(path)
	if err == nil && fi.Size() == int64(len(data)) {
		return nil
	}
	if err := os.WriteFile(path, data, perm); err != nil {
		return fmt.Errorf("bundled: write %s: %w", path, err)
	}
	return nil
}
