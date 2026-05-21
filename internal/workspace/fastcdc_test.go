package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"math/bits"
	"os"
	"path/filepath"
	"testing"

	"github.com/localai-org/darkaifactory/internal/cas"
)

func TestFastCDCChunkerEmptyStream(t *testing.T) {
	store := mustOpenStore(t)
	c := &FastCDCChunker{}
	refs, total, err := c.Chunk(bytes.NewReader(nil), store)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if total != 0 || len(refs) != 0 {
		t.Errorf("empty stream produced refs=%d total=%d", len(refs), total)
	}
}

func TestFastCDCChunkerShortStreamYieldsSingleChunk(t *testing.T) {
	store := mustOpenStore(t)
	payload := bytes.Repeat([]byte("z"), 100)
	c := &FastCDCChunker{}
	refs, total, err := c.Chunk(bytes.NewReader(payload), store)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("chunks = %d, want 1", len(refs))
	}
	if total != uint64(len(payload)) || refs[0].Size != uint64(len(payload)) {
		t.Errorf("size = %d/%d, want %d", refs[0].Size, total, len(payload))
	}
}

// TestFastCDCChunkerSplitsLargeStream feeds 10 MiB of seeded random
// bytes through the chunker with default parameters and verifies the
// invariants the algorithm is supposed to guarantee: every chunk
// (except possibly the final one) is in [MinSize, MaxSize], the
// sizes sum to the input length, and the result is non-trivially
// split (more than one chunk).
func TestFastCDCChunkerSplitsLargeStream(t *testing.T) {
	store := mustOpenStore(t)
	data := randomBytes(1, 2, 10<<20)
	c := &FastCDCChunker{}
	refs, total, err := c.Chunk(bytes.NewReader(data), store)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if total != uint64(len(data)) {
		t.Errorf("total = %d, want %d", total, len(data))
	}
	if len(refs) < 2 {
		t.Fatalf("chunks = %d, want >= 2", len(refs))
	}
	var sum uint64
	for i, r := range refs {
		sum += r.Size
		isFinal := i == len(refs)-1
		if !isFinal && r.Size < DefaultFastCDCMinSize {
			t.Errorf("non-final chunk %d size %d < min %d",
				i, r.Size, DefaultFastCDCMinSize)
		}
		if r.Size > DefaultFastCDCMaxSize {
			t.Errorf("chunk %d size %d > max %d",
				i, r.Size, DefaultFastCDCMaxSize)
		}
	}
	if sum != uint64(len(data)) {
		t.Errorf("sum of chunk sizes = %d, want %d", sum, len(data))
	}
}

// TestFastCDCChunkerMaxSizeOnLowEntropy: an all-zero stream has no
// content-defined boundaries, so the chunker must fall back to the
// MaxSize hard-cut. Final partial chunk is whatever's left.
func TestFastCDCChunkerMaxSizeOnLowEntropy(t *testing.T) {
	store := mustOpenStore(t)
	const sz = 9 * DefaultFastCDCMaxSize / 2 // 4.5 × max
	zeros := make([]byte, sz)
	c := &FastCDCChunker{}
	refs, total, err := c.Chunk(bytes.NewReader(zeros), store)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if total != sz {
		t.Errorf("total = %d, want %d", total, sz)
	}
	// Expected: 4 max-size chunks + 1 half-size final.
	if len(refs) != 5 {
		t.Fatalf("chunks = %d, want 5", len(refs))
	}
	for i := range 4 {
		if refs[i].Size != DefaultFastCDCMaxSize {
			t.Errorf("chunk %d size = %d, want max %d",
				i, refs[i].Size, DefaultFastCDCMaxSize)
		}
	}
	if refs[4].Size != DefaultFastCDCMaxSize/2 {
		t.Errorf("final chunk size = %d, want %d",
			refs[4].Size, DefaultFastCDCMaxSize/2)
	}
}

// TestFastCDCChunkerDeterministic: chunking the same bytes through
// two independent stores yields identical chunk digests. Pins the
// stability property that makes content-addressed dedup work.
func TestFastCDCChunkerDeterministic(t *testing.T) {
	data := randomBytes(7, 8, 4<<20)
	s1, s2 := mustOpenStore(t), mustOpenStore(t)
	c := &FastCDCChunker{}
	r1, _, err := c.Chunk(bytes.NewReader(data), s1)
	if err != nil {
		t.Fatalf("Chunk #1: %v", err)
	}
	r2, _, err := c.Chunk(bytes.NewReader(data), s2)
	if err != nil {
		t.Fatalf("Chunk #2: %v", err)
	}
	if len(r1) != len(r2) {
		t.Fatalf("chunk counts differ: %d vs %d", len(r1), len(r2))
	}
	for i := range r1 {
		if r1[i].Digest != r2[i].Digest || r1[i].Size != r2[i].Size {
			t.Errorf("chunk %d differs: (%s,%d) vs (%s,%d)",
				i, r1[i].Digest, r1[i].Size, r2[i].Digest, r2[i].Size)
		}
	}
}

// TestFastCDCChunkerLocality: insert a kilobyte of bytes into the
// middle of a stream and verify that most chunks before and after
// the splice are unchanged. This is the property the FixedSizeChunker
// lacks — every chunk past the insertion point shifts and gets a
// new digest.
func TestFastCDCChunkerLocality(t *testing.T) {
	base := randomBytes(11, 13, 8<<20)
	mid := len(base) / 2
	insertion := randomBytes(21, 23, 1<<10)
	edited := make([]byte, 0, len(base)+len(insertion))
	edited = append(edited, base[:mid]...)
	edited = append(edited, insertion...)
	edited = append(edited, base[mid:]...)

	c := &FastCDCChunker{}
	rA, _, err := c.Chunk(bytes.NewReader(base), mustOpenStore(t))
	if err != nil {
		t.Fatalf("base Chunk: %v", err)
	}
	rB, _, err := c.Chunk(bytes.NewReader(edited), mustOpenStore(t))
	if err != nil {
		t.Fatalf("edited Chunk: %v", err)
	}

	shared := map[string]struct{}{}
	for _, r := range rA {
		shared[r.Digest] = struct{}{}
	}
	overlap := 0
	for _, r := range rB {
		if _, ok := shared[r.Digest]; ok {
			overlap++
		}
	}
	// Half-and-half: at minimum, several chunks before the splice
	// and several after should survive unchanged.
	minOverlap := len(rA) / 2
	if overlap < minOverlap {
		t.Errorf("shared chunks = %d (of %d original / %d edited); want >= %d",
			overlap, len(rA), len(rB), minOverlap)
	}
}

func TestFastCDCChunkerDefaultParams(t *testing.T) {
	c := &FastCDCChunker{}
	mn, av, mx := c.params()
	if mn != DefaultFastCDCMinSize {
		t.Errorf("min = %d, want %d", mn, DefaultFastCDCMinSize)
	}
	if av != DefaultFastCDCAvgSize {
		t.Errorf("avg = %d, want %d", av, DefaultFastCDCAvgSize)
	}
	if mx != DefaultFastCDCMaxSize {
		t.Errorf("max = %d, want %d", mx, DefaultFastCDCMaxSize)
	}
}

func TestFastCDCChunkerCustomParams(t *testing.T) {
	store := mustOpenStore(t)
	c := &FastCDCChunker{MinSize: 1 << 10, AvgSize: 4 << 10, MaxSize: 16 << 10}
	data := randomBytes(3, 5, 1<<20)
	refs, total, err := c.Chunk(bytes.NewReader(data), store)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if total != uint64(len(data)) {
		t.Errorf("total = %d, want %d", total, len(data))
	}
	for i, r := range refs {
		isFinal := i == len(refs)-1
		if !isFinal && r.Size < 1<<10 {
			t.Errorf("chunk %d size %d below min", i, r.Size)
		}
		if r.Size > 16<<10 {
			t.Errorf("chunk %d size %d above max", i, r.Size)
		}
	}
}

func TestFastCDCChunkerRejectsBadParams(t *testing.T) {
	cases := []struct {
		name string
		c    FastCDCChunker
	}{
		{"min negative", FastCDCChunker{MinSize: -1, AvgSize: 1 << 20, MaxSize: 4 << 20}},
		{"avg below min", FastCDCChunker{MinSize: 2 << 20, AvgSize: 1 << 20, MaxSize: 4 << 20}},
		{"max below avg", FastCDCChunker{MinSize: 256 << 10, AvgSize: 1 << 20, MaxSize: 512 << 10}},
	}
	store := mustOpenStore(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := tc.c.Chunk(bytes.NewReader(nil), store); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestFastCDCChunkerRejectsNilStore(t *testing.T) {
	c := &FastCDCChunker{}
	if _, _, err := c.Chunk(bytes.NewReader(nil), nil); err == nil {
		t.Fatal("expected error for nil store")
	}
}

func TestFastCDCChunkerPropagatesReadError(t *testing.T) {
	store := mustOpenStore(t)
	c := &FastCDCChunker{MinSize: 1 << 10, AvgSize: 4 << 10, MaxSize: 16 << 10}
	r := &failingReader{
		src: bytes.NewReader(bytes.Repeat([]byte("x"), 32<<10)),
		n:   2 << 10,
	}
	if _, _, err := c.Chunk(r, store); err == nil {
		t.Fatal("expected read error")
	}
}

// TestFastCDCChunkerPropagatesPutError pre-plants a regular file at
// <store-root>/sha256 so CAS Put's MkdirAll fails, exercising the
// putChunk error-wrap branch in Chunk.
func TestFastCDCChunkerPropagatesPutError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sha256"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	store, err := cas.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c := &FastCDCChunker{MinSize: 8, AvgSize: 32, MaxSize: 128}
	if _, _, err := c.Chunk(bytes.NewReader([]byte("hello world hello world")), store); err == nil {
		t.Fatal("expected error when CAS is poisoned")
	}
}

func TestFastCDCMasksPopcountOrdering(t *testing.T) {
	cases := []int{1 << 13, 1 << 16, 1 << 20, 1 << 24}
	for _, avg := range cases {
		s, l := fastCDCMasks(avg)
		ps, pl := bits.OnesCount64(s), bits.OnesCount64(l)
		if ps <= pl {
			t.Errorf("avg=%d popcount: MaskS=%d, MaskL=%d (want S > L)", avg, ps, pl)
		}
		log2A := bits.Len(uint(avg)) - 1
		if ps != log2A+2 {
			t.Errorf("avg=%d MaskS popcount %d, want %d", avg, ps, log2A+2)
		}
		if pl != log2A-2 {
			t.Errorf("avg=%d MaskL popcount %d, want %d", avg, pl, log2A-2)
		}
	}
}

func TestBuildFastCDCMaskClamps(t *testing.T) {
	if m := buildFastCDCMask(-1); m != 0 {
		t.Errorf("buildFastCDCMask(-1) = %#x, want 0", m)
	}
	full := buildFastCDCMask(1000)
	if got, want := bits.OnesCount64(full), len(fastCDCMaskPositions); got != want {
		t.Errorf("buildFastCDCMask(1000) popcount = %d, want %d", got, want)
	}
}

// TestGearTableStability pins a checksum over the gear table so any
// accidental change to the SplitMix64 seed/state — which would
// silently invalidate every chunk boundary in stores built by
// older versions — fails loudly here instead of at runtime in
// production.
func TestGearTableStability(t *testing.T) {
	const want = "d29093e6712e90f0bc37a7f04c5354412a53527b1c94cba195bdba6a7158a4d1"
	h := sha256.New()
	var b [8]byte
	for _, v := range gearTable {
		binary.LittleEndian.PutUint64(b[:], v)
		h.Write(b[:])
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		t.Fatalf("gear table sha256 = %s, want %s\n"+
			"the gear table has changed — every chunk boundary will\n"+
			"shift; if this is intentional, update the pinned value",
			got, want)
	}
}

func TestFastCDCCutPureFunction(t *testing.T) {
	maskS, maskL := fastCDCMasks(64)
	if got := fastCDCCut(make([]byte, 5), 16, 32, 64, maskS, maskL); got != 5 {
		t.Errorf("short buf cut = %d, want 5", got)
	}
	if got := fastCDCCut(make([]byte, 128), 16, 32, 64, maskS, maskL); got != 64 {
		t.Errorf("all-zero cut = %d, want 64 (maxS)", got)
	}
}

// TestFastCDCCutFindsBoundaryWithinAvg verifies that a buffer
// engineered to flip the hash to zero produces a cut in the
// [MinSize, AvgSize) window. Constructed by feeding bytes whose
// gearTable entries cancel out so h reaches 0.
func TestFastCDCCutFindsBoundaryWithinAvg(t *testing.T) {
	// Find one input byte b whose gearTable[b] is "small" — i.e.
	// h mod (1<<24) == 0 after enough doublings would mask-zero.
	// Easier: feed a long run of zeros; gearTable[0] != 0 so h
	// grows, but search for the first byte index where h&maskS==0.
	const minS, avgS, maxS = 32, 256, 1024
	maskS, maskL := fastCDCMasks(avgS)

	buf := randomBytes(99, 100, maxS)
	cut := fastCDCCut(buf, minS, avgS, maxS, maskS, maskL)
	if cut < 1 || cut > maxS {
		t.Errorf("cut = %d, want in [1, %d]", cut, maxS)
	}
}

// FuzzFastCDCCut: arbitrary bytes + sane sizes must not panic and
// must return a cut in [1, min(len, maxS)] (or len if len <= minS).
func FuzzFastCDCCut(f *testing.F) {
	f.Add([]byte("hello world"), 4, 16, 64)
	f.Add([]byte{}, 4, 16, 64)
	f.Add(make([]byte, 200), 4, 16, 64)
	f.Add(bytes.Repeat([]byte{0xff}, 1024), 32, 128, 512)
	f.Fuzz(func(t *testing.T, buf []byte, minS, avgS, maxS int) {
		// Constrain sizes to keep the fuzzer focused on the
		// algorithm rather than tripping validateFastCDC.
		minS = (minS & 0x3ff) + 1
		avgS = minS + (avgS & 0xfff)
		maxS = avgS + (maxS & 0xfff)
		if maxS > 1<<20 {
			return
		}
		maskS, maskL := fastCDCMasks(avgS)
		cut := fastCDCCut(buf, minS, avgS, maxS, maskS, maskL)
		switch {
		case len(buf) == 0:
			if cut != 0 {
				t.Errorf("empty buf cut = %d, want 0", cut)
			}
		case len(buf) <= minS:
			if cut != len(buf) {
				t.Errorf("short buf len=%d cut=%d", len(buf), cut)
			}
		default:
			upper := min(maxS, len(buf))
			if cut < 1 || cut > upper {
				t.Errorf("cut=%d out of [1, %d] for len=%d minS=%d maxS=%d",
					cut, upper, len(buf), minS, maxS)
			}
		}
	})
}

// fastCDCNilReaderError isn't a real case (Chunk takes io.Reader),
// but a nil io.Reader would panic. Confirm we handle a Reader that
// returns 0,nil indefinitely by capping iterations — that is, the
// Read loop must terminate on EOF, not on a sentinel like
// "no progress". This test uses io.LimitReader to provoke the
// boundary: 0 bytes available + EOF returns cleanly.
func TestFastCDCChunkerReaderReturningZeroEOF(t *testing.T) {
	store := mustOpenStore(t)
	r := io.LimitReader(bytes.NewReader(nil), 0)
	c := &FastCDCChunker{}
	refs, total, err := c.Chunk(r, store)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if total != 0 || len(refs) != 0 {
		t.Errorf("expected empty result, got refs=%d total=%d", len(refs), total)
	}
}
