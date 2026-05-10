package imagestore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
)

// pushTrivialImage pushes a minimal image with one layer whose
// content is derived from `ref` so each call produces a distinct
// digest. Returns the pulled Image after Pull. Helper for manage
// tests that need several distinct images present.
func pushTrivialImage(t *testing.T, store *Store, ref string) *Image {
	t.Helper()
	layer := helloLayer(t, "ref="+ref+"\n")
	img, _ := mutate.AppendLayers(empty.Image, layer)
	img, _ = mutate.Config(img, v1.Config{Entrypoint: []string{testEntrypoint}})
	r, _ := name.ParseReference(ref)
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push: %v", err)
	}
	pulled, err := store.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	return pulled
}

func TestListReturnsEntriesNewestFirst(t *testing.T) {
	host := localRegistry(t)
	store, _ := New(t.TempDir(), 1<<30)
	clock := time.Unix(1000, 0)
	store.nowFn = func() time.Time { return clock }

	pushTrivialImage(t, store, host+"/test/a:v1")
	clock = clock.Add(time.Minute)
	pushTrivialImage(t, store, host+"/test/b:v1")

	entries, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if !entries[0].LastUsed.After(entries[1].LastUsed) {
		t.Errorf("not sorted newest-first: %v", entries)
	}
}

func TestRemoveDeletesEntryAndBundle(t *testing.T) {
	host := localRegistry(t)
	store, _ := New(t.TempDir(), 1<<30)
	pulled := pushTrivialImage(t, store, host+"/test/rm:v1")
	if _, err := store.Bundle(context.Background(), pulled.Digest, BundleOptions{}); err != nil {
		t.Fatalf("Bundle: %v", err)
	}

	if err := store.Remove(pulled.Digest); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	entries, _ := store.List()
	if len(entries) != 0 {
		t.Errorf("entries after Remove = %+v, want empty", entries)
	}
}

func TestRemoveIsIdempotentForUnknownDigest(t *testing.T) {
	store, _ := New(t.TempDir(), 1<<30)
	if err := store.Remove("sha256:0000000000000000000000000000000000000000000000000000000000000000"); err != nil {
		t.Errorf("Remove of unknown digest = %v, want nil", err)
	}
}

func TestRemoveErrorsWhenLocked(t *testing.T) {
	host := localRegistry(t)
	store, _ := New(t.TempDir(), 1<<30)
	pulled := pushTrivialImage(t, store, host+"/test/locked:v1")

	unlock, err := store.Lock(pulled.Digest)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer unlock()

	err = store.Remove(pulled.Digest)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want ErrLocked", err)
	}
}

func TestPruneRemovesStaleEntries(t *testing.T) {
	host := localRegistry(t)
	store, _ := New(t.TempDir(), 1<<30)
	clock := time.Unix(0, 0)
	store.nowFn = func() time.Time { return clock }

	clock = time.Unix(100, 0)
	pulledOld := pushTrivialImage(t, store, host+"/test/old:v1")
	clock = time.Unix(1000, 0)
	pulledNew := pushTrivialImage(t, store, host+"/test/new:v1")

	// Now() = 2000s; cutoff 1500s → threshold = t-cutoff = 500.
	// Old (100) is older than threshold, new (1000) is fresher.
	clock = time.Unix(2000, 0)
	pruned, err := store.Prune(1500 * time.Second)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !slices.Contains(pruned, pulledOld.Digest) {
		t.Errorf("pruned = %v, expected to include old digest %q", pruned, pulledOld.Digest)
	}
	if slices.Contains(pruned, pulledNew.Digest) {
		t.Errorf("pruned = %v, must not include fresh digest %q", pruned, pulledNew.Digest)
	}
}

func TestPruneSkipsLockedEntries(t *testing.T) {
	host := localRegistry(t)
	store, _ := New(t.TempDir(), 1<<30)
	clock := time.Unix(100, 0)
	store.nowFn = func() time.Time { return clock }

	pulled := pushTrivialImage(t, store, host+"/test/lockedstale:v1")
	unlock, err := store.Lock(pulled.Digest)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer unlock()

	clock = time.Unix(10000, 0)
	pruned, err := store.Prune(time.Second)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if slices.Contains(pruned, pulled.Digest) {
		t.Errorf("locked entry was pruned: %v", pruned)
	}
}

// TestPruneNoOpForZeroDuration covers the early-return path.
func TestPruneNoOpForZeroDuration(t *testing.T) {
	store, _ := New(t.TempDir(), 1<<30)
	pruned, err := store.Prune(0)
	if err != nil {
		t.Fatalf("Prune(0): %v", err)
	}
	if pruned != nil {
		t.Errorf("expected nil, got %v", pruned)
	}
}

// TestPruneNoStaleEntriesReturnsEmpty exercises Prune's "loop ran,
// nothing matched" path.
func TestPruneNoStaleEntriesReturnsEmpty(t *testing.T) {
	host := localRegistry(t)
	store, _ := New(t.TempDir(), 1<<30)
	clock := time.Unix(1000, 0)
	store.nowFn = func() time.Time { return clock }
	pushTrivialImage(t, store, host+"/test/fresh:v1")

	clock = time.Unix(1001, 0)
	pruned, err := store.Prune(time.Hour)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(pruned) != 0 {
		t.Errorf("pruned = %v, want empty", pruned)
	}
}

func TestListEmptyStore(t *testing.T) {
	store, _ := New(t.TempDir(), 1<<30)
	entries, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries = %+v, want empty", entries)
	}
}

func TestUsageEmptyStore(t *testing.T) {
	store, _ := New(t.TempDir(), 1<<30)
	usage, err := store.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if usage != 0 {
		t.Errorf("usage = %d, want 0", usage)
	}
}

// TestStoreNewRejectsEmptyRoot covers the validation path.
func TestStoreNewRejectsEmptyRoot(t *testing.T) {
	if _, err := New("", 0); err == nil {
		t.Fatal("expected error for empty root")
	}
}

// TestWriteJSONFailsWhenDirIsAFile covers writeJSON's WriteFile
// error path: passing a path whose parent isn't a directory makes
// the .tmp file write fail with ENOTDIR.
func TestWriteJSONFailsWhenDirIsAFile(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeJSON(filepath.Join(blocker, "child.json"), struct{}{}); err == nil {
		t.Fatal("expected writeJSON error when parent is a file")
	}
}

// TestWriteJSONFailsWhenTargetIsDir covers writeJSON's Rename
// error path: WriteFile of the .tmp succeeds, then Rename to the
// final path fails because that path is a non-empty directory.
func TestWriteJSONFailsWhenTargetIsDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "occupied")
	// Pre-create target as a non-empty directory so Rename over it fails.
	// #nosec G301 — test-only.
	if err := os.MkdirAll(filepath.Join(target, "child"), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeJSON(target, struct{}{}); err == nil {
		t.Fatal("expected writeJSON error when target is a non-empty dir")
	}
}

// TestLockFailsWhenLocksDirIsAFile covers the MkdirAll error path
// in Lock by pre-placing a regular file at the locks/ path so the
// `os.MkdirAll(locks/...)` call fails with ENOTDIR.
func TestLockFailsWhenLocksDirIsAFile(t *testing.T) {
	root := t.TempDir()
	store, err := New(root, 1<<30)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// New created locks/ as a directory; replace it with a regular
	// file so MkdirAll inside Lock can't resolve a parent.
	locksPath := filepath.Join(root, locksDir)
	if err := os.RemoveAll(locksPath); err != nil {
		t.Fatalf("rm locks dir: %v", err)
	}
	if err := os.WriteFile(locksPath, []byte("blocker"), 0o600); err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	if _, err := store.Lock("sha256:any"); err == nil {
		t.Fatal("expected Lock to fail when locks/ is a regular file")
	}
}

// TestLockFailsWhenLockfilePathIsADir covers the OpenFile error
// path in Lock by pre-placing a directory at the lockfile path.
func TestLockFailsWhenLockfilePathIsADir(t *testing.T) {
	root := t.TempDir()
	store, err := New(root, 1<<30)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	digest := "sha256:abc"
	// Put a directory where the lockfile would go.
	// #nosec G301 — test-only.
	if err := os.MkdirAll(store.lockPath(digest), 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.Lock(digest); err == nil {
		t.Fatal("expected Lock to fail when lockfile path is a directory")
	}
}

// TestStoreNewRejectsUncreateableRoot covers the MkdirAll error
// branch of New. Pointing at a path under a regular file makes the
// children of that path unmkdirable.
func TestStoreNewRejectsUncreateableRoot(t *testing.T) {
	parent := t.TempDir()
	blocker := parent + "/blocker"
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	if _, err := New(blocker+"/store", 0); err == nil {
		t.Fatal("expected error when root is uncreateable")
	}
}

// TestLoadMetaRejectsBadJSON covers the json.Unmarshal failure branch.
func TestLoadMetaRejectsBadJSON(t *testing.T) {
	root := t.TempDir()
	store, _ := New(root, 1<<30)
	if err := os.WriteFile(store.metaPath(), []byte("not valid json"), 0o644); err != nil {
		t.Fatalf("seed bad meta: %v", err)
	}
	if _, err := store.loadMeta(); err == nil {
		t.Fatal("expected error for malformed meta.json")
	}
}

// TestLoadMetaRejectsWrongVersion covers the version-check branch.
func TestLoadMetaRejectsWrongVersion(t *testing.T) {
	root := t.TempDir()
	store, _ := New(root, 1<<30)
	if err := os.WriteFile(store.metaPath(),
		[]byte(`{"version":999,"entries":[]}`), 0o644); err != nil {
		t.Fatalf("seed bad-version meta: %v", err)
	}
	if _, err := store.loadMeta(); err == nil {
		t.Fatal("expected error for wrong version")
	}
}

func TestUsageMatchesEntrySum(t *testing.T) {
	host := localRegistry(t)
	store, _ := New(t.TempDir(), 1<<30)
	pulled := pushTrivialImage(t, store, host+"/test/usage:v1")

	usage, err := store.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if usage != pulled.Size {
		t.Errorf("usage = %d, want %d (the only entry's size)", usage, pulled.Size)
	}
}
