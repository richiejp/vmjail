// Package egresscache packs a recorded egress-proxy HTTP cache
// directory (an httpcache CAS: index.json + blobs/sha256/…) into a
// single-layer OCI image and pushes/pulls it to an OCI registry. It is
// build-time tooling for the e2e suite's pinned egress-replay cache
// (see PLAN.md "Pinned egress-replay cache for the bun e2e test"):
// `make update-egress-cache` records the cache and Pushes it to ghcr;
// `make integration-build` Pulls it back to bin/egress-cache, which the
// proxy then replays offline.
//
// The cache is an opaque tree of files, so it rides as the rootfs of a
// one-layer image — the simplest artifact every OCI registry accepts —
// rather than an OCI "artifact" with a custom media type.
package egresscache

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// epoch is the fixed mtime stamped on every tar entry so identical cache
// content packs to an identical layer digest — a re-record that changed
// nothing is then a no-op push. A function (not a global) keeps
// gochecknoglobals quiet.
func epoch() time.Time { return time.Unix(0, 0).UTC() }

// Push packs the directory tree at srcDir into a single-layer image and
// pushes it to ref. auth may be nil for an anonymous (public) push,
// though registries generally require auth to write. Extra crane options
// (e.g. crane.Insecure for a local test registry) append after auth.
func Push(srcDir, ref string, auth authn.Authenticator, opts ...crane.Option) error {
	tarPath, err := tarTree(srcDir)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tarPath) }()

	layer, err := tarball.LayerFromFile(tarPath)
	if err != nil {
		return fmt.Errorf("egresscache: layer from %s: %w", tarPath, err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		return fmt.Errorf("egresscache: assemble image: %w", err)
	}
	if err := crane.Push(img, ref, craneOpts(auth, opts)...); err != nil {
		return fmt.Errorf("egresscache: push %s: %w", ref, err)
	}
	return nil
}

// Pull fetches ref, flattens it, and extracts the tree into destDir
// (created if absent). auth may be nil for an anonymous (public) pull.
// Extra crane options append after auth.
func Pull(ref, destDir string, auth authn.Authenticator, opts ...crane.Option) error {
	img, err := crane.Pull(ref, craneOpts(auth, opts)...)
	if err != nil {
		return fmt.Errorf("egresscache: pull %s: %w", ref, err)
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return fmt.Errorf("egresscache: mkdir %s: %w", destDir, err)
	}
	rc := mutate.Extract(img)
	defer func() { _ = rc.Close() }()
	if err := untar(rc, destDir); err != nil {
		return fmt.Errorf("egresscache: extract into %s: %w", destDir, err)
	}
	return nil
}

// craneOpts prepends the auth option (anonymous when nil) to extra.
func craneOpts(auth authn.Authenticator, extra []crane.Option) []crane.Option {
	if auth == nil {
		auth = authn.Anonymous
	}
	return append([]crane.Option{crane.WithAuth(auth)}, extra...)
}

// tarTree writes srcDir's tree to a temp tar file with normalized
// (deterministic) headers and returns its path. Entry names are
// relative to srcDir, so extraction reconstructs the directory's
// contents directly at the destination root.
func tarTree(srcDir string) (string, error) {
	f, err := os.CreateTemp("", "egresscache-*.tar")
	if err != nil {
		return "", fmt.Errorf("egresscache: temp tar: %w", err)
	}
	tw := tar.NewWriter(f)
	walkErr := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr := &tar.Header{
			Name:    filepath.ToSlash(rel),
			ModTime: epoch(),
			Mode:    0o644,
		}
		if info.IsDir() {
			hdr.Typeflag, hdr.Name, hdr.Mode = tar.TypeDir, hdr.Name+"/", 0o755
			return tw.WriteHeader(hdr)
		}
		hdr.Typeflag, hdr.Size = tar.TypeReg, info.Size()
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		return copyFileInto(tw, path)
	})
	if cerr := closeAll(tw, f, walkErr); cerr != nil {
		_ = os.Remove(f.Name())
		return "", cerr
	}
	return f.Name(), nil
}

// copyFileInto streams the file at path into tw.
func copyFileInto(tw io.Writer, path string) error {
	in, err := os.Open(path) //nolint:gosec // G304: path comes from filepath.Walk of a caller-named dir.
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	_, err = io.Copy(tw, in)
	return err
}

// closeAll flushes the tar writer and closes the file, preferring the
// earliest non-nil error (walkErr first).
func closeAll(tw *tar.Writer, f *os.File, walkErr error) error {
	twErr := tw.Close()
	fErr := f.Close()
	switch {
	case walkErr != nil:
		return fmt.Errorf("egresscache: walk tree: %w", walkErr)
	case twErr != nil:
		return fmt.Errorf("egresscache: finish tar: %w", twErr)
	case fErr != nil:
		return fmt.Errorf("egresscache: close tar: %w", fErr)
	}
	return nil
}

// untar extracts a tar stream into destDir, rejecting paths that would
// escape it (zip-slip).
func untar(r io.Reader, destDir string) error {
	tr := tar.NewReader(r)
	//vmjail:lint:disable=forloopbound reason="bounded by the tar entry count"
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(destDir, hdr.Name)
		if err != nil {
			return err
		}
		if err := extractEntry(tr, hdr, target); err != nil {
			return err
		}
	}
}

// extractEntry writes one tar entry (dir or regular file) to target.
func extractEntry(tr io.Reader, hdr *tar.Header, target string) error {
	if hdr.Typeflag == tar.TypeDir {
		return os.MkdirAll(target, 0o750)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return err
	}
	//nolint:gosec // G304: target is safeJoin-confined under destDir.
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	//nolint:gosec // G110: tar is our own packed cache, not untrusted input.
	if _, err := io.Copy(out, tr); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// safeJoin joins name onto destDir, rejecting absolute paths or any that
// climb out of destDir via "..".
func safeJoin(destDir, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("egresscache: unsafe tar path %q", name)
	}
	return filepath.Join(destDir, clean), nil
}
