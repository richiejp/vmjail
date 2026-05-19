// Package sandboxnet is a spike for D6c.2: prove that
// gvisor-tap-vsock's VirtualNetwork can accept a plain net.Conn,
// run gVisor netstack as the gateway, and round-trip an Ethernet
// frame through the StdioProtocol (2-byte little-endian length
// prefix, no HTTP handshake). This is the byte shape we'd get on
// the host side of Cloud Hypervisor's hybrid-vsock UDS hop, where
// the guest dials AF_VSOCK CID 2 port N and CH proxies that
// stream onto a host Unix socket sandboxd owns. No CH or VM in
// this test — just the in-process library wiring.
//
// Asserts: the netstack answers an ARP request for the gateway
// IP with a correctly-shaped ARP reply. That proves the conn
// is being read, parsed, fed to the netstack, the netstack
// generates a response, and the response is framed back onto
// the conn. Enough to green-light wiring this library into the
// real path; everything beyond (TCP through to host net.Dial,
// DHCP server replies, etc.) is gvproxy's own scope.
package sandboxnet

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

// virtualNetwork configuration constants for the spike. Values
// are arbitrary — they only need to be self-consistent and avoid
// conflicting with anything else on the host (no kernel route is
// installed; this all lives inside gvisor netstack).
const (
	spikeSubnet      = "192.168.127.0/24"
	spikeGatewayIP   = "192.168.127.1"
	spikeGatewayMAC  = "5a:94:ef:e4:0c:dd"
	spikeMTU         = 1500
	spikeReadTimeout = 5 * time.Second
)

// fake guest constants
var (
	spikeGuestMAC = [6]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
	spikeGuestIP  = [4]byte{192, 168, 127, 2}
)

func TestSpikeARPRoundTripsThroughStdioProtocol(t *testing.T) {
	vn, err := virtualnetwork.New(&types.Configuration{
		Subnet:            spikeSubnet,
		GatewayIP:         spikeGatewayIP,
		GatewayMacAddress: spikeGatewayMAC,
		MTU:               spikeMTU,
		Protocol:          types.StdioProtocol,
	})
	if err != nil {
		t.Fatalf("virtualnetwork.New: %v", err)
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
		acceptErr <- vn.AcceptStdio(ctx, hostSide)
	}()

	frame := buildARPRequest(spikeGuestMAC[:], spikeGuestIP[:], net.ParseIP(spikeGatewayIP).To4())
	writeFrame(t, guestSide, frame)

	reply := readFrame(t, guestSide, spikeReadTimeout)
	assertARPReply(t, reply, spikeGatewayMAC, spikeGuestMAC[:])

	cancel()
	// AcceptStdio's rx loop unblocks when its context is cancelled
	// or the conn is closed. Either way the goroutine should exit
	// promptly; surface any unexpected error.
	_ = hostSide.Close()
	select {
	case <-acceptErr:
	case <-time.After(spikeReadTimeout):
		t.Fatal("AcceptStdio did not return after cancel")
	}
}

// buildARPRequest assembles an Ethernet+ARP request asking for
// targetIP's MAC. RFC 826 layout, hand-rolled to avoid pulling
// a packet builder into the spike.
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

// writeFrame writes a StdioProtocol frame: 2-byte LE size, then
// the frame bytes. Mirrors gvforwarder's rx() loop.
func writeFrame(t *testing.T, w io.Writer, frame []byte) {
	t.Helper()
	hdr := make([]byte, 2)
	binary.LittleEndian.PutUint16(hdr, uint16(len(frame))) //nolint:gosec // ARP frame ≤ 42 bytes.
	if _, err := w.Write(append(hdr, frame...)); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}
}

// readFrame reads one StdioProtocol frame within timeout. Returns
// the frame bytes (without the size header). Fatals on timeout or
// short read — the test that calls this would have nothing
// meaningful to say after either failure.
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
// addressed to wantDstMAC and sourced from wantSrcMAC.
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
