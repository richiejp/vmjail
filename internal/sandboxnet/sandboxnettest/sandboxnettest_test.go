package sandboxnettest

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

const testGatewayMACStr = "5a:94:ef:e4:0c:dd"

func TestARPRequestShape(t *testing.T) {
	src := []byte{0x02, 0, 0, 0, 0, 1}
	frame := ARPRequest(src, []byte{10, 0, 0, 2}, []byte{10, 0, 0, 1})
	if len(frame) != 42 {
		t.Fatalf("ARP frame length = %d, want 42", len(frame))
	}
	for i := range 6 {
		if frame[i] != 0xff {
			t.Fatalf("frame[%d] = %x, want 0xff (broadcast)", i, frame[i])
		}
	}
	if frame[12] != 0x08 || frame[13] != 0x06 {
		t.Fatalf("ethertype = %x %x, want 08 06", frame[12], frame[13])
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	payload := []byte("hello-frame-payload")
	go func() {
		// Writer goroutine sidesteps the testinggoroutine analyzer:
		// a t.Fatalf out of WriteFrame from a non-test goroutine
		// would not propagate. ReadFrame timing out is the test
		// surface anyway.
		hdr := make([]byte, 2)
		binary.LittleEndian.PutUint16(hdr, uint16(len(payload))) //nolint:gosec // bounded by test payload.
		_, _ = a.Write(append(hdr, payload...))
	}()
	got := ReadFrame(t, b, time.Second)
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

// fakeT lets us assert AssertARPReply / ReadFrame failure paths
// without taking down the parent test. Fatalf panics with a
// sentinel; the test recovers and inspects `failed`.
type fakeT struct {
	testing.TB
	failed bool
}

type fakeFatalSignal struct{}

func (f *fakeT) Helper() {}
func (f *fakeT) Fatalf(string, ...any) {
	f.failed = true
	panic(fakeFatalSignal{})
}

func runFakeT(fn func(*fakeT)) (failed bool) {
	ft := &fakeT{}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(fakeFatalSignal); !ok {
				panic(r)
			}
		}
		failed = ft.failed
	}()
	fn(ft)
	return
}

func TestAssertARPReplyAcceptsValidReply(t *testing.T) {
	gatewayMAC := []byte{0x5a, 0x94, 0xef, 0xe4, 0x0c, 0xdd}
	guestMAC := []byte{0x02, 0, 0, 0, 0, 1}
	frame := make([]byte, 42)
	copy(frame[0:6], guestMAC)
	copy(frame[6:12], gatewayMAC)
	frame[12], frame[13] = 0x08, 0x06
	frame[20], frame[21] = 0x00, 0x02
	if runFakeT(func(ft *fakeT) {
		AssertARPReply(ft, frame, testGatewayMACStr, guestMAC)
	}) {
		t.Fatal("AssertARPReply rejected a valid reply")
	}
}

func TestAssertARPReplyRejects(t *testing.T) {
	gatewayMAC := []byte{0x5a, 0x94, 0xef, 0xe4, 0x0c, 0xdd}
	guestMAC := []byte{0x02, 0, 0, 0, 0, 1}
	validFrame := func() []byte {
		f := make([]byte, 42)
		copy(f[0:6], guestMAC)
		copy(f[6:12], gatewayMAC)
		f[12], f[13] = 0x08, 0x06
		f[20], f[21] = 0x00, 0x02
		return f
	}
	cases := []struct {
		name  string
		frame []byte
		dst   []byte
		mac   string
	}{
		{"too short", make([]byte, 20), guestMAC, testGatewayMACStr},
		{"wrong dst MAC", validFrame(), []byte{0, 0, 0, 0, 0, 1}, testGatewayMACStr},
		{"unparseable want src MAC", validFrame(), guestMAC, "not-a-mac"},
		{"wrong src MAC", validFrame(), guestMAC, "00:11:22:33:44:55"},
		{"wrong ethertype", func() []byte { f := validFrame(); f[12], f[13] = 0x08, 0x00; return f }(), guestMAC, testGatewayMACStr},
		{"wrong op", func() []byte { f := validFrame(); f[20], f[21] = 0x00, 0x01; return f }(), guestMAC, testGatewayMACStr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !runFakeT(func(ft *fakeT) {
				AssertARPReply(ft, tc.frame, tc.mac, tc.dst)
			}) {
				t.Fatal("AssertARPReply accepted invalid frame")
			}
		})
	}
}

func TestReadFrameRejectsBadSize(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	go func() {
		_, _ = a.Write([]byte{0, 0})
	}()
	if !runFakeT(func(ft *fakeT) {
		ReadFrame(ft, b, time.Second)
	}) {
		t.Fatal("ReadFrame accepted size=0 frame")
	}
}

func TestReadFrameRejectsOversizeFrame(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	go func() {
		_, _ = a.Write([]byte{0xff, 0xff}) // claims 65535-byte frame
	}()
	if !runFakeT(func(ft *fakeT) {
		ReadFrame(ft, b, time.Second)
	}) {
		t.Fatal("ReadFrame accepted oversize frame")
	}
}

func TestWriteFrameSurfacesWriteError(t *testing.T) {
	a, b := net.Pipe()
	_ = a.Close()
	_ = b.Close()
	if !runFakeT(func(ft *fakeT) {
		WriteFrame(ft, a, []byte("payload"))
	}) {
		t.Fatal("WriteFrame swallowed a closed-pipe error")
	}
}

func TestReadFrameSurfacesShortHeader(t *testing.T) {
	a, b := net.Pipe()
	_ = a.Close()
	_ = b.Close()
	if !runFakeT(func(ft *fakeT) {
		ReadFrame(ft, b, time.Second)
	}) {
		t.Fatal("ReadFrame swallowed a closed-pipe error on header read")
	}
}

func TestReadFrameSurfacesShortBody(t *testing.T) {
	a, b := net.Pipe()
	go func() {
		_, _ = a.Write([]byte{4, 0}) // promises 4 bytes, sends none
		_ = a.Close()
	}()
	if !runFakeT(func(ft *fakeT) {
		ReadFrame(ft, b, time.Second)
	}) {
		t.Fatal("ReadFrame accepted short body")
	}
	_ = b.Close()
}
