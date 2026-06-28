package imagestore

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// requireReflink skips the calling test when the temp filesystem can't
// reflink, since CloneBundle uses `cp --reflink=always`. Production
// stores live on XFS/btrfs/ZFS; a CI runner whose TMPDIR is tmpfs or
// ext4 would otherwise fail a test that has nothing to do with the code
// under test.
func requireReflink(t *testing.T) {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "a"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed reflink probe: %v", err)
	}
	//nolint:gosec // G204: constant flags + TempDir paths, test-only.
	if out, err := exec.Command("cp", "--reflink=always", filepath.Join(d, "a"), filepath.Join(d, "b")).CombinedOutput(); err != nil {
		t.Skipf("test filesystem does not support reflink: %v: %s", err, out)
	}
}

// newTestStore returns a Store rooted at a temp dir. Quota disabled —
// these tests exercise CloneBundle, not eviction.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir(), 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// seedTemplateBundle lays down a minimal cached-bundle dir under the
// store (config.json + rootfs with one file) and returns the *Bundle a
// dispatch would have got from Bundle().
func seedTemplateBundle(t *testing.T, s *Store) *Bundle {
	t.Helper()
	dir := filepath.Join(s.Root, bundlesDir, "sha256_deadbeef")
	rootfs := filepath.Join(dir, "rootfs", "nix", "store")
	if err := os.MkdirAll(rootfs, 0o750); err != nil {
		t.Fatalf("mkdir rootfs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "pkg"), []byte("orig"), 0o644); err != nil {
		t.Fatalf("write pkg: %v", err)
	}
	return &Bundle{Digest: "sha256:deadbeef", Path: dir, UID: 1000, GID: 1000}
}

func TestCloneBundleIsolatesWrites(t *testing.T) {
	requireReflink(t)
	s := newTestStore(t)
	tmpl := seedTemplateBundle(t, s)

	clone, cleanup, err := s.CloneBundle(tmpl)
	if err != nil {
		t.Fatalf("CloneBundle: %v", err)
	}
	defer cleanup()

	// The clone is a distinct path that preserves identity metadata.
	if clone.Path == tmpl.Path {
		t.Fatalf("clone shares the template path %s", clone.Path)
	}
	if clone.Digest != tmpl.Digest || clone.UID != tmpl.UID || clone.GID != tmpl.GID {
		t.Errorf("clone metadata = %+v, want digest/uid/gid from %+v", clone, tmpl)
	}
	// It lives under the clones dir (so buildShares exports a per-dispatch
	// parent, not the shared cache), and keeps the template's basename so
	// the guest's `--bundle /bundles/<base>` resolves.
	if filepath.Base(clone.Path) != filepath.Base(tmpl.Path) {
		t.Errorf("clone basename = %s, want %s", filepath.Base(clone.Path), filepath.Base(tmpl.Path))
	}
	if got := filepath.Base(filepath.Dir(filepath.Dir(clone.Path))); got != clonesDir {
		t.Errorf("clone parent = %s, want under %s", got, clonesDir)
	}

	// Content copied through.
	clonePkg := filepath.Join(clone.Path, "rootfs", "nix", "store", "pkg")
	if b, _ := os.ReadFile(clonePkg); string(b) != "orig" { //nolint:gosec // test path under TempDir.
		t.Fatalf("clone pkg = %q, want orig", b)
	}

	// Mutating the clone must not touch the template (isolation).
	if err := os.WriteFile(clonePkg, []byte("mutated"), 0o644); err != nil {
		t.Fatalf("write clone pkg: %v", err)
	}
	tmplPkg := filepath.Join(tmpl.Path, "rootfs", "nix", "store", "pkg")
	if b, _ := os.ReadFile(tmplPkg); string(b) != "orig" { //nolint:gosec // test path under TempDir.
		t.Errorf("template pkg = %q after clone write, want orig (cache mutated!)", b)
	}
}

func TestCloneBundleCleanupRemovesReadOnlyTree(t *testing.T) {
	requireReflink(t)
	s := newTestStore(t)
	tmpl := seedTemplateBundle(t, s)
	// Make a store path read-only (0555), as a real nix store is — this is
	// what defeats a plain os.RemoveAll and exercises forceRemoveAll.
	roDir := filepath.Join(tmpl.Path, "rootfs", "nix", "store", "pkg-1.0")
	if err := os.MkdirAll(roDir, 0o750); err != nil {
		t.Fatalf("mkdir ro dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(roDir, "f"), []byte("x"), 0o444); err != nil {
		t.Fatalf("write ro file: %v", err)
	}
	if err := os.Chmod(roDir, 0o555); err != nil { //nolint:gosec // G302: deliberately read-only, mimicking a nix store path.
		t.Fatalf("chmod ro dir: %v", err)
	}
	// Restore writability before the framework's TempDir cleanup, which
	// (like a plain RemoveAll) can't unlink under a 0555 dir.
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o750) }) //nolint:gosec // G302: dir needs owner rwx so the framework can clean it.

	clone, cleanup, err := s.CloneBundle(tmpl)
	if err != nil {
		t.Fatalf("CloneBundle: %v", err)
	}
	if _, err := os.Stat(clone.Path); err != nil {
		t.Fatalf("clone missing after create: %v", err)
	}
	cleanup()
	// cleanup removes the whole per-dispatch dir despite the read-only
	// store path inside (the grandparent of the clone basename).
	if _, err := os.Stat(clone.Path); !os.IsNotExist(err) {
		t.Errorf("clone still present after cleanup: stat err = %v", err)
	}
}

func TestNewSweepsOrphanClones(t *testing.T) {
	root := t.TempDir()
	s, err := New(root, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Simulate an orphan clone left by a hard-killed prior process.
	orphan := filepath.Join(s.Root, clonesDir, "d-orphan", "sha256_x", "rootfs")
	if err := os.MkdirAll(orphan, 0o750); err != nil {
		t.Fatalf("seed orphan clone: %v", err)
	}
	// Re-opening the store (a fresh sandboxd start) sweeps it.
	if _, err := New(root, 0); err != nil {
		t.Fatalf("New (reopen): %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, clonesDir, "d-orphan")); !os.IsNotExist(err) {
		t.Errorf("orphan clone survived startup sweep: stat err = %v", err)
	}
}

func TestCloneBundleMissingTemplate(t *testing.T) {
	s := newTestStore(t)
	bad := &Bundle{Digest: "sha256:nope", Path: filepath.Join(s.Root, bundlesDir, "absent")}
	_, _, err := s.CloneBundle(bad)
	if err == nil {
		t.Fatal("CloneBundle of a missing template: want error, got nil")
	}
}
