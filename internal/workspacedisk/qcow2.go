// Package workspacedisk owns persistent block images used for writable
// sandbox workspaces.
package workspacedisk

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	qcowMagic        = 0x514649fb
	qcowVersion      = 3
	headerLength     = 104
	clusterBits      = 16
	clusterSize      = uint64(1 << clusterBits)
	refcountOrder    = 4 // 16-bit refcounts.
	metadataClusters = 4
	sectorSize       = 512
)

var (
	// ErrInvalidSize reports a virtual size that cannot be represented by the
	// deliberately small, single-refcount-table empty image layout.
	ErrInvalidSize = errors.New("workspacedisk: invalid virtual size")
	// ErrUnalignedSize reports a virtual size that is not sector aligned.
	ErrUnalignedSize = errors.New("workspacedisk: virtual size is not sector aligned")
)

// MaxVirtualSize is the largest image addressable by the one-cluster L1 table
// emitted by CreateEmptyQCOW2 (32 TiB with 64 KiB clusters).
const MaxVirtualSize = clusterSize * (clusterSize / 8) * (clusterSize / 8)

// CreateEmptyQCOW2 writes a deterministic, sparse qcow2 v3 image of
// virtualSize bytes. The image has no backing file and preallocates only its
// four metadata clusters; guest data clusters are allocated by the VMM.
//
// The caller owns creation and atomic publication of the destination file.
// Keeping that policy outside this encoder permits both local files and tests
// using an in-memory WriterAt.
func CreateEmptyQCOW2(dst io.WriterAt, virtualSize uint64) error {
	if dst == nil {
		return errors.New("workspacedisk: nil destination")
	}
	if virtualSize == 0 || virtualSize > MaxVirtualSize {
		return fmt.Errorf("%w: got %d, want 1..%d bytes", ErrInvalidSize, virtualSize, MaxVirtualSize)
	}
	if virtualSize%sectorSize != 0 {
		return fmt.Errorf("%w: got %d bytes", ErrUnalignedSize, virtualSize)
	}

	image := make([]byte, metadataClusters*clusterSize)
	header := image[:headerLength]
	put32(header, 0, qcowMagic)
	put32(header, 4, qcowVersion)
	put32(header, 20, clusterBits)
	put64(header, 24, virtualSize)
	put32(header, 36, l1Entries(virtualSize))
	put64(header, 40, 3*clusterSize) // L1 table.
	put64(header, 48, clusterSize)   // Refcount table.
	put32(header, 56, 1)             // Refcount table clusters.
	put32(header, 96, refcountOrder)
	put32(header, 100, headerLength)

	// The first refcount-table entry points at the first refcount block.
	put64(image, clusterSize, 2*clusterSize)
	// Header, refcount table, refcount block, and L1 table are all live.
	for cluster := uint64(0); cluster < metadataClusters; cluster++ {
		binary.BigEndian.PutUint16(image[2*clusterSize+cluster*2:], 1)
	}

	written, err := dst.WriteAt(image, 0)
	if err != nil {
		return fmt.Errorf("workspacedisk: write qcow2 metadata: %w", err)
	}
	if written != len(image) {
		return fmt.Errorf("workspacedisk: write qcow2 metadata: %w", io.ErrShortWrite)
	}
	return nil
}

func l1Entries(size uint64) uint32 {
	const coverage = clusterSize * (clusterSize / 8)
	// CreateEmptyQCOW2 rejects sizes above MaxVirtualSize, so this quotient is
	// at most clusterSize/8 (8192) and is exactly representable as uint32.
	// #nosec G115 -- the public entry point enforces the bound before calling.
	return uint32((size + coverage - 1) / coverage)
}

func put32(dst []byte, offset uint64, value uint32) {
	binary.BigEndian.PutUint32(dst[offset:], value)
}

func put64(dst []byte, offset, value uint64) {
	binary.BigEndian.PutUint64(dst[offset:], value)
}
