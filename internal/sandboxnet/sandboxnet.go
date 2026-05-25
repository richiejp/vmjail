// Package sandboxnet runs a per-VM userspace TCP/IP stack on the
// sandboxd side, so a guest can reach the host network without
// the host process needing CAP_NET_ADMIN or a tap+bridge setuid
// helper. The stack lives entirely inside
// gvisor-tap-vsock/virtualnetwork; sandboxd ferries Ethernet
// frames between it and the guest over Cloud Hypervisor's
// hybrid-vsock UDS hop.
//
// Wire shape, host side:
//
//	guest agent --AF_VSOCK--> CH --UDS--> sandboxd listener --> (*Network).Accept
//
// Frames on the conn are StdioProtocol: <u16-le size><eth frame>.
// One Network per VM. Multiple conns to the same Network are
// dispatched by the embedded switch on Ethernet MAC; for our
// one-guest-per-VM model only one conn is ever active, but
// reconnection is handled transparently.
package sandboxnet

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// DefaultMTU is 1500 to match the host's underlying interface,
// so the netstack doesn't generate frames the host stack would
// have to fragment when it dials out to the real internet. The
// gvproxy default is 4000, suited to virtio's large-MTU mode;
// we'd save a per-packet syscall there but pay it back in
// MSS-clamping complexity on the way out.
const DefaultMTU = 1500

// Default subnet / gateway shipped by gvproxy's own examples and
// reused by Podman Machine. The 192.168.127.0/24 block is
// unrouted on the host, so it can't collide with the host's real
// addresses; it lives entirely inside the per-VM netstack.
const (
	DefaultSubnet     = "192.168.127.0/24"
	DefaultGatewayIP  = "192.168.127.1"
	DefaultGatewayMAC = "5a:94:ef:e4:0c:dd"

	// DefaultHostLoopbackIP is the virtual IP the guest dials to
	// reach the sandboxd process's 127.0.0.1 — the netstack's
	// NAT rewrites the destination on the way out, and
	// GatewayVirtualIPs makes the gateway answer ARP for it.
	// Matches the convention podman machine and gvproxy use.
	// Exported because the integration test points a guest-side
	// HTTPS client at this exact address.
	DefaultHostLoopbackIP = "192.168.127.254"

	// hostLoopbackTarget is the host-side address the netstack
	// translates DefaultHostLoopbackIP to. Always 127.0.0.1 —
	// any other address would defeat the "reach my own services"
	// purpose of the magic IP.
	hostLoopbackTarget = "127.0.0.1"
)

// Config describes the per-VM virtual network. Zero-valued
// fields fall back to package defaults.
type Config struct {
	Subnet     string
	GatewayIP  string
	GatewayMAC string
	MTU        int
	// HostLoopbackIP enables the guest→host loopback escape
	// hatch. When non-empty, the netstack answers ARP for this IP
	// on the gateway interface and NATs outbound traffic to it
	// onto 127.0.0.1 of the sandboxd process. Empty leaves the
	// guest with no route to the host's loopback. Defaults to
	// DefaultHostLoopbackIP when zero-valued — keep the slice-4
	// integration test path easy to reach without ceremony.
	HostLoopbackIP string

	// DenyEgress switches the netstack to default-deny: the only
	// outbound dials the TCP/UDP forwarders make are to the host
	// loopback target (the post-NAT 127.0.0.1) — i.e. host-side
	// services such as the caching proxy reached via
	// DefaultHostLoopbackIP. Every other dial is dropped and logged
	// as `egress-deny`. Defaults to false (allow all) to preserve
	// the pre-Phase-J path until proxy-env injection (J2) routes all
	// legitimate egress through the loopback proxy; J5 flips the
	// suite default.
	DenyEgress bool

	// dnsZones are the static DNS records the in-stack resolver
	// serves before falling back to the host resolver. Unexported:
	// no caller sets zones yet, but the field keeps the DNS server
	// wiring honest for J3.
	dnsZones []types.Zone
}

// Network is one VM's userspace TCP/IP stack. Construct with New,
// hand accepted conns to Accept, and discard when the VM stops.
// All methods are safe for concurrent use.
//
// Unlike the stock gvisor-tap-vsock virtualnetwork, this stack owns
// its TCP/UDP forward path so it can enforce an egress policy (see
// forward.go) — the library's forwarders hardcode net.Dial with no
// allowlist hook, and the stack they wire it onto is unexported, so
// there is no way to interpose after construction.
type Network struct {
	sw      *tap.Switch
	nat     map[tcpip.Address]tcpip.Address
	natLock sync.Mutex
	deny    bool
}

// New constructs the netstack from cfg. The returned Network has
// no conns attached yet; call Accept to plug one in.
func New(cfg Config) (*Network, error) {
	if cfg.Subnet == "" {
		cfg.Subnet = DefaultSubnet
	}
	if cfg.GatewayIP == "" {
		cfg.GatewayIP = DefaultGatewayIP
	}
	if cfg.GatewayMAC == "" {
		cfg.GatewayMAC = DefaultGatewayMAC
	}
	if cfg.MTU == 0 {
		cfg.MTU = DefaultMTU
	}
	if cfg.HostLoopbackIP == "" {
		cfg.HostLoopbackIP = DefaultHostLoopbackIP
	}
	if _, _, err := net.ParseCIDR(cfg.Subnet); err != nil {
		return nil, fmt.Errorf("sandboxnet: subnet %q: %w", cfg.Subnet, err)
	}
	// Must be IPv4: the netstack builds tcpip addresses via
	// AddrFrom4Slice(ip.To4()), which panics on a non-IPv4 address.
	// net.ParseIP accepts IPv6, so check To4() explicitly here rather
	// than crashing later in createNetStack.
	if ip := net.ParseIP(cfg.GatewayIP); ip == nil || ip.To4() == nil {
		return nil, fmt.Errorf("sandboxnet: gateway IP %q is not a valid IPv4 address", cfg.GatewayIP)
	}
	if _, err := net.ParseMAC(cfg.GatewayMAC); err != nil {
		return nil, fmt.Errorf("sandboxnet: gateway MAC %q: %w", cfg.GatewayMAC, err)
	}
	var gatewayVIPs []string
	var natMap map[string]string
	if cfg.HostLoopbackIP != "" {
		if ip := net.ParseIP(cfg.HostLoopbackIP); ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("sandboxnet: host loopback IP %q is not a valid IPv4 address", cfg.HostLoopbackIP)
		}
		gatewayVIPs = []string{cfg.HostLoopbackIP}
		natMap = map[string]string{cfg.HostLoopbackIP: hostLoopbackTarget}
	}

	//nolint:gosec // G115: MTU is a small positive config value, defaulted to DefaultMTU (1500).
	tapEndpoint, err := tap.NewLinkEndpoint(false, uint32(cfg.MTU), cfg.GatewayMAC, cfg.GatewayIP, gatewayVIPs)
	if err != nil {
		return nil, fmt.Errorf("sandboxnet: tap endpoint: %w", err)
	}
	sw := tap.NewSwitch(false)
	tapEndpoint.Connect(sw)
	sw.Connect(tapEndpoint)

	s, err := createNetStack(cfg, tapEndpoint)
	if err != nil {
		return nil, err
	}

	n := &Network{sw: sw, nat: natTable(natMap), deny: cfg.DenyEgress}
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, n.tcpForwarder(s).HandlePacket)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, n.udpForwarder(s).HandlePacket)
	if err := startDNSServer(cfg, s); err != nil {
		return nil, err
	}
	return n, nil
}

// Accept blocks until conn is closed or ctx is cancelled, ferrying
// StdioProtocol frames between conn and the netstack switch. The
// switch's CAM table learns the guest's MAC from the first frame;
// subsequent traffic to that MAC routes back to this conn. On any
// I/O error the switch internally disconnects the conn from the
// CAM table — callers can re-Accept a fresh conn (guest reboot) on
// the same Network.
func (n *Network) Accept(ctx context.Context, conn net.Conn) error {
	return n.sw.Accept(ctx, conn, types.StdioProtocol)
}
