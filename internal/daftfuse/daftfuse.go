// Package daftfuse implements the lazy chunk-fetch FUSE filesystem
// that sits underneath virtiofsd on the sandbox-host. Workspace
// reads from inside the VM cross virtiofs → daft-fuse; misses in
// the local CAS trigger a chunk fetch over the user-host
// transport (Slice 3.5 broker or direct dial).
//
// Slice 4a (this commit) is the transport-agnostic library: a
// Manifest plus a CAS plus a ChunkFetcher gives you a Mount with
// a per-path ReadAt that the FUSE adapter delegates to. The FUSE
// node implementations live in fuse_linux.go behind a build tag
// so non-linux unit tests still compile.
//
// Threading model: ReadAt is goroutine-safe. Concurrent reads of
// the same digest are coalesced through singleflight so the
// transport sees one in-flight request per chunk regardless of
// reader count.
package daftfuse

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/richiejp/darkaifactory/internal/cas"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
)

// ChunkFetcher fetches a single chunk's compressed bytes from a
// remote workspace server. Implementations are typically thin
// wrappers around workspacev1connect.WorkspaceServiceClient; tests
// provide an in-memory map. Returned bytes are zstd-compressed; the
// caller (Mount) hands them to cas.Store.PutCompressed, which
// verifies the digest before committing.
type ChunkFetcher interface {
	FetchChunk(ctx context.Context, digest string) (compressed []byte, err error)
}

// Decision is a Policy verdict on a read.
type Decision int

const (
	// Allow lets the read proceed normally.
	Allow Decision = iota
	// Deny surfaces EACCES to the VM. The policy engine has
	// decided the workload is not permitted to see this chunk.
	Deny
	// Freeze surfaces EAGAIN and signals the caller's freeze
	// handler. Slice 4a only ships the EAGAIN half — the QMP
	// stop wiring is a separate slice.
	Freeze
)

// ReadEvent is the input to Policy.OnRead. Path is manifest-
// relative and forward-slash separated. Digest is the chunk that
// would be served by this read; multi-chunk reads issue one
// ReadEvent per chunk.
type ReadEvent struct {
	Path   string
	Offset uint64
	Size   uint64
	Digest string
}

// Policy is consulted before each chunk-level read. Implementations
// must be safe for concurrent use.
type Policy interface {
	OnRead(*ReadEvent) Decision
}

// AllowAll is a Policy that approves every read. It is the default
// when Options.Policy is nil.
type AllowAll struct{}

// OnRead always returns Allow.
func (AllowAll) OnRead(*ReadEvent) Decision { return Allow }

// Options configures a Mount.
type Options struct {
	// Policy is consulted on every chunk-level read. nil → AllowAll.
	Policy Policy
	// FetchRetries is the maximum number of times a chunk fetch is
	// retried on transient error. 0 → defaultFetchRetries.
	FetchRetries int
	// FetchBackoff is the initial backoff between retries; each
	// retry doubles it. 0 → defaultFetchBackoff.
	FetchBackoff time.Duration
}

const (
	defaultFetchRetries = 3
	defaultFetchBackoff = 50 * time.Millisecond
)

// Mount is the in-memory representation of a workspace manifest
// plus its read/fetch machinery. Build one with New, hand it to
// Serve (FUSE adapter, fuse_linux.go) or drive it directly via
// ReadAt / Lookup.
type Mount struct {
	store   *cas.Store
	fetcher ChunkFetcher
	policy  Policy
	retries int
	backoff time.Duration

	tree *tree
	sf   singleflight.Group
}

// New builds a Mount from a manifest. The store backs cache hits;
// fetcher provides misses. nil fetcher means "no remote" — every
// miss is a permanent error, useful for tests that prime the store
// up-front.
func New(m *workspacev1.Manifest, store *cas.Store, fetcher ChunkFetcher, opts Options) (*Mount, error) {
	if m == nil {
		return nil, errors.New("daftfuse: nil manifest")
	}
	if store == nil {
		return nil, errors.New("daftfuse: nil cas store")
	}
	t, err := buildTree(m)
	if err != nil {
		return nil, fmt.Errorf("daftfuse: build tree: %w", err)
	}
	policy := opts.Policy
	if policy == nil {
		policy = AllowAll{}
	}
	retries := opts.FetchRetries
	if retries <= 0 {
		retries = defaultFetchRetries
	}
	backoff := opts.FetchBackoff
	if backoff <= 0 {
		backoff = defaultFetchBackoff
	}
	return &Mount{
		store:   store,
		fetcher: fetcher,
		policy:  policy,
		retries: retries,
		backoff: backoff,
		tree:    t,
	}, nil
}

// ErrDenied is returned by ReadAt when Policy.OnRead returns Deny.
var ErrDenied = errors.New("daftfuse: read denied by policy")

// ErrFrozen is returned by ReadAt when Policy.OnRead returns Freeze.
// FUSE adapters map this to EAGAIN.
var ErrFrozen = errors.New("daftfuse: read frozen by policy")

// ErrNotRegular is returned by ReadAt when the path resolves to a
// directory or symlink rather than a regular file.
var ErrNotRegular = errors.New("daftfuse: not a regular file")
