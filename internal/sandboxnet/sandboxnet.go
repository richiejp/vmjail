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

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
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
)

// Config describes the per-VM virtual network. Zero-valued
// fields fall back to package defaults; see DefaultMTU /
// DefaultSubnet / DefaultGatewayIP / DefaultGatewayMAC.
type Config struct {
	// Subnet is the CIDR the gateway serves. Frames whose dst IP
	// falls inside this range route into the netstack; everything
	// else gets NAT'd out via the host syscall layer.
	Subnet string
	// GatewayIP is the netstack's own address inside Subnet —
	// also the default route the guest learns via DHCP.
	GatewayIP string
	// GatewayMAC is the L2 address the netstack uses on the
	// virtual NIC. Must be a unicast MAC the guest can ARP-resolve.
	GatewayMAC string
	// MTU bounds the netstack's frame size. 0 → DefaultMTU.
	MTU int
}

// Network is one VM's userspace TCP/IP stack. Construct with New,
// hand accepted conns to Accept, and discard when the VM stops.
// All methods are safe for concurrent use.
type Network struct {
	vn *virtualnetwork.VirtualNetwork
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
	if _, _, err := net.ParseCIDR(cfg.Subnet); err != nil {
		return nil, fmt.Errorf("sandboxnet: subnet %q: %w", cfg.Subnet, err)
	}
	if ip := net.ParseIP(cfg.GatewayIP); ip == nil {
		return nil, fmt.Errorf("sandboxnet: gateway IP %q is not parseable", cfg.GatewayIP)
	}
	if _, err := net.ParseMAC(cfg.GatewayMAC); err != nil {
		return nil, fmt.Errorf("sandboxnet: gateway MAC %q: %w", cfg.GatewayMAC, err)
	}
	vn, err := virtualnetwork.New(&types.Configuration{
		Subnet:            cfg.Subnet,
		GatewayIP:         cfg.GatewayIP,
		GatewayMacAddress: cfg.GatewayMAC,
		MTU:               cfg.MTU,
		Protocol:          types.StdioProtocol,
	})
	if err != nil {
		return nil, fmt.Errorf("sandboxnet: virtualnetwork.New: %w", err)
	}
	return &Network{vn: vn}, nil
}

// Accept blocks until conn is closed or ctx is cancelled, ferrying
// StdioProtocol frames between conn and the netstack switch. The
// switch's CAM table learns the guest's MAC from the first frame;
// subsequent traffic to that MAC routes back to this conn.
//
// On any I/O error the switch internally disconnects the conn from
// the CAM table and returns; callers can re-Accept a fresh conn
// (e.g. after a guest reboot) on the same Network.
func (n *Network) Accept(ctx context.Context, conn net.Conn) error {
	return n.vn.AcceptStdio(ctx, conn)
}
