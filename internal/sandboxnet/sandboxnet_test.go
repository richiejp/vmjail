package sandboxnet_test

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/richiejp/vmjail/internal/sandboxnet"
	"github.com/richiejp/vmjail/internal/sandboxnet/sandboxnettest"
)

const readTimeout = 5 * time.Second

var (
	testGuestMAC = [6]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
	testGuestIP  = [4]byte{192, 168, 127, 2}
)

func TestNewDefaults(t *testing.T) {
	n, err := sandboxnet.New(sandboxnet.Config{})
	if err != nil {
		t.Fatalf("New(zero): %v", err)
	}
	if n == nil {
		t.Fatal("New returned nil Network")
	}
}

// TestNewRejectsBadConfig pins the boundary-input branches. Each
// case wires a single field to a known-bad value; New must
// reject before constructing the netstack.
func TestNewRejectsBadConfig(t *testing.T) {
	const (
		wantGatewayIP  = "gateway IP"
		wantLoopbackIP = "host loopback IP"
	)
	cases := []struct {
		name string
		cfg  sandboxnet.Config
		want string
	}{
		{"bad subnet", sandboxnet.Config{Subnet: "not-a-cidr"}, "subnet"},
		{"bad gateway IP", sandboxnet.Config{GatewayIP: "not-an-ip"}, wantGatewayIP},
		// IPv6 parses but has no To4(); New must reject it rather than
		// let createNetStack panic in AddrFrom4Slice.
		{"ipv6 gateway IP", sandboxnet.Config{GatewayIP: "::1"}, wantGatewayIP},
		{"bad gateway MAC", sandboxnet.Config{GatewayMAC: "not-a-mac"}, "gateway MAC"},
		{"bad host loopback IP", sandboxnet.Config{HostLoopbackIP: "not-an-ip"}, wantLoopbackIP},
		{"ipv6 host loopback IP", sandboxnet.Config{HostLoopbackIP: "fe80::1"}, wantLoopbackIP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sandboxnet.New(tc.cfg)
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
// asserts an ARP request comes back as an ARP reply with the
// gateway MAC. Failing here pins a library protocol shift
// before the real-VM e2e ever runs.
func TestAcceptRoundTripsARP(t *testing.T) {
	cases := []struct {
		name     string
		targetIP net.IP
	}{
		{"gateway IP", net.ParseIP(sandboxnet.DefaultGatewayIP).To4()},
		// .254 is the GatewayVirtualIP wired by HostLoopbackIP;
		// an ARP reply for it proves the netstack accepted the
		// extra address (without it the slice-4 integration test
		// can't even open a TCP conn to the host).
		{"host loopback IP", net.ParseIP(sandboxnet.DefaultHostLoopbackIP).To4()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, err := sandboxnet.New(sandboxnet.Config{})
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

			frame := sandboxnettest.ARPRequest(testGuestMAC[:], testGuestIP[:], tc.targetIP)
			sandboxnettest.WriteFrame(t, guestSide, frame)
			reply := sandboxnettest.ReadFrame(t, guestSide, readTimeout)
			sandboxnettest.AssertARPReply(t, reply, sandboxnet.DefaultGatewayMAC, testGuestMAC[:])

			cancel()
			_ = hostSide.Close()
			select {
			case <-acceptErr:
			case <-time.After(readTimeout):
				t.Fatal("Accept did not return after cancel")
			}
		})
	}
}
