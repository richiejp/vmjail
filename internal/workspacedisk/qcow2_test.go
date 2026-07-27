package workspacedisk

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

type shortWriter struct{}

func (shortWriter) WriteAt(p []byte, _ int64) (int, error) { return len(p) - 1, nil }

const defaultWorkspaceSize = 20 << 30

type writerAtBuffer struct{ data []byte }

func (b *writerAtBuffer) WriteAt(p []byte, off int64) (int, error) {
	end := int(off) + len(p)
	if end > len(b.data) {
		b.data = append(b.data, make([]byte, end-len(b.data))...)
	}
	return copy(b.data[int(off):], p), nil
}

func (b *writerAtBuffer) ReadAt(p []byte, off int64) (int, error) {
	if int(off) >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[int(off):])
	if n != len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (*writerAtBuffer) Sync() error { return nil }

func TestCreateEmptyQCOW2(t *testing.T) {
	var got writerAtBuffer
	if err := CreateEmptyQCOW2(&got, defaultWorkspaceSize); err != nil {
		t.Fatalf("CreateEmptyQCOW2: %v", err)
	}
	if len(got.data) != metadataClusters*int(clusterSize) {
		t.Fatalf("physical metadata size = %d, want %d", len(got.data), metadataClusters*clusterSize)
	}
	want32 := map[int]uint32{0: qcowMagic, 4: qcowVersion, 20: clusterBits, 36: 40, 56: 1, 96: refcountOrder, 100: headerLength}
	for off, want := range want32 {
		if value := binary.BigEndian.Uint32(got.data[off:]); value != want {
			t.Errorf("header uint32 at %d = %d, want %d", off, value, want)
		}
	}
	want64 := map[int]uint64{24: defaultWorkspaceSize, 40: 3 * clusterSize, 48: clusterSize}
	for off, want := range want64 {
		if value := binary.BigEndian.Uint64(got.data[off:]); value != want {
			t.Errorf("header uint64 at %d = %d, want %d", off, value, want)
		}
	}
	if value := binary.BigEndian.Uint64(got.data[clusterSize:]); value != 2*clusterSize {
		t.Errorf("refcount table entry = %d, want %d", value, 2*clusterSize)
	}
	for cluster := uint64(0); cluster < metadataClusters; cluster++ {
		if value := binary.BigEndian.Uint16(got.data[2*clusterSize+cluster*2:]); value != 1 {
			t.Errorf("refcount %d = %d, want 1", cluster, value)
		}
	}
	if tail := got.data[2*clusterSize+metadataClusters*2:]; !bytes.Equal(tail, make([]byte, len(tail))) {
		t.Fatal("unused refcounts or L1 entries are not zero")
	}
}

func TestCreateEmptyQCOW2Deterministic(t *testing.T) {
	var first, second writerAtBuffer
	if err := CreateEmptyQCOW2(&first, defaultWorkspaceSize); err != nil {
		t.Fatal(err)
	}
	if err := CreateEmptyQCOW2(&second, defaultWorkspaceSize); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.data, second.data) {
		t.Fatal("identical virtual sizes produced different images")
	}
}

func TestCreateEmptyQCOW2RejectsInvalidSize(t *testing.T) {
	tests := []struct {
		name string
		size uint64
		want error
	}{
		{name: "zero", want: ErrInvalidSize},
		{name: "unaligned", size: 513, want: ErrUnalignedSize},
		{name: "too large", size: MaxVirtualSize + sectorSize, want: ErrInvalidSize},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dst writerAtBuffer
			err := CreateEmptyQCOW2(&dst, tc.size)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if len(dst.data) != 0 {
				t.Fatal("invalid request wrote output")
			}
		})
	}
}

func TestCreateEmptyQCOW2RejectsNilDestination(t *testing.T) {
	if err := CreateEmptyQCOW2(nil, defaultWorkspaceSize); err == nil {
		t.Fatal("nil destination accepted")
	}
}

func TestCreateEmptyQCOW2RejectsShortWrite(t *testing.T) {
	if err := CreateEmptyQCOW2(shortWriter{}, defaultWorkspaceSize); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("error = %v, want %v", err, io.ErrShortWrite)
	}
}

func TestGrowQCOW2(t *testing.T) {
	var image writerAtBuffer
	if err := CreateEmptyQCOW2(&image, defaultWorkspaceSize); err != nil {
		t.Fatal(err)
	}
	const grown = 24 << 30
	if err := GrowQCOW2(&image, grown); err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint64(image.data[24:]); got != grown {
		t.Errorf("virtual size = %d, want %d", got, uint64(grown))
	}
	if got := binary.BigEndian.Uint32(image.data[36:]); got != l1Entries(grown) {
		t.Errorf("L1 entries = %d, want %d", got, l1Entries(grown))
	}
	if err := GrowQCOW2(&image, grown); err != nil {
		t.Fatalf("idempotent grow: %v", err)
	}
	if err := GrowQCOW2(&image, defaultWorkspaceSize); !errors.Is(err, ErrShrink) {
		t.Fatalf("shrink error = %v, want %v", err, ErrShrink)
	}
}

func TestGrowQCOW2RejectsInvalidInputs(t *testing.T) {
	if err := GrowQCOW2(nil, defaultWorkspaceSize); err == nil {
		t.Fatal("nil destination accepted")
	}
	for _, size := range []uint64{0, 513, MaxVirtualSize + sectorSize} {
		var image writerAtBuffer
		if err := GrowQCOW2(&image, size); err == nil {
			t.Errorf("invalid size %d accepted", size)
		}
	}
	var corrupt writerAtBuffer
	corrupt.data = make([]byte, headerLength)
	if err := GrowQCOW2(&corrupt, defaultWorkspaceSize); err == nil {
		t.Fatal("corrupt image accepted")
	}
}
