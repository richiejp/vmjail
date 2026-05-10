package imagestore

import (
	"context"
	"errors"
	"os"
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
