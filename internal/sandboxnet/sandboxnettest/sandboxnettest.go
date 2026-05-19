// Package sandboxnettest provides test helpers for exercising
// sandboxnet's StdioProtocol byte path without a real VM.
//
// Lives outside the test file because two packages need it:
// sandboxnet itself (unit-test) and sandbox (vsocknet receiver
// test). A *_test.go file can't be imported from another
// package's tests.
package sandboxnettest

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

// EtherType ARP. The codebase has no other L2 surface; constants
// stay scoped here.
const (
	ethTypeARP   = 0x0806
	arpHWEther   = 1
	arpProtoIPv4 = 0x0800
	arpOpRequest = 1
	arpOpReply   = 2
)

// MaxFrameSize bounds ReadFrame so a malformed size header can't
// allocate megabytes; 1600 covers MTU 1500 plus eth header
// slack.
const MaxFrameSize = 1600

// ARPRequest assembles an Ethernet+ARP request asking for
// targetIP's MAC. Hand-rolled to RFC 826.
func ARPRequest(srcMAC, srcIP, targetIP []byte) []byte {
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

// WriteFrame writes one StdioProtocol frame: 2-byte LE size,
// then the frame bytes.
func WriteFrame(t testing.TB, w io.Writer, frame []byte) {
	t.Helper()
	hdr := make([]byte, 2)
	binary.LittleEndian.PutUint16(hdr, uint16(len(frame))) //nolint:gosec // ARP frame ≤ 42 bytes.
	if _, err := w.Write(append(hdr, frame...)); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
}

// ReadFrame reads one StdioProtocol frame within timeout.
func ReadFrame(t testing.TB, r net.Conn, timeout time.Duration) []byte {
	t.Helper()
	if err := r.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(r, hdr); err != nil {
		t.Fatalf("read size header: %v", err)
	}
	size := binary.LittleEndian.Uint16(hdr)
	if size == 0 || size > MaxFrameSize {
		t.Fatalf("ReadFrame: bad size %d", size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read frame body: %v", err)
	}
	return buf
}

// AssertARPReply checks that frame is an Ethernet ARP-reply
// addressed to wantDstMAC and sourced from wantSrcMACStr.
func AssertARPReply(t testing.TB, frame []byte, wantSrcMACStr string, wantDstMAC []byte) {
	t.Helper()
	if len(frame) < 42 {
		t.Fatalf("frame too short: %d bytes", len(frame))
	}
	if !bytes.Equal(frame[0:6], wantDstMAC) {
		t.Fatalf("eth dst MAC = %x, want %x", frame[0:6], wantDstMAC)
	}
	wantSrcMAC, err := net.ParseMAC(wantSrcMACStr)
	if err != nil {
		t.Fatalf("parse want src MAC: %v", err)
	}
	if !bytes.Equal(frame[6:12], wantSrcMAC) {
		t.Fatalf("eth src MAC = %x, want %x", frame[6:12], wantSrcMAC)
	}
	ethType := binary.BigEndian.Uint16(frame[12:14])
	if ethType != ethTypeARP {
		t.Fatalf("ethertype = 0x%04x, want 0x%04x", ethType, ethTypeARP)
	}
	arpOp := binary.BigEndian.Uint16(frame[20:22])
	if arpOp != arpOpReply {
		t.Fatalf("arp op = %d, want %d (reply)", arpOp, arpOpReply)
	}
}
