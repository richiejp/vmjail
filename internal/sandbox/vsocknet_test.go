package sandbox

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/sandboxnet"
	"github.com/localai-org/darkaifactory/internal/sandboxnet/sandboxnettest"
)

const vsocknetReadTimeout = 5 * time.Second

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
					Net:        false,
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stop, _, err := startVsockNetReceiver(context.Background(), tc.cfg)
			if err != nil {
				t.Fatalf("startVsockNetReceiver: %v", err)
			}
			stop()
		})
	}
}

// TestStartVsockNetReceiverRoundTrip pins the byte path end to
// end: dial the listener as a "fake guest", send an ARP request
// for the gateway, and assert an ARP reply comes back. Also pins
// the observer's net-counter callback: after the round trip both
// rx_bytes and tx_bytes must be non-zero, since the ARP frame went
// in and a reply came out.
func TestStartVsockNetReceiverRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{
		HostSocket: filepath.Join(dir, "v.sock"),
		GuestCID:   3,
		Net:        true,
	}
	stop, collectNet, err := startVsockNetReceiver(context.Background(), cfg)
	if err != nil {
		t.Fatalf("startVsockNetReceiver: %v", err)
	}
	t.Cleanup(stop)
	if collectNet == nil {
		t.Fatal("collectNet nil; expected non-nil when net is enabled")
	}
	// Before any guest dials in, the closure must return nil so
	// the observer can omit the Net block — the "wired but never
	// connected" window is distinguishable from "wired and idle".
	if got := collectNet(); got != nil {
		t.Errorf("collectNet() = %+v before Accept, want nil", *got)
	}

	conn, err := net.Dial("unix", cfg.Vsock.HostSocket+"_9500")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	guestMAC := []byte{0x02, 0, 0, 0, 0, 1}
	frame := sandboxnettest.ARPRequest(guestMAC,
		[]byte{192, 168, 127, 2},
		net.ParseIP(sandboxnet.DefaultGatewayIP).To4())
	sandboxnettest.WriteFrame(t, conn, frame)
	reply := sandboxnettest.ReadFrame(t, conn, vsocknetReadTimeout)
	sandboxnettest.AssertARPReply(t, reply,
		sandboxnet.DefaultGatewayMAC, guestMAC)

	got := collectNet()
	if got == nil {
		t.Fatal("collectNet returned nil after round trip")
	}
	if got.RxBytes == 0 {
		t.Errorf("RxBytes = 0; want non-zero after sending ARP request")
	}
	if got.TxBytes == 0 {
		t.Errorf("TxBytes = 0; want non-zero after receiving ARP reply")
	}
}

// TestStartVsockNetReceiverNoCollectorWhenDisabled pins the
// invariant that the collector is nil when net is off — observers
// downstream rely on this to skip the Net block cleanly without
// invoking a closure that would deref a nil network.
func TestStartVsockNetReceiverNoCollectorWhenDisabled(t *testing.T) {
	stop, collectNet, err := startVsockNetReceiver(context.Background(), validCfg())
	if err != nil {
		t.Fatalf("startVsockNetReceiver: %v", err)
	}
	defer stop()
	if collectNet != nil {
		t.Fatal("collectNet non-nil, want nil when Vsock is nil")
	}
}

//nolint:dupl // structurally parallel to TracePort by design.
func TestStartVsockNetReceiverPreCleansStaleSocket(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{
		HostSocket: filepath.Join(dir, "v.sock"),
		GuestCID:   3,
		Net:        true,
	}

	stalePath := cfg.Vsock.HostSocket + "_9500"
	stale, err := net.Listen("unix", stalePath)
	if err != nil {
		t.Fatalf("preseed listener: %v", err)
	}
	_ = stale.Close()

	stop, _, err := startVsockNetReceiver(context.Background(), cfg)
	if err != nil {
		t.Fatalf("startVsockNetReceiver against stale socket: %v", err)
	}
	stop()
}

// TestStartVsockNetReceiverStopUnblocksConnectedGuest pins the
// teardown invariant: stop() must close any conns still owned by
// per-conn goroutines, otherwise gvproxy's rxStream parks in
// io.ReadFull and wg.Wait deadlocks.
func TestStartVsockNetReceiverStopUnblocksConnectedGuest(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{
		HostSocket: filepath.Join(dir, "v.sock"),
		GuestCID:   3,
		Net:        true,
	}
	stop, _, err := startVsockNetReceiver(context.Background(), cfg)
	if err != nil {
		t.Fatalf("startVsockNetReceiver: %v", err)
	}
	conn, err := net.Dial("unix", cfg.Vsock.HostSocket+"_9500")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	guestMAC := []byte{0x02, 0, 0, 0, 0, 1}
	frame := sandboxnettest.ARPRequest(guestMAC,
		[]byte{192, 168, 127, 2},
		net.ParseIP(sandboxnet.DefaultGatewayIP).To4())
	sandboxnettest.WriteFrame(t, conn, frame)
	_ = sandboxnettest.ReadFrame(t, conn, vsocknetReadTimeout)

	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stop() did not return within 3s; conn-tracking leak")
	}
}

func TestStartVsockNetReceiverSurfacesListenError(t *testing.T) {
	cfg := validCfg()
	cfg.Vsock = &VsockConfig{
		HostSocket: "/no/such/directory/v.sock",
		GuestCID:   3,
		Net:        true,
	}
	stop, _, err := startVsockNetReceiver(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error from Listen on bad path")
	}
	stop()
}
