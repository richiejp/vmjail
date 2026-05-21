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
			stop()
		})
	}
}

// TestStartVsockNetReceiverRoundTrip pins the byte path end to
// end: dial the listener as a "fake guest", send an ARP request
// for the gateway, and assert an ARP reply comes back.
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

	conn, err := net.Dial("unix", cfg.Vsock.HostSocket+"_8001")
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
}

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
	_ = stale.Close()

	stop, err := startVsockNetReceiver(context.Background(), cfg)
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
		NetPort:    8001,
	}
	stop, err := startVsockNetReceiver(context.Background(), cfg)
	if err != nil {
		t.Fatalf("startVsockNetReceiver: %v", err)
	}
	conn, err := net.Dial("unix", cfg.Vsock.HostSocket+"_8001")
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
		NetPort:    8001,
	}
	stop, err := startVsockNetReceiver(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error from Listen on bad path")
	}
	stop()
}
