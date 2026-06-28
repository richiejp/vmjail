package imagestore

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/richiejp/darkaifactory/internal/oops"
)

// Subdirectory names under Store.Root.
const (
	imagesDir  = "images"  // shared OCI image layout (layer cache)
	bundlesDir = "bundles" // unpacked rootfs per image digest
	locksDir   = "locks"   // bundle lockfiles (one per locked bundle)
	metaFile   = "meta.json"
)

// Store is a content-addressable image+bundle store rooted at Root.
// Use New to construct; the zero value is not safe for concurrent use.
type Store struct {
	// Root is the directory holding images/, bundles/, locks/, meta.json.
	Root string
	// Quota caps the layer cache in bytes. 0 disables enforcement.
	Quota int64

	// nowFn is the wall-clock used for LastUsed timestamps; tests
	// inject a fake clock.
	nowFn func() time.Time

	// mu serialises meta.json read-modify-writes within a single
	// process. Cross-process safety relies on the lockfiles, not
	// this mutex.
	mu sync.Mutex

	// hostedRT, when set, is the mTLS transport used to pull image refs
	// hosted on the orchestrator's embedded registry (ociregistry.Host).
	// Nil leaves every ref on the default keychain/transport. Wired by
	// the sandbox-host serve path; see UseHostedRegistry.
	hostedRT http.RoundTripper
}

// UseHostedRegistry wires the mTLS transport the Store uses to pull
// image refs hosted on the orchestrator's embedded registry. Refs that
// aren't orchestrator-hosted (ociregistry.IsHosted) stay on the default
// keychain. Passing nil disables the hosted path.
func (s *Store) UseHostedRegistry(rt http.RoundTripper) {
	s.hostedRT = rt
}

// New returns a Store rooted at root. The root directory is created if
// missing. quota is the byte limit on the layer cache; pass 0 to
// disable enforcement.
func New(root string, quota int64) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("imagestore: root is required")
	}
	for _, sub := range []string{imagesDir, bundlesDir, locksDir} {
		// #nosec G301 — store subdirs (images, bundles, locks) are
		// traversed by the daft-sandboxd subprocesses including
		// virtiofsd which may run as a different uid.
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			return nil, fmt.Errorf("imagestore: mkdir %s: %w", sub, err)
		}
	}
	// Per-dispatch CoW clones (clonesDir) are ephemeral: each is removed
	// by its dispatch's teardown. Any clone still on disk at startup is an
	// orphan from a previous process that was hard-killed before its
	// teardown ran (a SIGKILL skips Go defers), so sweep them here — New
	// runs once per sandboxd start, before any dispatch, and a node owns
	// its store exclusively. Best-effort: a sweep failure is logged, not
	// fatal, since a stale clone wastes disk but doesn't break boot.
	if err := forceRemoveAll(filepath.Join(root, clonesDir)); err != nil {
		oops.Log("imagestore: sweep stale clones", err)
	}
	return &Store{
		Root:  root,
		Quota: quota,
		nowFn: time.Now,
	}, nil
}

// metadata is the on-disk shape of meta.json. Versioned so future
// schema changes can refuse to read incompatible files instead of
// silently corrupting data.
type metadata struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// metadataVersion bumps when the on-disk format changes in a
// non-backward-compatible way.
const metadataVersion = 1

func (s *Store) metaPath() string { return filepath.Join(s.Root, metaFile) }

// loadMeta reads meta.json. A missing file is not an error — it
// represents an empty store, which is the legitimate cold-start state.
func (s *Store) loadMeta() (metadata, error) {
	b, err := os.ReadFile(s.metaPath())
	if err != nil {
		if os.IsNotExist(err) {
			return metadata{Version: metadataVersion}, nil
		}
		return metadata{}, fmt.Errorf("imagestore: read meta: %w", err)
	}
	var m metadata
	if err := json.Unmarshal(b, &m); err != nil {
		return metadata{}, fmt.Errorf("imagestore: parse meta: %w", err)
	}
	if m.Version != metadataVersion {
		return metadata{}, fmt.Errorf(
			"imagestore: meta.json version %d, want %d",
			m.Version, metadataVersion,
		)
	}
	return m, nil
}

// saveMeta writes meta.json atomically. Delegates the marshal+write+
// rename dance to writeJSON (defined in runtimespec.go) so we have
// one atomic-JSON-write code path across the package.
func (s *Store) saveMeta(m metadata) error {
	m.Version = metadataVersion
	return writeJSON(s.metaPath(), m)
}

// applyLocks decorates entries with their current Locked state by
// checking for lockfiles. Pure file existence check; callers
// Lock/Unlock to populate the directory.
func (s *Store) applyLocks(entries []Entry) []Entry {
	out := make([]Entry, len(entries))
	for i, e := range entries {
		out[i] = e
		if _, err := os.Stat(s.lockPath(e.Digest)); err == nil {
			out[i].Locked = true
		}
	}
	return out
}
