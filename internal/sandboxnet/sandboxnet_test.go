package sandboxnet

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

const readTimeout = 5 * time.Second

// fake guest constants
var (
	testGuestMAC = [6]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
	testGuestIP  = [4]byte{192, 168, 127, 2}
)

// TestNewDefaults verifies zero-valued Config fields fall back to
// the package defaults rather than producing a broken netstack.
func TestNewDefaults(t *testing.T) {
	n, err := New(Config{})
	if err != nil {
		t.Fatalf("New(zero): %v", err)
	}
	if n == nil {
		t.Fatal("New returned nil Network")
	}
}

// TestNewRejectsBadConfig pins the boundary-input branches. Each
// case wires a single field to a known-bad value; New should
// reject before constructing the netstack.
func TestNewRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "bad subnet",
			cfg:  Config{Subnet: "not-a-cidr"},
			want: "subnet",
		},
		{
			name: "bad gateway IP",
			cfg:  Config{GatewayIP: "not-an-ip"},
			want: "gateway IP",
		},
		{
			name: "bad gateway MAC",
			cfg:  Config{GatewayMAC: "not-a-mac"},
			want: "gateway MAC",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil {
				t.Fatalf("New(%+v): want error, got nil", tc.cfg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New error = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

// TestAcceptRoundTripsARP pipes a net.Conn through Accept and
// asserts an ARP request for the gateway IP comes back as an ARP
// reply with the gateway MAC. The byte path exercised here is the
// same one the production wiring uses on every accepted UDS conn:
// read header, parse Ethernet, dispatch to netstack, frame response
// back. If the library's protocol shape ever shifts under us, this
// test will fail before any real VM is involved.
func TestAcceptRoundTripsARP(t *testing.T) {
	n, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	hostSide, guestSide := net.Pipe()
	t.Cleanup(func() {
		_ = hostSide.Close()
		_ = guestSide.Close()
	})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	acceptErr := make(chan error, 1)
	go func() {
		acceptErr <- n.Accept(ctx, hostSide)
	}()

	frame := buildARPRequest(testGuestMAC[:], testGuestIP[:],
		net.ParseIP(DefaultGatewayIP).To4())
	writeFrame(t, guestSide, frame)

	reply := readFrame(t, guestSide, readTimeout)
	assertARPReply(t, reply, DefaultGatewayMAC, testGuestMAC[:])

	cancel()
	_ = hostSide.Close()
	select {
	case <-acceptErr:
	case <-time.After(readTimeout):
		t.Fatal("Accept did not return after cancel")
	}
}

// buildARPRequest assembles an Ethernet+ARP request asking for
// targetIP's MAC. Hand-rolled per RFC 826 to avoid pulling a
// packet builder into the test.
func buildARPRequest(srcMAC, srcIP, targetIP []byte) []byte {
	const (
		ethTypeARP   = 0x0806
		arpHWEther   = 1
		arpProtoIPv4 = 0x0800
		arpOpRequest = 1
	)
	const broadcast = "\xff\xff\xff\xff\xff\xff"
	const targetUnknown = "\x00\x00\x00\x00\x00\x00"
	buf := make([]byte, 0, 42)
	buf = append(buf, broadcast...)
	buf = append(buf, srcMAC...)
	buf = binary.BigEndian.AppendUint16(buf, ethTypeARP)
	buf = binary.BigEndian.AppendUint16(buf, arpHWEther)
	buf = binary.BigEndian.AppendUint16(buf, arpProtoIPv4)
	buf = append(buf, 6, 4)
	buf = binary.BigEndian.AppendUint16(buf, arpOpRequest)
	buf = append(buf, srcMAC...)
	buf = append(buf, srcIP...)
	buf = append(buf, targetUnknown...)
	buf = append(buf, targetIP...)
	return buf
}

// writeFrame writes one StdioProtocol frame: 2-byte LE size, then
// the frame bytes. Mirrors gvforwarder's rx() loop.
func writeFrame(t *testing.T, w io.Writer, frame []byte) {
	t.Helper()
	hdr := make([]byte, 2)
	binary.LittleEndian.PutUint16(hdr, uint16(len(frame))) //nolint:gosec // ARP frame ≤ 42 bytes.
	if _, err := w.Write(append(hdr, frame...)); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
}

// readFrame reads one StdioProtocol frame within timeout.
func readFrame(t *testing.T, r net.Conn, timeout time.Duration) []byte {
	t.Helper()
	if err := r.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(r, hdr); err != nil {
		t.Fatalf("read size header: %v", err)
	}
	size := binary.LittleEndian.Uint16(hdr)
	if size == 0 || size > 1600 {
		t.Fatalf("readFrame: bad size %d", size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read frame body: %v", err)
	}
	return buf
}

// assertARPReply checks that frame is an Ethernet ARP-reply
// addressed to wantDstMAC and sourced from wantSrcMACStr.
func assertARPReply(t *testing.T, frame []byte, wantSrcMACStr string, wantDstMAC []byte) {
	t.Helper()
	if len(frame) < 42 {
		t.Fatalf("frame too short: %d bytes", len(frame))
	}
	gotDstMAC := frame[0:6]
	if !bytes.Equal(gotDstMAC, wantDstMAC) {
		t.Fatalf("eth dst MAC = %x, want %x", gotDstMAC, wantDstMAC)
	}
	wantSrcMAC, err := net.ParseMAC(wantSrcMACStr)
	if err != nil {
		t.Fatalf("parse want src MAC: %v", err)
	}
	gotSrcMAC := frame[6:12]
	if !bytes.Equal(gotSrcMAC, wantSrcMAC) {
		t.Fatalf("eth src MAC = %x, want %x", gotSrcMAC, wantSrcMAC)
	}
	ethType := binary.BigEndian.Uint16(frame[12:14])
	if ethType != 0x0806 {
		t.Fatalf("ethertype = 0x%04x, want 0x0806 (ARP)", ethType)
	}
	arpOp := binary.BigEndian.Uint16(frame[20:22])
	if arpOp != 2 {
		t.Fatalf("arp op = %d, want 2 (reply)", arpOp)
	}
}
