// Package httpcache is a content-addressed HTTP response cache. It
// stores response bodies in an internal/cas.Store (sha256 + zstd,
// verified on read) and keeps a small on-disk JSON index mapping a
// caller-chosen request key to {body digest, status, headers}.
//
// It is the storage half of PLAN.md Phase I (caching/pinning egress
// proxy), slice I1: record/replay. The daft-mitm-proxy consults it
// around its upstream call so a recorded run can be replayed offline
// — de-flaking the integration suite's dependency on reachable
// package hosts. The cache is deliberately content-agnostic: it
// reuses the same blob primitive as workspace sync and replaces the
// workspace Manifest with this flat key -> Entry index.
//
// Caching policy (what is safe to cache) lives in the caller; this
// package only stores and retrieves what it is given. The one
// transformation it makes is dropping hop-by-hop / framing headers
// at Put time so a replayed response is reconstructed cleanly from a
// known body length rather than a stale Transfer-Encoding.
package httpcache

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"sync"

	"github.com/localai-org/darkaifactory/internal/cas"
	"github.com/localai-org/darkaifactory/internal/oops"
)

// indexName is the JSON index file kept alongside the blob store
// under the cache dir.
const indexName = "index.json"

// blobsSubdir is the cas.Store root under the cache dir. Keeping it
// in a subdir leaves the cache dir's top level for the index (and
// any future metadata) without colliding with the CAS fan-out.
const blobsSubdir = "blobs"

// dirPerm: the cache is private to the user, same stance as the CAS.
// (Index files inherit os.CreateTemp's 0o600 on Unix — no Chmod.)
const dirPerm = 0o700

// Seams so persistLocked's fs error paths can be exercised without
// invasive mocking. Same pattern as cmd/daft-mitm-proxy's crypto
// seams.
//
//nolint:gochecknoglobals // test seams.
var (
	createTempFn = os.CreateTemp
	renameFn     = os.Rename
)

// hopByHopHeaders are dropped at Put time. They describe the
// connection or the wire framing of one specific response, not the
// payload, so replaying them verbatim against a different body
// length would corrupt the stream. On replay the caller sets
// Content-Length from Entry.Size.
//
//nolint:gochecknoglobals // effectively const; a set literal.
var hopByHopHeaders = map[string]struct{}{
	"Connection":          {},
	"Proxy-Connection":    {},
	"Keep-Alive":          {},
	"Transfer-Encoding":   {},
	"Content-Length":      {},
	"Te":                  {},
	"Trailer":             {},
	"Upgrade":             {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
}

// Entry is one cached response: a pointer into the CAS for the body
// plus the status and headers needed to reconstruct the response.
type Entry struct {
	Digest string      `json:"digest"`
	Size   uint64      `json:"size"`
	Status int         `json:"status"`
	Header http.Header `json:"header"`
}

// Cache is a key -> Entry index backed by a CAS blob store. Lookup
// and Put are safe for concurrent use.
type Cache struct {
	store     *cas.Store
	offline   bool
	indexPath string

	mu    sync.Mutex
	index map[string]Entry
}

// Open returns a Cache rooted at dir, creating it if necessary. The
// blob store lives under dir/blobs and the index under
// dir/index.json; an existing index is loaded so a prior run's
// recordings are visible. offline makes the caller treat misses as
// hard failures (the Cache itself does not fetch — that is the
// caller's job — but it carries the flag so the policy lives in one
// place).
func Open(dir string, offline bool) (*Cache, error) {
	if dir == "" {
		return nil, errors.New("httpcache: empty cache dir")
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("httpcache: mkdir %s: %w", dir, err)
	}
	store, err := cas.Open(filepath.Join(dir, blobsSubdir))
	if err != nil {
		return nil, fmt.Errorf("httpcache: open store: %w", err)
	}
	c := &Cache{
		store:     store,
		offline:   offline,
		indexPath: filepath.Join(dir, indexName),
		index:     make(map[string]Entry),
	}
	if err := c.loadIndex(); err != nil {
		return nil, err
	}
	return c, nil
}

// Offline reports whether the cache was opened in offline mode.
func (c *Cache) Offline() bool { return c.offline }

// loadIndex reads the on-disk index if present. A missing index is
// not an error (fresh cache); a malformed one is, since silently
// starting empty would hide corruption and re-record everything.
func (c *Cache) loadIndex() error {
	data, err := os.ReadFile(c.indexPath) //nolint:gosec // indexPath is cache dir + constant.
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("httpcache: read index: %w", err)
	}
	idx := make(map[string]Entry)
	if err := json.Unmarshal(data, &idx); err != nil {
		return fmt.Errorf("httpcache: parse index %s: %w", c.indexPath, err)
	}
	c.index = idx
	return nil
}

// Lookup returns the Entry recorded for key, if any.
func (c *Cache) Lookup(key string) (Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.index[key]
	return e, ok
}

// Body returns a verified reader over the cached body for digest.
// The caller must Close it. Corruption surfaces as cas.ErrCorrupt at
// end of stream.
func (c *Cache) Body(digest string) (io.ReadCloser, error) {
	return c.store.Get(digest)
}

// Put stores body under key with the given status and headers,
// returning the recorded Entry. The body is fully consumed into the
// CAS (its digest and size are computed there). Hop-by-hop / framing
// headers are dropped so replay reconstructs from Entry.Size. The
// index is persisted before Put returns, so a crashed run still
// replays everything it had fully recorded.
func (c *Cache) Put(key string, status int, header http.Header, body io.Reader) (Entry, error) {
	digest, size, err := c.store.Put(body)
	if err != nil {
		return Entry{}, fmt.Errorf("httpcache: store body: %w", err)
	}
	entry := Entry{
		Digest: digest,
		Size:   size,
		Status: status,
		Header: sanitizeHeader(header),
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.index[key] = entry
	if err := c.persistLocked(); err != nil {
		return Entry{}, err
	}
	return entry, nil
}

// persistLocked writes the index atomically (tmp + rename) so a
// concurrent reader or a crash mid-write never sees a truncated
// file. Caller holds c.mu.
func (c *Cache) persistLocked() error {
	data, err := json.MarshalIndent(c.index, "", "  ")
	if err != nil {
		return fmt.Errorf("httpcache: marshal index: %w", err)
	}
	tmp, err := createTempFn(filepath.Dir(c.indexPath), "index-*.tmp")
	if err != nil {
		return fmt.Errorf("httpcache: create tmp index: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		oops.Skip("httpcache: remove leftover tmp index",
			os.Remove(tmpPath), fs.ErrNotExist)
	}()
	if _, err := tmp.Write(data); err != nil {
		oops.Skip("httpcache: close after write error", tmp.Close(), os.ErrClosed)
		return fmt.Errorf("httpcache: write tmp index: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("httpcache: close tmp index: %w", err)
	}
	if err := renameFn(tmpPath, c.indexPath); err != nil {
		return fmt.Errorf("httpcache: rename index: %w", err)
	}
	return nil
}

// sanitizeHeader returns a deep copy of h with hop-by-hop / framing
// headers removed. Canonicalises keys so the drop-set match is
// case-insensitive regardless of how the upstream cased them.
func sanitizeHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		ck := textproto.CanonicalMIMEHeaderKey(k)
		if _, drop := hopByHopHeaders[ck]; drop {
			continue
		}
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[ck] = cp
	}
	return out
}
