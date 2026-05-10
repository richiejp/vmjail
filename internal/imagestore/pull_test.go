package imagestore

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

// localRegistry spins up an in-process OCI registry for the duration
// of the test. Returned URL is "host:port" (no scheme) — pass it to
// crane functions that take a name. Closes automatically on t.Cleanup.
func localRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse registry URL: %v", err)
	}
	return u.Host
}

// pushTestImage assembles a synthetic image of `layerSize` random
// bytes per layer and pushes it to ref. Returns the resolved digest.
// Layer count = 1 (enough to exercise the size accounting).
func pushTestImage(t *testing.T, ref string, layerSize int64) string {
	t.Helper()
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatalf("parse ref %q: %v", ref, err)
	}
	layer, err := random.Layer(layerSize, "application/vnd.oci.image.layer.v1.tar+gzip")
	if err != nil {
		t.Fatalf("random layer: %v", err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatalf("append layer: %v", err)
	}
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push: %v", err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return d.String()
}

func TestPullStoresImageAndUpdatesMeta(t *testing.T) {
	host := localRegistry(t)
	ref := host + "/test/hello:v1"
	digest := pushTestImage(t, ref, 1024)

	store, err := New(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got, err := store.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if got.Digest != digest {
		t.Errorf("digest = %q, want %q", got.Digest, digest)
	}
	if got.Size <= 0 {
		t.Errorf("size = %d, want > 0", got.Size)
	}

	// Layout was created.
	if _, err := os.Stat(filepath.Join(store.Root, imagesDir, "oci-layout")); err != nil {
		t.Errorf("oci-layout missing: %v", err)
	}

	// meta.json has the entry.
	meta, err := store.loadMeta()
	if err != nil {
		t.Fatalf("loadMeta: %v", err)
	}
	if len(meta.Entries) != 1 || meta.Entries[0].Digest != digest {
		t.Errorf("entries = %+v, want one matching %q", meta.Entries, digest)
	}
}

func TestPullIsIdempotentForSameRef(t *testing.T) {
	host := localRegistry(t)
	ref := host + "/test/idemp:v1"
	pushTestImage(t, ref, 512)

	store, err := New(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first, err := store.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("first pull: %v", err)
	}
	second, err := store.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("second pull: %v", err)
	}
	if first.Digest != second.Digest {
		t.Errorf("digest changed across pulls: %q vs %q", first.Digest, second.Digest)
	}

	meta, _ := store.loadMeta()
	if len(meta.Entries) != 1 {
		t.Errorf("entries = %d, want 1 after idempotent pull", len(meta.Entries))
	}
}

// TestPullRejectsImageOverQuota checks the user-requested guardrail:
// a single image larger than the entire quota is refused before any
// network I/O reaches the OCI layout writer.
func TestPullRejectsImageOverQuota(t *testing.T) {
	host := localRegistry(t)
	ref := host + "/test/big:v1"
	pushTestImage(t, ref, 4096)

	// Tiny quota — image will be at least the layer size.
	store, err := New(t.TempDir(), 100)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = store.Pull(context.Background(), ref)
	if !errors.Is(err, ErrImageExceedsQuota) {
		t.Fatalf("err = %v, want ErrImageExceedsQuota", err)
	}
	// Nothing should have been written.
	meta, _ := store.loadMeta()
	if len(meta.Entries) != 0 {
		t.Errorf("entries = %+v, want empty after over-quota refusal", meta.Entries)
	}
}

// TestPullEvictsLRUWhenAddingNewImageOverflows is the integration of
// quota.Allocate's eviction with disk I/O: pull image A, mark it
// older, then pull image B which only fits if A is evicted.
func TestPullEvictsLRUWhenAddingNewImageOverflows(t *testing.T) {
	host := localRegistry(t)
	refA := host + "/test/a:v1"
	refB := host + "/test/b:v1"
	pushTestImage(t, refA, 4096)
	pushTestImage(t, refB, 4096)

	root := t.TempDir()
	// Quota of 1 image's worth: pulling B must evict A.
	clock := time.Unix(1000, 0)
	store, err := New(root, 8*1024)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store.nowFn = func() time.Time { return clock }

	if _, err := store.Pull(context.Background(), refA); err != nil {
		t.Fatalf("pull A: %v", err)
	}
	clock = clock.Add(time.Minute)
	if _, err := store.Pull(context.Background(), refB); err != nil {
		t.Fatalf("pull B: %v", err)
	}

	meta, _ := store.loadMeta()
	if len(meta.Entries) != 1 {
		t.Fatalf("entries after eviction = %+v, want exactly one (the newer one)",
			meta.Entries)
	}
	if !strings.Contains(meta.Entries[0].Refs[0], "/b:v1") {
		t.Errorf("surviving entry refs = %v, want the B image", meta.Entries[0].Refs)
	}
}

// TestPullWithFakePullerExercisesQuotaCheckOnly: a fake puller with
// canned (image, ref) lets us exercise quota / metadata / disk paths
// without the in-process registry, which is much faster.
func TestPullWithFakePullerExercisesQuotaCheckOnly(t *testing.T) {
	store, err := New(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	ref, err := name.ParseReference("fake.local/test/x:v1")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p := &fakePuller{img: img, ref: ref}

	got, err := store.pullWith(context.Background(), "fake.local/test/x:v1", p)
	if err != nil {
		t.Fatalf("pullWith: %v", err)
	}
	d, _ := img.Digest()
	if got.Digest != d.String() {
		t.Errorf("digest = %q, want %q", got.Digest, d.String())
	}
}

// TestPullSkipsAllocationForExistingDigest verifies the
// already-cached short-circuit: a re-pull at full quota succeeds
// because the existing digest is treated as a 0-byte allocation.
func TestPullSkipsAllocationForExistingDigest(t *testing.T) {
	host := localRegistry(t)
	ref := host + "/test/cached:v1"
	pushTestImage(t, ref, 4096)

	// Quota only fits one image. Pull twice — the second pull
	// should not error even though the cache is "full", because
	// the digest is already present.
	store, _ := New(t.TempDir(), 8*1024)
	first, err := store.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("first pull: %v", err)
	}
	second, err := store.Pull(context.Background(), ref)
	if err != nil {
		t.Fatalf("second pull (cached): %v", err)
	}
	if first.Digest != second.Digest {
		t.Errorf("second pull returned different digest: %q vs %q",
			first.Digest, second.Digest)
	}
}

// TestPullPropagatesAllocateError uses a fake puller that returns
// a v1.Image whose layer reports an enormous size — pullWith should
// hit Allocate's "image > quota" guardrail and bail before any disk
// writes happen.
//
// We verify by checking that no OCI layout was created.
func TestPullPropagatesAllocateError(t *testing.T) {
	host := localRegistry(t)
	ref := host + "/test/big:v1"
	pushTestImage(t, ref, 4096)

	root := t.TempDir()
	store, _ := New(root, 100) // tiny quota
	_, err := store.Pull(context.Background(), ref)
	if !errors.Is(err, ErrImageExceedsQuota) {
		t.Fatalf("err = %v, want ErrImageExceedsQuota", err)
	}
	// Layout should not exist — Pull bailed before saveImage.
	if _, statErr := os.Stat(filepath.Join(root, imagesDir, "oci-layout")); statErr == nil {
		t.Error("oci-layout was created despite quota refusal")
	}
}

func TestPullPropagatesPullerError(t *testing.T) {
	store, err := New(t.TempDir(), 1<<30)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := errors.New("synthetic resolve failure")
	p := &fakePuller{err: want}
	_, err = store.pullWith(context.Background(), "x", p)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

type fakePuller struct {
	img v1.Image
	ref name.Reference
	err error
}

func (f *fakePuller) resolve(_ context.Context, _ string) (v1.Image, name.Reference, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.img, f.ref, nil
}
