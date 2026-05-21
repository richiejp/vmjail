package workspace

import (
	"errors"
	"fmt"
	"io"
	"math/bits"

	"github.com/localai-org/darkaifactory/internal/cas"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
)

// DefaultFastCDCMinSize is the smallest non-final chunk FastCDC
// will emit. Below this, per-chunk overhead (sha256 digest, zstd
// dict reset, proto entry) starts to dominate and the dedup win
// is too small to justify it.
const DefaultFastCDCMinSize = 256 << 10

// DefaultFastCDCAvgSize is the target average chunk size. The
// boundary-detection mask popcount is log2(AvgSize) ± 2 for
// normalized chunking (NC=2), so the realised distribution is
// tighter around the average than vanilla content-defined chunking.
const DefaultFastCDCAvgSize = 1 << 20

// DefaultFastCDCMaxSize caps a single chunk. When the rolling hash
// hasn't matched by MaxSize the chunker emits a hard cut, so a
// long run of low-entropy bytes can't produce a multi-GiB chunk
// that would pin RAM and defeat lazy fetch.
const DefaultFastCDCMaxSize = 4 << 20

// FastCDCChunker implements content-defined chunking via FastCDC
// (Xia et al., USENIX ATC '16). Boundaries are determined by a
// rolling 64-bit gear hash. Inserting bytes in the middle of a
// stream only affects chunks within one MaxSize window of the
// insertion point, so dedup survives edits — the property the
// FixedSizeChunker lacks.
//
// Min/Avg/Max default to DefaultFastCDC* if zero.
type FastCDCChunker struct {
	// MinSize is the smallest non-final chunk in bytes. The final
	// chunk of a stream may be smaller.
	MinSize int
	// AvgSize is the target average chunk size in bytes. Drives
	// the boundary-mask popcount.
	AvgSize int
	// MaxSize is the hard cap. A chunk reaching MaxSize is cut
	// even if no content-defined boundary has appeared.
	MaxSize int
}

// Chunk implements Chunker.
func (c *FastCDCChunker) Chunk(r io.Reader, store *cas.Store) ([]*workspacev1.ChunkRef, uint64, error) {
	if store == nil {
		return nil, 0, errors.New("workspace: nil cas store")
	}
	minS, avgS, maxS := c.params()
	if err := validateFastCDC(minS, avgS, maxS); err != nil {
		return nil, 0, err
	}
	maskS, maskL := fastCDCMasks(avgS)

	var (
		refs  []*workspacev1.ChunkRef
		total uint64
		// buf is a window over the unconsumed stream. head is the
		// first byte of the next prospective chunk; tail is buf's
		// length. Reads land in buf[tail:cap(buf)] directly so
		// there's no per-Read copy; cuts advance head rather than
		// memcpy'ing the tail to the front, so a stream that emits
		// N chunks pays at most one compaction memcpy per N (and
		// often zero if the producer hits EOF before the next
		// fill).
		buf  = make([]byte, maxS)
		head = 0
		tail = 0
		eof  bool
	)
	//daft:lint:disable=forloopbound reason="exits when the input is fully drained (head == tail after EOF) or any non-EOF read error fires"
	for {
		for !eof && tail-head < maxS {
			if tail == cap(buf) {
				// Out of room at the end. Compact: shift the live
				// window back to buf[0:]. Single memcpy of at most
				// maxS-1 bytes per compaction; amortises away on
				// long streams.
				copy(buf, buf[head:tail])
				tail -= head
				head = 0
			}
			n, err := r.Read(buf[tail:cap(buf)])
			tail += n
			if errors.Is(err, io.EOF) {
				eof = true
				break
			}
			if err != nil {
				return nil, 0, fmt.Errorf("workspace: read: %w", err)
			}
		}
		if head == tail {
			return refs, total, nil
		}
		cutAt := fastCDCCut(buf[head:tail], minS, avgS, maxS, maskS, maskL)
		ref, err := putChunk(store, buf[head:head+cutAt])
		if err != nil {
			return nil, 0, err
		}
		refs = append(refs, ref)
		total += ref.Size
		head += cutAt
	}
}

func (c *FastCDCChunker) params() (minS, avgS, maxS int) {
	minS, avgS, maxS = c.MinSize, c.AvgSize, c.MaxSize
	if minS == 0 {
		minS = DefaultFastCDCMinSize
	}
	if avgS == 0 {
		avgS = DefaultFastCDCAvgSize
	}
	if maxS == 0 {
		maxS = DefaultFastCDCMaxSize
	}
	return minS, avgS, maxS
}

func validateFastCDC(minS, avgS, maxS int) error {
	if minS <= 0 {
		return fmt.Errorf("workspace: fastcdc min size %d must be > 0", minS)
	}
	if avgS < minS {
		return fmt.Errorf("workspace: fastcdc avg size %d < min %d", avgS, minS)
	}
	if maxS < avgS {
		return fmt.Errorf("workspace: fastcdc max size %d < avg %d", maxS, avgS)
	}
	return nil
}

// fastCDCCut finds the next cut point in buf and returns its byte
// length. Streams shorter than minS pass through as a single
// (final) chunk. When buf reaches maxS without a content-defined
// boundary, the cut is forced at maxS.
func fastCDCCut(buf []byte, minS, avgS, maxS int, maskS, maskL uint64) int {
	n := len(buf)
	if n <= minS {
		return n
	}
	if maxS > n {
		maxS = n
	}
	if avgS > maxS {
		avgS = maxS
	}
	var h uint64
	i := 0
	// Pre-roll up to MinSize without checking — content before
	// MinSize can't produce a cut, but it still contributes
	// entropy to h so the first checked byte already sees a
	// well-mixed hash.
	for ; i < minS; i++ {
		h = (h << 1) + gearTable[buf[i]]
	}
	// [Min, Avg): harder mask — more bits set, lower probability
	// of cut, biases chunks toward AvgSize.
	for ; i < avgS; i++ {
		h = (h << 1) + gearTable[buf[i]]
		if h&maskS == 0 {
			return i + 1
		}
	}
	// [Avg, Max): softer mask — fewer bits set, higher
	// probability of cut, so the chunk is far more likely to end
	// before MaxSize than the harder mask would allow.
	for ; i < maxS; i++ {
		h = (h << 1) + gearTable[buf[i]]
		if h&maskL == 0 {
			return i + 1
		}
	}
	return maxS
}

// fastCDCMasks returns (MaskS, MaskL) for the given average size.
// Popcount targets log2(avg) ± 2 (normalized chunking, NC=2).
func fastCDCMasks(avgSize int) (maskS, maskL uint64) {
	log2A := bits.Len(uint(avgSize)) - 1
	return buildFastCDCMask(log2A + 2), buildFastCDCMask(log2A - 2)
}

// fastCDCMaskPositions is a fixed permutation of bit positions
// 8..63 chosen to spread set bits across the upper portion of a
// 64-bit word. Bits 0..7 are excluded because they carry the
// lowest entropy in a left-shift rolling hash (a single byte's
// contribution only reaches the low bits on the iteration it was
// added). The exact order is arbitrary but stable — changing it
// would invalidate every chunk boundary in stores built by older
// versions.
//
//nolint:gochecknoglobals // stable lookup table; mutating it would invalidate every existing chunk store.
var fastCDCMaskPositions = [56]uint{
	15, 31, 23, 47, 39, 55, 19, 35, 27, 43, 11, 51, 59, 17, 33, 25,
	41, 9, 49, 57, 13, 21, 29, 37, 45, 53, 61, 63, 14, 22, 30, 38,
	46, 54, 10, 18, 26, 34, 42, 50, 58, 16, 24, 32, 40, 48, 56, 62,
	12, 20, 28, 36, 44, 52, 8, 60,
}

func buildFastCDCMask(popcount int) uint64 {
	if popcount < 0 {
		popcount = 0
	}
	if popcount > len(fastCDCMaskPositions) {
		popcount = len(fastCDCMaskPositions)
	}
	var m uint64
	for i := 0; i < popcount; i++ {
		m |= uint64(1) << fastCDCMaskPositions[i]
	}
	return m
}

// gearTable is the 256-entry FastCDC gear hash table. Each entry
// is a random uint64; the only property we need is "well
// distributed". Generated at init from SplitMix64 with a fixed
// seed so the values are stable across builds — changing the
// table would change every chunk boundary, invalidating dedup
// against existing stores.
//
//nolint:gochecknoglobals // stable lookup table populated in init from a fixed seed; mutating it would invalidate every existing chunk store.
var gearTable [256]uint64

func init() {
	// SplitMix64 (Vigna 2015). Trivial to reimplement without a
	// third-party dependency. The seed is the golden-ratio
	// constant; only its stability matters, not its value.
	var state uint64 = 0x9E3779B97F4A7C15
	for i := range gearTable {
		state += 0x9E3779B97F4A7C15
		z := state
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		gearTable[i] = z
	}
}
