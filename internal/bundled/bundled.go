// Package bundled embeds the sandbox-host artifacts (cloud-hypervisor,
// the guest kernel bzImage, the initramfs cpio) into the vmjail-sandboxd
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
	"os"
)

//go:embed assets/cloud-hypervisor
var cloudHypervisorBin []byte

//go:embed assets/bzImage
var kernelBin []byte

//go:embed assets/initramfs.cpio.gz
var initramfsBin []byte

//go:embed assets/virtiofsd
var virtiofsdBin []byte

// Paths contains parent-process /proc/self/fd paths to sealed artifacts. The
// fields line up 1:1 with sandbox.Config so callers can copy them
// straight in.
type Paths struct {
	CloudHypervisor string
	Kernel          string
	Initramfs       string
	Virtiofsd       string
	// Files own the sealed assets. Keep them open until all VM processes stop.
	Files [4]*os.File
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

// Extract loads embedded assets into sealed anonymous files. The directory
// argument is retained for launcher compatibility but is unused: no cache
// files are read or written. Call Close after all consumers have stopped.
func Extract(_ string) (Paths, error) {
	return loadAssets(cloudHypervisorBin, kernelBin, initramfsBin, virtiofsdBin)
}

// Close releases the parent's descriptors. Child processes inherit independent
// references; callers must nevertheless retain these until launching is done.
func (p Paths) Close() {
	for _, f := range p.Files {
		if f != nil {
			_ = f.Close()
		}
	}
}
