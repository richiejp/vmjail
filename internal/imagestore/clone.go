package imagestore

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/richiejp/darkaifactory/internal/oops"
)

// clonesDir holds per-dispatch copy-on-write clones of unpacked bundle
// rootfs, as a sibling of imagesDir/bundlesDir under Store.Root so a
// clone reflinks against the cached template on the same filesystem.
const clonesDir = "clones"

// CloneBundle makes an ephemeral copy-on-write clone of an unpacked
// bundle so a single dispatch can mutate its rootfs — both the guest's
// writes (a `nix develop` realising a closure into /nix/store) and the
// host-side provisioning writes (CA injection) — without ever touching
// the shared per-digest cache the template lives in. Without this the
// cached rootfs accumulates whatever the last workload left behind,
// which both breaks isolation between dispatches of the same image and
// makes the cache grow without bound.
//
// The store is expected to live on a reflink-capable filesystem (XFS,
// btrfs, or OpenZFS >= 2.2 with block_cloning enabled), where the copy
// shares data extents and costs O(metadata). cloneTree uses
// `--reflink=always`, so a store on a filesystem that can't reflink
// (ext4, tmpfs, a cross-device path) fails loudly here rather than
// silently full-copying gigabytes per dispatch. The returned cleanup
// removes the clone; callers defer it into teardown so the clone lives
// exactly as long as the dispatch.
//
// b must be a bundle returned by Bundle (its Path is the cached
// template). The clone preserves Digest/UID/GID and only redirects
// Path at the ephemeral copy.
func (s *Store) CloneBundle(b *Bundle) (*Bundle, func(), error) {
	parent := filepath.Join(s.Root, clonesDir)
	// #nosec G301 — same rationale as the bundle dir: virtiofsd and the
	// guest kernel mount traverse this path under possibly-different uids.
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, nil, fmt.Errorf("imagestore: mkdir clones dir: %w", err)
	}
	dir, err := os.MkdirTemp(parent, "d-")
	if err != nil {
		return nil, nil, fmt.Errorf("imagestore: mkdir clone: %w", err)
	}
	cleanup := func() {
		if err := forceRemoveAll(dir); err != nil {
			// Teardown callbacks predate clone cleanup and cannot return an
			// error. Do not hide a leak that can eventually exhaust the store.
			oops.Log("imagestore: remove clone "+dir, err)
		}
	}
	dst := filepath.Join(dir, filepath.Base(b.Path))
	if err := cloneTree(b.Path, dst); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("imagestore: clone bundle %s: %w", b.Digest, err)
	}
	return &Bundle{Digest: b.Digest, Path: dst, UID: b.UID, GID: b.GID}, cleanup, nil
}

// cloneTree reflink-copies the directory tree at src to dst (which must
// not yet exist). `cp -a` preserves perms, symlinks, and hardlinks — the
// last is load-bearing for a nix store, whose `/nix/store/.links` farm
// hardlinks identical files; a naive walk that dropped the hardlinks
// would balloon the copy. `--reflink=always` shares data extents and
// errors (rather than full-copying) if the filesystem can't reflink, so
// a misconfigured store surfaces loudly instead of silently costing a
// full byte copy per dispatch.
//
// Shelling out to coreutils `cp` is deliberate for this first cut: it
// gets reflink + hardlink-coalescing + xattrs right out of the box. A
// follow-up can replace it with a native walk over the FICLONE ioctl
// (golang.org/x/sys/unix.IoctlFileClone) to drop the process dependency.
func cloneTree(src, dst string) error {
	//nolint:gosec // G204: args are a constant flag set plus store-internal paths, not user input.
	cmd := exec.Command("cp", "--reflink=always", "-a", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cp --reflink=always -a %s %s: %w: %s", src, dst, err, out)
	}
	return nil
}

// forceRemoveAll removes the tree at path, first granting the owner
// write+exec on every directory so the unlink/rmdir of the read-only
// entries an unpacked image is full of can't EACCES — nix store paths
// are 0555 and `cp -a` preserves that, so a plain os.RemoveAll (which
// never chmods) fails partway through. This is the same chmod-then-unlink
// dance `rm -rf` does for write-protected dirs the caller owns; the clone
// is owned by the daemon uid that created it, so the chmods succeed.
func forceRemoveAll(path string) error {
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // tolerate races/perm while pre-chmodding; RemoveAll below reports the real failure.
		}
		if d.IsDir() {
			//nolint:gosec // G302: transient chmod to unlink a dir we're deleting; needs owner rwx (x to enter, w to unlink children).
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}
