package imagestore

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/richiejp/darkaifactory/internal/trace"
)

// remoteKeychain authenticates pulls. DefaultKeychain reads
// ~/.docker/config.json, $DOCKER_CONFIG, and the standard cloud
// keychains. Once the orchestrator-bundled registry lands we'll wrap
// this with our mTLS-cert-derived auth.
var remoteKeychain = authn.DefaultKeychain

// Image is the result of a successful Pull.
type Image struct {
	// Ref is the human-readable reference the caller asked for, e.g.
	// "alpine:3.19" or "ghcr.io/owner/repo@sha256:…". Preserved so
	// `daft images ls` can show what the user actually typed.
	Ref string
	// Digest is the canonical sha256 digest the ref resolved to.
	// Persisted as the primary key in meta.json; bundle directory
	// name; lookup key for Bundle().
	Digest string
	// Size is the total bytes of the image's layers (sum of compressed
	// layer sizes from the manifest). What the quota counts against.
	Size int64
}

// puller is the network-talking subset of go-containerregistry the
// Store needs. Real impl uses remote.Image; tests replace it with a
// fake that returns canned v1.Image values, so the quota / disk /
// metadata logic can be exercised without spinning up a registry.
type puller interface {
	resolve(ctx context.Context, ref string) (v1.Image, name.Reference, error)
}

// remotePuller is the production puller. It resolves refs against the
// configured registry (Docker Hub by default; the orchestrator-bundled
// registry once that lands) using the calling user's keychain.
type remotePuller struct{}

func (remotePuller) resolve(ctx context.Context, ref string) (v1.Image, name.Reference, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, nil, fmt.Errorf("imagestore: parse %q: %w", ref, err)
	}
	img, err := remote.Image(
		r,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(remoteKeychain),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("imagestore: fetch %q: %w", ref, err)
	}
	return img, r, nil
}

// Pull fetches ref into the store, evicting LRU images first if the
// new image would push the cache over its quota. Returns the resolved
// Image record (digest, size, ref). Idempotent: re-pulling the same
// ref refreshes its LastUsed and returns the existing entry.
//
// Errors map onto the user's remediation:
//   - ErrImageExceedsQuota   raise quota or pick a smaller image
//   - ErrCannotFreeEnough    release locked bundles, then retry
//   - anything else          network / registry / I/O failure
func (s *Store) Pull(ctx context.Context, ref string) (*Image, error) {
	return s.pullWith(ctx, ref, remotePuller{})
}

// pullWith is Pull parameterised on the puller; production calls Pull,
// tests inject fakes.
func (s *Store) pullWith(ctx context.Context, ref string, p puller) (*Image, error) {
	ctx, span := trace.Start(ctx, "imagestore.Pull",
		trace.WithAttributes(trace.KV{Key: "ref", Value: ref}))
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()

	img, parsedRef, err := p.resolve(ctx, ref)
	if err != nil {
		return nil, trace.RecordError(ctx, err)
	}

	digest, err := img.Digest()
	if err != nil {
		return nil, trace.RecordError(ctx, fmt.Errorf("imagestore: digest: %w", err))
	}
	digestStr := digest.String()
	span.SetAttribute("digest", digestStr)

	size, err := imageSize(img)
	if err != nil {
		return nil, trace.RecordError(ctx, err)
	}
	span.SetAttribute("size_bytes", strconv.FormatInt(size, 10))

	meta, err := s.loadMeta()
	if err != nil {
		return nil, trace.RecordError(ctx, err)
	}
	entries := s.applyLocks(meta.Entries)

	// Treat "we already have this digest" as a 0-byte allocation —
	// it should always succeed even at full quota.
	allocSize := size
	if hasDigest(entries, digestStr) {
		allocSize = 0
	}

	plan, err := Allocate(entries, allocSize, s.Quota)
	if err != nil {
		return nil, trace.RecordError(ctx, err)
	}

	for _, d := range plan.Evict {
		if err := s.removeOnDisk(d); err != nil {
			return nil, trace.RecordError(ctx, fmt.Errorf("imagestore: evict %s: %w", d, err))
		}
		meta.Entries = removeEntry(meta.Entries, d)
	}

	layoutPath := filepath.Join(s.Root, imagesDir)
	if err := saveImage(layoutPath, img, parsedRef); err != nil {
		return nil, trace.RecordError(ctx, err)
	}

	now := s.nowFn()
	meta.Entries = upsertEntry(meta.Entries, Entry{
		Digest:   digestStr,
		Refs:     []string{parsedRef.String()},
		Size:     size,
		LastUsed: now,
	})
	if err := s.saveMeta(meta); err != nil {
		return nil, trace.RecordError(ctx, err)
	}

	return &Image{
		Ref:    parsedRef.String(),
		Digest: digestStr,
		Size:   size,
	}, nil
}

// imageSize sums the compressed sizes of all layers — what the
// registry charges us for and what we should charge against the
// quota. Manifest size and config size are negligible (KB, not MB).
func imageSize(img v1.Image) (int64, error) {
	layers, err := img.Layers()
	if err != nil {
		return 0, fmt.Errorf("imagestore: layers: %w", err)
	}
	var total int64
	for _, l := range layers {
		sz, err := l.Size()
		if err != nil {
			return 0, fmt.Errorf("imagestore: layer size: %w", err)
		}
		total += sz
	}
	return total, nil
}

// saveImage writes img into the OCI layout at layoutPath, indexing it
// under the original ref so layout.AppendImage's annotations let us
// find it again. Layers that already exist as blobs in the layout are
// not re-written — that's how the layer cache deduplicates.
func saveImage(layoutPath string, img v1.Image, ref name.Reference) error {
	p, err := layout.FromPath(layoutPath)
	if err != nil {
		// Layout doesn't exist yet — bootstrap it.
		p, err = layout.Write(layoutPath, empty.Index)
		if err != nil {
			return fmt.Errorf("imagestore: init layout: %w", err)
		}
	}
	annotations := map[string]string{
		"org.opencontainers.image.ref.name": ref.String(),
	}
	if err := p.AppendImage(img, layout.WithAnnotations(annotations)); err != nil {
		return fmt.Errorf("imagestore: append image: %w", err)
	}
	return nil
}

// removeOnDisk deletes the bundle for digest (if any) and lets the
// next layout traversal GC orphan blobs. The OCI layout doesn't have a
// built-in "delete this image" op — we just drop it from index.json.
func (s *Store) removeOnDisk(digest string) error {
	bundleDir := filepath.Join(s.Root, bundlesDir, digestPathFragment(digest))
	if err := os.RemoveAll(bundleDir); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Future: walk the layout's index.json and prune the manifest
	// entry for digest, then GC orphan blobs. For v0 we leave layer
	// blobs in place — they're shared, and the next quota check will
	// see them as unowned and re-evaluate. (TODO before this slice
	// can claim production-readiness.)
	return nil
}

// digestPathFragment turns "sha256:abcd..." into a filesystem-safe
// directory name. The colon separator becomes an underscore so the
// digest is reversible by sight, and a future multi-algo world
// doesn't collide. strings.Cut on a malformed digest yields algo=raw
// and hex="" — `algo+"_"` is a deterministic, traceable path either
// way, so we don't bother with a dead error branch.
func digestPathFragment(digest string) string {
	algo, hex, _ := strings.Cut(digest, ":")
	return algo + "_" + hex
}

func hasDigest(entries []Entry, digest string) bool {
	for _, e := range entries {
		if e.Digest == digest {
			return true
		}
	}
	return false
}

func removeEntry(entries []Entry, digest string) []Entry {
	out := entries[:0]
	for _, e := range entries {
		if e.Digest == digest {
			continue
		}
		out = append(out, e)
	}
	return out
}

func upsertEntry(entries []Entry, e Entry) []Entry {
	for i := range entries {
		if entries[i].Digest != e.Digest {
			continue
		}
		// Merge: preserve unique refs, refresh size + LastUsed.
		entries[i].Size = e.Size
		entries[i].LastUsed = e.LastUsed
		for _, r := range e.Refs {
			if !slices.Contains(entries[i].Refs, r) {
				entries[i].Refs = append(entries[i].Refs, r)
			}
		}
		return entries
	}
	return append(entries, e)
}
