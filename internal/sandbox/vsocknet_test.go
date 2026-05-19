package sandbox

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/sandboxnet"
)

const vsocknetReadTimeout = 5 * time.Second

// TestStartVsockNetReceiverNilWhenDisabled returns a no-op stop
// when Vsock is nil or NetPort is zero. Caller can defer the
// returned stop unconditionally without checking the flag.
// Mirrors the TracePort no-op test by design — the two receivers
// share the same disable contract and we want a regression on
// either side to fail locally.
//
//nolint:dupl // structurally parallel to TracePort by design.
func TestStartVsockNetReceiverNilWhenDisabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"nil vsock", validCfg()},
		{
			name: "zero net port",
			cfg: Config{
				CloudHypervisor: "/c",
				Kernel:          "/k",
				Initramfs:       "/i",
				Vsock: &VsockConfig{
					HostSocket: testHostSocketPath,
					GuestCID:   3,
					NetPort:    0,
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stop, err := startVsockNetReceiver(context.Background(), tc.cfg)
			if err != nil {
				t.Fatalf("startVsockNetReceiver: %v", err)
			}
			stop() // must not panic
		})
	}
}

// TestStartVsockNetReceiverRoundTrip pins the byte path end to
// end: dial the listener as a "fake guest", send an ARP request
// for the gateway, and assert an ARP reply with the gateway MAC
// comes back. Exercises listener.Accept → goroutine → netstack →
// reply → conn write.
func TestStartVsockNetReceiverRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{
		HostSocket: filepath.Join(dir, "v.sock"),
		GuestCID:   3,
		NetPort:    8001,
	}

	stop, err := startVsockNetReceiver(context.Background(), cfg)
	if err != nil {
		t.Fatalf("startVsockNetReceiver: %v", err)
	}
	t.Cleanup(stop)

	addr := cfg.Vsock.HostSocket + "_8001"
	conn, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	guestMAC := []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
	guestIP := []byte{192, 168, 127, 2}
	gatewayIP := net.ParseIP(sandboxnet.DefaultGatewayIP).To4()
	frame := arpRequestFrame(guestMAC, guestIP, gatewayIP)
	writeNetFrame(t, conn, frame)

	reply := readNetFrame(t, conn, vsocknetReadTimeout)
	assertARPReplyFromGateway(t, reply, guestMAC)
}

// TestStartVsockNetReceiverPreCleansStaleSocket simulates a
// previous run crashing while a stale socket file remained. The
// new listener should remove it and bind successfully — without
// pre-clean Listen would fail with EADDRINUSE. Parallel to the
// trace-receiver counterpart by design.
//
//nolint:dupl // structurally parallel to TracePort by design.
func TestStartVsockNetReceiverPreCleansStaleSocket(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{
		HostSocket: filepath.Join(dir, "v.sock"),
		GuestCID:   3,
		NetPort:    8001,
	}

	stalePath := cfg.Vsock.HostSocket + "_8001"
	stale, err := net.Listen("unix", stalePath)
	if err != nil {
		t.Fatalf("preseed listener: %v", err)
	}
	_ = stale.Close() // leaves the socket file behind

	stop, err := startVsockNetReceiver(context.Background(), cfg)
	if err != nil {
		t.Fatalf("startVsockNetReceiver against stale socket: %v", err)
	}
	stop()
}

// TestStartVsockNetReceiverSurfacesListenError exercises the
// path where the listen syscall itself fails (parent dir missing).
// The stop func returned in that case must be safe to call.
func TestStartVsockNetReceiverSurfacesListenError(t *testing.T) {
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{
		HostSocket: "/no/such/directory/v.sock",
		GuestCID:   3,
		NetPort:    8001,
	}
	stop, err := startVsockNetReceiver(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error from Listen on bad path")
	}
	stop() // must not panic
}

// arpRequestFrame builds the same Ethernet+ARP request shape the
// sandboxnet unit test uses, just inlined here to keep test
// dependencies symmetrical with the trace-receiver tests.
func arpRequestFrame(srcMAC, srcIP, targetIP []byte) []byte {
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

func writeNetFrame(t *testing.T, w io.Writer, frame []byte) {
	t.Helper()
	hdr := make([]byte, 2)
	binary.LittleEndian.PutUint16(hdr, uint16(len(frame))) //nolint:gosec // ARP frame ≤ 42 bytes.
	if _, err := w.Write(append(hdr, frame...)); err != nil {
		t.Fatalf("writeNetFrame: %v", err)
	}
}

func readNetFrame(t *testing.T, r net.Conn, timeout time.Duration) []byte {
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
		t.Fatalf("readNetFrame: bad size %d", size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatalf("read frame body: %v", err)
	}
	return buf
}

func assertARPReplyFromGateway(t *testing.T, frame, wantDstMAC []byte) {
	t.Helper()
	if len(frame) < 42 {
		t.Fatalf("frame too short: %d bytes", len(frame))
	}
	if !bytes.Equal(frame[0:6], wantDstMAC) {
		t.Fatalf("eth dst MAC = %x, want %x", frame[0:6], wantDstMAC)
	}
	gatewayMAC, err := net.ParseMAC(sandboxnet.DefaultGatewayMAC)
	if err != nil {
		t.Fatalf("parse gateway MAC: %v", err)
	}
	if !bytes.Equal(frame[6:12], gatewayMAC) {
		t.Fatalf("eth src MAC = %x, want %x", frame[6:12], gatewayMAC)
	}
	ethType := binary.BigEndian.Uint16(frame[12:14])
	if ethType != 0x0806 {
		t.Fatalf("ethertype = 0x%04x, want 0x0806", ethType)
	}
	arpOp := binary.BigEndian.Uint16(frame[20:22])
	if arpOp != 2 {
		t.Fatalf("arp op = %d, want 2 (reply)", arpOp)
	}
}
