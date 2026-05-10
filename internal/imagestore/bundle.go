package imagestore

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// ErrUnknownImage is returned by Bundle when the requested digest is
// not in the layer cache. Caller should Pull first.
var ErrUnknownImage = errors.New("imagestore: image not in store; pull it first")

// Bundle is the path to an unpacked OCI runtime bundle ready for crun.
type Bundle struct {
	// Digest is the image digest the bundle was assembled from.
	Digest string
	// Path is the absolute directory containing config.json and
	// rootfs/. Pass directly to `crun run --bundle <Path>`.
	Path string
}

// BundleOptions overrides the OCI runtime spec we generate from the
// image's config. All fields are optional; zero values mean "honour
// the image config" or "use our default."
type BundleOptions struct {
	// Args, when non-nil, replaces the image's Entrypoint+Cmd. Pass
	// []string{"/bin/sh"} to override.
	Args []string
	// Env, when non-nil, replaces the image's Env entirely.
	Env []string
	// Cwd, when non-empty, overrides the image's WorkingDir.
	Cwd string
	// Hostname, when non-empty, overrides the default ("daft-container").
	Hostname string
}

// Bundle materialises a runnable bundle for digest. Idempotent: if
// the bundle directory exists with config.json and rootfs/, it is
// reused. The image must already be in the layer cache (Pull first).
func (s *Store) Bundle(ctx context.Context, digest string, opts BundleOptions) (*Bundle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	bundlePath := filepath.Join(s.Root, bundlesDir, digestPathFragment(digest))
	rootfs := filepath.Join(bundlePath, "rootfs")
	configPath := filepath.Join(bundlePath, "config.json")

	if isAssembled(configPath, rootfs) {
		s.touchEntry(digest)
		return &Bundle{Digest: digest, Path: bundlePath}, nil
	}

	img, err := s.loadLayoutImage(digest)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		return nil, fmt.Errorf("imagestore: mkdir rootfs: %w", err)
	}

	if err := unpackRootfs(img, rootfs); err != nil {
		return nil, err
	}

	cfg, err := buildRuntimeConfig(img, opts)
	if err != nil {
		return nil, err
	}
	if err := writeJSON(configPath, cfg); err != nil {
		return nil, err
	}

	s.touchEntry(digest)
	return &Bundle{Digest: digest, Path: bundlePath}, nil
}

// loadLayoutImage looks up digest in the on-disk OCI layout. Returns
// ErrUnknownImage if the layout has no manifest for it (or if the
// layout itself doesn't exist yet — a cold-start store treats every
// digest as unknown).
func (s *Store) loadLayoutImage(digest string) (v1.Image, error) {
	p, err := layout.FromPath(filepath.Join(s.Root, imagesDir))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownImage, digest)
	}
	h, err := v1.NewHash(digest)
	if err != nil {
		return nil, fmt.Errorf("imagestore: parse digest %q: %w", digest, err)
	}
	img, err := p.Image(h)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnknownImage, digest)
	}
	return img, nil
}

// touchEntry refreshes LastUsed for digest. Best-effort — failures
// degrade LRU accuracy but don't fail the Bundle call.
func (s *Store) touchEntry(digest string) {
	meta, err := s.loadMeta()
	if err != nil {
		return
	}
	for i := range meta.Entries {
		if meta.Entries[i].Digest == digest {
			meta.Entries[i].LastUsed = s.nowFn()
			_ = s.saveMeta(meta)
			return
		}
	}
}

// isAssembled reports whether a bundle dir already has the two parts
// crun needs (config.json + rootfs/). Existence is the only check —
// we never partial-write, since failure during unpack leaves only one
// of the two.
func isAssembled(configPath, rootfs string) bool {
	if _, err := os.Stat(configPath); err != nil {
		return false
	}
	fi, err := os.Stat(rootfs)
	return err == nil && fi.IsDir()
}

// unpackRootfs flattens the image's layers into dst.
// mutate.Extract returns a single tar stream with whiteouts already
// applied per the OCI image spec.
func unpackRootfs(img v1.Image, dst string) error {
	stream := mutate.Extract(img)
	defer func() { _ = stream.Close() }()
	return untarTo(stream, dst)
}

// untarTo reads a tar stream from r and writes its entries under dst.
// Files that would escape dst (via "../" or absolute path in the
// header) are rejected — defence against tar-bomb / path traversal.
func untarTo(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("imagestore: tar read: %w", err)
		}
		target, err := safeJoin(dst, hdr.Name)
		if err != nil {
			return err
		}
		if err := writeTarEntry(tr, hdr, target); err != nil {
			return fmt.Errorf("imagestore: write %s: %w", hdr.Name, err)
		}
	}
}

// safeJoin protects against tar entries that try to write outside dst.
// "/etc/passwd" → dst/etc/passwd; "../../etc/passwd" → error.
func safeJoin(dst, name string) (string, error) {
	cleaned := filepath.Clean("/" + name)
	target := filepath.Join(dst, cleaned)
	dstClean := filepath.Clean(dst)
	if !strings.HasPrefix(target, dstClean+string(filepath.Separator)) && target != dstClean {
		return "", fmt.Errorf("imagestore: tar entry %q escapes bundle root", name)
	}
	return target, nil
}

func writeTarEntry(tr *tar.Reader, hdr *tar.Header, target string) error {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, hdr.FileInfo().Mode().Perm())
	case tar.TypeReg:
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, hdr.FileInfo().Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	case tar.TypeSymlink:
		_ = os.Remove(target)
		return os.Symlink(hdr.Linkname, target)
	case tar.TypeLink:
		linkSource, err := safeJoin(filepath.Dir(target), hdr.Linkname)
		if err != nil {
			return err
		}
		_ = os.Remove(target)
		return os.Link(linkSource, target)
	default:
		// Devices, fifos, etc. are skipped — the rootfs runs
		// unprivileged inside the VM and can't usefully use them.
		return nil
	}
}
