// Package workspace builds and serves content-addressed manifests
// of a directory tree on top of internal/cas.
//
// Slice 1 ships a fixed-size chunker (DefaultChunkSize) plus the
// manifest builder. The fixed-size chunker is correct but suboptimal
// for deduplication — when a file's middle changes by even one
// byte, every following chunk's digest changes too. The PLAN.md
// target is FastCDC (content-defined chunk boundaries), which we'll
// swap in behind the same Chunker interface once the rest of the
// pipeline is exercised.
package workspace

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/richiejp/darkaifactory/internal/cas"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
)

// DefaultChunkSize is 1 MiB — large enough to amortise per-chunk
// proto + filesystem overhead at multi-GiB workspace sizes, small
// enough that lazy-fetching a single chunk over a slow link doesn't
// stall the VM for seconds.
const DefaultChunkSize = 1 << 20

// Chunker splits a stream into ChunkRefs and writes the chunk
// payloads to a CAS. Implementations must be safe to call from a
// single goroutine; they're not expected to be reentrant.
type Chunker interface {
	// Chunk reads from r until EOF, writes each chunk to store,
	// and returns the ordered chunk references plus the total
	// uncompressed byte count. A short read (less than chunkSize)
	// at EOF is the final chunk — no padding.
	Chunk(r io.Reader, store *cas.Store) (refs []*workspacev1.ChunkRef, total uint64, err error)
}

// FixedSizeChunker splits the stream at fixed byte boundaries.
type FixedSizeChunker struct {
	// ChunkSize is the maximum chunk size in bytes. Must be > 0.
	// The last chunk of a file may be smaller. If zero,
	// DefaultChunkSize is used.
	ChunkSize int
}

// Chunk implements Chunker.
func (c *FixedSizeChunker) Chunk(r io.Reader, store *cas.Store) ([]*workspacev1.ChunkRef, uint64, error) {
	if store == nil {
		return nil, 0, errors.New("workspace: nil cas store")
	}
	size := c.ChunkSize
	if size == 0 {
		size = DefaultChunkSize
	}
	if size < 0 {
		return nil, 0, fmt.Errorf("workspace: chunk size %d must be positive", size)
	}

	var (
		refs  []*workspacev1.ChunkRef
		total uint64
		buf   = make([]byte, size)
	)
	for {
		n, err := io.ReadFull(r, buf)
		switch {
		case err == nil:
			ref, perr := putChunk(store, buf[:n])
			if perr != nil {
				return nil, 0, perr
			}
			refs = append(refs, ref)
			total += ref.Size
		case errors.Is(err, io.ErrUnexpectedEOF):
			// Final partial chunk.
			if n > 0 {
				ref, perr := putChunk(store, buf[:n])
				if perr != nil {
					return nil, 0, perr
				}
				refs = append(refs, ref)
				total += ref.Size
			}
			return refs, total, nil
		case errors.Is(err, io.EOF):
			// Stream ended exactly on a chunk boundary, or was
			// empty. Either way: nothing more to do.
			return refs, total, nil
		default:
			return nil, 0, fmt.Errorf("workspace: read: %w", err)
		}
	}
}

// putChunk pushes a single chunk into the CAS and returns its ref.
func putChunk(store *cas.Store, data []byte) (*workspacev1.ChunkRef, error) {
	digest, size, err := store.Put(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("workspace: put chunk: %w", err)
	}
	return &workspacev1.ChunkRef{Digest: digest, Size: size}, nil
}
