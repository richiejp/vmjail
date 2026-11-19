// Package buildimage embeds the devcontainer build image (the buildah-based
// in-sandbox image builder) into the vmjail-orchestrator binary, so the
// orchestrator can auto-seed it into its embedded OCI registry at startup
// and a build-based devcontainer needs no VMJAIL_BUILD_IMAGE_REF.
//
// Builds outside the project's Nix flake see an empty stub file in assets/,
// so plain `go build` still compiles — but IsBundled() then reports false
// and Image() returns ErrNotBundled. The flake overwrites the stub with the
// real `vmjail-build-image` tarball at build time (see flake.nix).
//
// This mirrors internal/bundled (the VM artefacts embedded into
// vmjail-sandboxd), but the payload is an OCI image consumed through the
// registry rather than files extracted to a cache dir.
package buildimage

import (
	"bytes"
	_ "embed"
	"errors"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/richiejp/vmjail/internal/ociregistry"
)

//go:embed assets/build-image.tar.gz
var buildImageTar []byte

// ErrNotBundled is returned by Image when the binary was built without the
// build image embedded (e.g. plain `go build` outside Nix). Callers fall
// back to leaving the registry unseeded — a build-based devcontainer then
// fails loudly at dispatch unless VMJAIL_BUILD_IMAGE_REF points elsewhere.
var ErrNotBundled = errors.New("buildimage: build image not embedded in this binary")

// minBundledSize is the threshold below which the embedded asset is treated
// as a stub. The real build image is hundreds of MiB; 4 KiB is comfortably
// above the stub size (0 B) and far below any real image.
const minBundledSize = 4 * 1024

// IsBundled reports whether this binary has the real build image embedded.
func IsBundled() bool {
	return len(buildImageTar) >= minBundledSize
}

// Image loads the embedded build image. Returns ErrNotBundled if the asset
// is a stub. The returned v1.Image is lazy — its layers are read when the
// caller pushes it into the registry.
func Image() (v1.Image, error) {
	return imageFrom(buildImageTar)
}

// imageFrom is Image with the source bytes injected, so tests can exercise
// both the stub (ErrNotBundled) and the real-image paths without the embed
// being populated.
func imageFrom(b []byte) (v1.Image, error) {
	if len(b) < minBundledSize {
		return nil, ErrNotBundled
	}
	return ociregistry.ImageFromReader(bytes.NewReader(b))
}
