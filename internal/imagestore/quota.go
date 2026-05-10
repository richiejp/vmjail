// Package imagestore is a content-addressable image+bundle store for
// the sandbox-host. It pulls OCI images via go-containerregistry,
// caches their layer blobs in a single shared OCI image layout, and
// produces unpacked bundles that crun consumes inside the VM.
//
// Two storage tiers, two cleanup policies (per PLAN.md "Container
// image distribution"):
//
//   - Layer cache (the OCI layout): the bandwidth-saver. Bounded by a
//     byte quota with LRU eviction. Default DefaultQuota = 50 GiB.
//   - Bundle cache (unpacked rootfs): derived state, refcount-locked
//     while a VM is using it, otherwise GC-able on demand.
//
// The pure decision logic — eviction order, capacity checks, the
// "single image > quota" guardrail — lives in this file with no I/O,
// so it can be exhaustively unit-tested. The plumbing that maps it
// onto the on-disk OCI layout lives in store.go / pull.go.
package imagestore

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// DefaultQuota is the byte ceiling on the layer cache when a Store is
// constructed without an explicit Quota. 50 GiB matches the default
// the user requested in the slice plan: enough room for "a few base
// images plus a workspace's worth of agent runs", small enough that
// it doesn't silently fill a developer laptop's disk.
const DefaultQuota int64 = 50 << 30 // 50 GiB

// ErrImageExceedsQuota is returned by Allocate when a single image's
// total size is larger than the entire quota. We refuse to evict the
// rest of the cache to make room for one image we still couldn't
// fit, so the caller gets a clear error before any I/O.
var ErrImageExceedsQuota = errors.New("imagestore: image size exceeds quota")

// ErrCannotFreeEnough is returned by Allocate when even after evicting
// every unlocked image we couldn't free enough room. Distinct from
// ErrImageExceedsQuota because the caller's remediation differs:
// ExceedsQuota → raise the quota or pick a smaller image; CannotFree
// → release locked bundles, then retry.
var ErrCannotFreeEnough = errors.New("imagestore: cannot free enough space; locked bundles in the way")

// Entry describes one image's footprint for quota accounting. Stored
// in the on-disk meta.json and rebuilt from disk if the meta is
// missing.
type Entry struct {
	// Digest is the canonical image digest (sha256:...). Identifies
	// the image uniquely; two refs pointing at the same digest share
	// one Entry.
	Digest string `json:"digest"`
	// Refs are the human-readable refs (e.g. "alpine:3.19") that
	// resolve to Digest. A single image can have several. Empty
	// after a remote tag move that left the digest orphaned.
	Refs []string `json:"refs,omitempty"`
	// Size is the total bytes of all layer blobs unique to this
	// image. Sum across Entries can exceed the on-disk total
	// because shared blobs count once per image — the quota check
	// uses the on-disk-unique total, not the sum here.
	Size int64 `json:"size"`
	// LastUsed is the most recent access (pull or bundle build).
	// LRU eviction picks the smallest LastUsed.
	LastUsed time.Time `json:"last_used"`
	// Locked indicates a VM currently has a bundle from this image
	// open. Eviction skips locked entries.
	Locked bool `json:"-"`
}

// Plan is the eviction recommendation Allocate produces: zero or more
// existing entries to delete to free room for a new image. It's a
// value type so the caller can inspect / log / commit it
// independently of the disk operations.
type Plan struct {
	// Evict lists Entry digests to delete, in order. Empty if no
	// eviction is needed.
	Evict []string
	// Freed is the total bytes the eviction will release.
	Freed int64
}

// Allocate decides how to make room for an incoming image of size
// `incoming` given the current Entries and the configured quota.
//
// Decision tree:
//
//  1. If quota == 0, no limit. Return empty plan.
//  2. If incoming > quota, return ErrImageExceedsQuota — refuse to
//     start a download we can't possibly keep.
//  3. If sum(entries.Size) + incoming <= quota, no eviction needed.
//  4. Otherwise build an LRU-ordered list of unlocked entries, evict
//     until current + incoming <= quota.
//  5. If we can't free enough (because locked entries hold the
//     remaining headroom), return ErrCannotFreeEnough.
//
// The function is pure — entries is read-only, no clock is consulted,
// no disk I/O. Caller commits the returned Plan.
func Allocate(entries []Entry, incoming, quota int64) (Plan, error) {
	if quota <= 0 {
		return Plan{}, nil
	}
	if incoming < 0 {
		return Plan{}, fmt.Errorf("imagestore: incoming size %d is negative", incoming)
	}
	if incoming > quota {
		return Plan{}, fmt.Errorf("%w: image %d bytes > quota %d bytes",
			ErrImageExceedsQuota, incoming, quota)
	}

	var current int64
	for _, e := range entries {
		current += e.Size
	}
	if current+incoming <= quota {
		return Plan{}, nil
	}

	// Sort unlocked entries by LastUsed ascending; oldest first.
	candidates := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.Locked {
			continue
		}
		candidates = append(candidates, e)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].LastUsed.Before(candidates[j].LastUsed)
	})

	var plan Plan
	target := current + incoming - quota
	for _, c := range candidates {
		if plan.Freed >= target {
			break
		}
		plan.Evict = append(plan.Evict, c.Digest)
		plan.Freed += c.Size
	}
	if plan.Freed < target {
		return Plan{}, fmt.Errorf("%w: need %d bytes, can only free %d",
			ErrCannotFreeEnough, target, plan.Freed)
	}
	return plan, nil
}

// FilterOlderThan returns the digests of entries whose LastUsed is
// older than `now - cutoff`. Locked entries are skipped — Prune is a
// best-effort cleanup, not a forced kill.
func FilterOlderThan(entries []Entry, cutoff time.Duration, now time.Time) []string {
	if cutoff <= 0 {
		return nil
	}
	threshold := now.Add(-cutoff)
	var stale []string
	for _, e := range entries {
		if e.Locked {
			continue
		}
		if e.LastUsed.Before(threshold) {
			stale = append(stale, e.Digest)
		}
	}
	return stale
}
