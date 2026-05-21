package imagestore

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"

	"github.com/localai-org/darkaifactory/internal/oops"
	"github.com/localai-org/darkaifactory/internal/trace"
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
	// ExtraMounts is appended to the default OCI mounts (proc, dev,
	// sys). Used by the host orchestrator to bind-mount a writable
	// workspace from the VM's virtiofs share into the container.
	//
	// Each entry is encoded as-is into config.json. Caller is
	// responsible for picking a Destination that exists in the
	// container rootfs (or for ensuring crun's mount call creates
	// it via `nodev,nosuid,…` options when using bind mounts).
	ExtraMounts []BundleMount
	// HostUID / HostGID are the IDs container uid 0 / gid 0
	// map to via a one-entry user namespace. Required when the
	// bundle's files are served by an unprivileged virtiofsd —
	// the daemon performs every syscall as its own host uid, so
	// the container's uid must match the file owner for DAC
	// checks to pass. Zero / zero (the default) leaves the
	// user namespace and mappings off.
	HostUID int
	HostGID int
}

// BundleMount mirrors the subset of the OCI runtime-spec mount type
// we expose through BundleOptions. Marshalled into the runtime
// spec's JSON when present.
type BundleMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// Bundle materialises a runnable bundle for digest. Idempotent: if
// the bundle directory exists with config.json and rootfs/, it is
// reused. The image must already be in the layer cache (Pull first).
func (s *Store) Bundle(ctx context.Context, digest string, opts BundleOptions) (*Bundle, error) {
	ctx, span := trace.Start(ctx, "imagestore.Bundle",
		trace.WithAttributes(trace.KV{Key: "digest", Value: digest}))
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

	bundlePath := filepath.Join(s.Root, bundlesDir, digestPathFragment(digest))
	rootfs := filepath.Join(bundlePath, "rootfs")
	configPath := filepath.Join(bundlePath, "config.json")

	if isAssembled(configPath, rootfs) {
		s.touchEntry(digest)
		span.AddEvent("cached")
		return &Bundle{Digest: digest, Path: bundlePath}, nil
	}

	img, err := s.loadLayoutImage(digest)
	if err != nil {
		return nil, trace.RecordError(ctx, err)
	}

	// #nosec G301 — 0755 is intentional. The bundle dir is read by
	// virtiofsd (running as the daft-sandboxd user) and the VM's
	// kernel mount, which may use a different uid inside the
	// guest. World-execute lets crun's uid traverse this path
	// regardless of which user owns it.
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		return nil, trace.RecordError(ctx, fmt.Errorf("imagestore: mkdir rootfs: %w", err))
	}

	if err := unpackRootfs(img, rootfs); err != nil {
		return nil, trace.RecordError(ctx, err)
	}

	cfg, err := buildRuntimeConfig(img, opts)
	if err != nil {
		return nil, trace.RecordError(ctx, err)
	}
	if err := writeJSON(configPath, cfg); err != nil {
		return nil, trace.RecordError(ctx, err)
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
			// touchEntry is best-effort: a save failure here just
			// means LRU accuracy degrades by one entry. But "best
			// effort" doesn't mean "silent" — we still log the
			// error via oops, and any unexpected I/O failure
			// (permission, EROFS, etc.) will exit the process,
			// which is the correct response: the cache is in an
			// inconsistent state.
			oops.Must("imagestore: touch entry", s.saveMeta(meta))
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
	defer func() {
		// stream wraps the layer extractor; Close after a successful
		// untar should be a no-op. An error here means the
		// underlying tar reader was already drained funny — log
		// and continue, since untarTo has already returned.
		oops.Skip("imagestore: close mutate.Extract stream",
			stream.Close(), io.EOF, os.ErrClosed)
	}()
	return untarTo(stream, dst)
}

// untarTo reads a tar stream from r and writes its entries under dst.
// Files that would escape dst (via "../" or absolute path in the
// header) are rejected — defence against tar-bomb / path traversal.
func untarTo(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	//daft:lint:disable=forloopbound reason="exits on tar.Reader.Next returning io.EOF (clean end) or any other read error"
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("imagestore: tar read: %w", err)
		}
		target, err := safeJoin(dst, hdr.Name)
		if err != nil {
			return err
		}
		if err := writeTarEntry(tr, hdr, target, dst); err != nil {
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

func writeTarEntry(tr *tar.Reader, hdr *tar.Header, target, root string) error {
	// Every write path goes through an existing ancestor of
	// `target`. Nix-built images carry directory entries at
	// mode 0o555 (read-only); without relaxing those during
	// extraction, a later layer adding files under a Nix
	// store path fails with EACCES. The rootfs is mounted
	// readonly at runtime (see buildRuntimeConfig), so the
	// on-disk dir modes don't affect container security —
	// only this extraction.
	if err := ensureWritableAncestors(filepath.Dir(target), root); err != nil {
		return err
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		// #nosec G301 — see writableDirMode doc.
		if err := os.MkdirAll(target, writableDirMode); err != nil {
			return err
		}
		// Idempotent: a later layer re-emitting the same dir
		// with a different mode would otherwise leave a
		// read-only dir behind that blocks subsequent writes.
		return os.Chmod(target, writableDirMode)
	case tar.TypeReg:
		// #nosec G301 — same justification as the rootfs MkdirAll
		// above: 0755 lets crun's container uid traverse the path
		// regardless of who owns the bundle dir.
		if err := os.MkdirAll(filepath.Dir(target), writableDirMode); err != nil {
			return err
		}
		// #nosec G304 — `target` came through safeJoin which
		// guarantees it sits under the bundle dir. Tar entries are
		// the literal point of this function.
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, hdr.FileInfo().Mode().Perm())
		if err != nil {
			return err
		}
		if _, copyErr := io.Copy(f, tr); copyErr != nil {
			// Close on a partially-written file is informational —
			// the io.Copy error is what the caller needs. We still
			// surface a Close error if it happens for an unexpected
			// reason (e.g. ENOSPC on flush) via oops.Must, since a
			// silent half-written file is exactly the kind of bug
			// this policy exists to prevent.
			oops.Skip("bundle: close partial file", f.Close(),
				os.ErrClosed)
			return copyErr
		}
		return f.Close()
	case tar.TypeSymlink:
		// Remove may legitimately fail with ENOENT (no prior entry
		// to replace). Anything else — permission denied, EROFS,
		// EIO — is a real problem we can't recover from.
		oops.Skip("bundle: remove pre-existing symlink target",
			os.Remove(target), fs.ErrNotExist)
		return os.Symlink(hdr.Linkname, target)
	case tar.TypeLink:
		linkSource, err := safeJoin(filepath.Dir(target), hdr.Linkname)
		if err != nil {
			return err
		}
		oops.Skip("bundle: remove pre-existing hardlink target",
			os.Remove(target), fs.ErrNotExist)
		return os.Link(linkSource, target)
	default:
		// Devices, fifos, etc. are skipped — the rootfs runs
		// unprivileged inside the VM and can't usefully use them.
		return nil
	}
}

// writableDirMode is the mode used for every directory created
// during tar extraction. The image's tar-header dir modes are
// ignored: Nix-built images ship 0o555 dirs which would block
// subsequent layers from writing inside, and the runtime spec
// already marks the rootfs readonly at mount time so the on-disk
// owner perms don't affect container security.
const writableDirMode os.FileMode = 0o755

// ensureWritableAncestors walks up from dir to root and chmods
// any existing directory that doesn't already grant the daemon
// uid u+rwx. This is a no-op for greenfield extractions and only
// fires when a later layer needs to extend a directory tree that
// an earlier layer materialised read-only. Stops at root so we
// can't accidentally chmod the user's cache dir.
func ensureWritableAncestors(dir, root string) error {
	rootClean := filepath.Clean(root)
	dir = filepath.Clean(dir)
	//daft:lint:disable=forloopbound reason="terminates when dir reaches root or filesystem root; bounded by path depth"
	for {
		if dir == rootClean || !strings.HasPrefix(dir, rootClean+string(filepath.Separator)) {
			return nil
		}
		info, err := os.Stat(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// MkdirAll will create this with writableDirMode.
				dir = filepath.Dir(dir)
				continue
			}
			return err
		}
		if info.IsDir() && info.Mode().Perm()&0o700 != 0o700 {
			if err := os.Chmod(dir, info.Mode().Perm()|0o700); err != nil {
				return err
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}
