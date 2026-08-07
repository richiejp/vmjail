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
	"bytes"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"io"
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

//nolint:gochecknoglobals // narrow filesystem hooks make atomic-write failures deterministic in tests.
var (
	tempChmod = func(f *os.File, mode os.FileMode) error { return f.Chmod(mode) }
	tempWrite = func(f *os.File, data []byte) (int, error) { return f.Write(data) }
	tempClose = func(f *os.File) error { return f.Close() }
)

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
// Re-extraction is idempotent: if a target regular file already exists with
// the same SHA-256 digest, it is not rewritten — letting callers pass a
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
	// #nosec G301 — the cache dir lives under $XDG_CACHE_HOME and
	// gets read by daft-sandboxd subprocesses; 0755 lets the
	// per-VM unprivileged users traverse it.
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

// writeIfChanged writes data atomically unless path is already a regular file
// with the exact SHA-256 digest. The cache directory is stable across binary
// upgrades, so size alone cannot distinguish same-sized artifact revisions.
func writeIfChanged(path string, data []byte, perm os.FileMode) error {
	want := sha256.Sum256(data)
	f, err := os.Open(path) //nolint:gosec // path is the caller-selected cache directory.
	if err == nil {
		fi, statErr := f.Stat()
		if statErr == nil && fi.Mode().IsRegular() && fi.Size() == int64(len(data)) {
			h := sha256.New()
			if _, copyErr := io.Copy(h, f); copyErr == nil && bytes.Equal(h.Sum(nil), want[:]) {
				_ = f.Close()
				return nil
			}
		}
		_ = f.Close()
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("bundled: write %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tempChmod(tmp, perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("bundled: chmod %s: %w", path, err)
	}
	if _, err := tempWrite(tmp, data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("bundled: write %s: %w", path, err)
	}
	if err := tempClose(tmp); err != nil {
		return fmt.Errorf("bundled: close %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("bundled: replace %s: %w", path, err)
	}
	return nil
}
