package sandboxnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/services/dns"
	"github.com/containers/gvisor-tap-vsock/pkg/services/forwarder"
	"github.com/inetaf/tcpproxy"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/localai-org/darkaifactory/internal/oops"
)

// tcpCreateEndpoint / udpCreateEndpoint are seams over the gvisor
// forwarder's endpoint creation so tests can drive the otherwise
// unreachable creation-failure branches. Production always uses the
// real CreateEndpoint.
//
//nolint:gochecknoglobals // test seams, mutated only by export_test.go under sequential tests.
var (
	tcpCreateEndpoint = func(r *tcp.ForwarderRequest, wq *waiter.Queue) (tcpip.Endpoint, tcpip.Error) {
		return r.CreateEndpoint(wq)
	}
	udpCreateEndpoint = func(r *udp.ForwarderRequest, wq *waiter.Queue) (tcpip.Endpoint, tcpip.Error) {
		return r.CreateEndpoint(wq)
	}
)

// linkLocalSubnet is the IPv4 link-local block. We never enable EC2
// metadata access, so dials into it are always refused — a guest
// reaching for 169.254.169.254 is either a cloud-init probe or an
// SSRF attempt, neither of which the sandbox should serve.
const linkLocalSubnet = "169.254.0.0/16"

// egressPermitted reports whether an outbound dial to the post-NAT
// destination IP dst is allowed.
//
// When deny is false the netstack is in legacy allow-all mode and
// every dial is permitted — the pre-Phase-J behaviour. When deny is
// true only the host loopback target is reachable: that is where
// host-side services (the caching proxy, reached by the guest via
// DefaultHostLoopbackIP and NAT-translated onto 127.0.0.1) live.
// Every other destination is WAN and refused.
//
// Port is not consulted yet: the loopback target is the host's own
// trusted services, so all its ports are in-scope. Restricting to
// the specific proxy/DNS ports is a J-phase refinement.
func egressPermitted(deny bool, dst string) bool {
	if !deny {
		return true
	}
	return dst == hostLoopbackTarget
}

// translate applies the NAT map to a guest destination address,
// returning the host-side address the forwarder should dial. The
// only entry we install is DefaultHostLoopbackIP → 127.0.0.1; any
// other address passes through unchanged (and, under deny, is then
// refused by egressPermitted).
func (n *Network) translate(addr tcpip.Address) tcpip.Address {
	n.natLock.Lock()
	defer n.natLock.Unlock()
	if replaced, ok := n.nat[addr]; ok {
		return replaced
	}
	return addr
}

// denyLog records a refused egress dial on the oops sink in the
// shape the Phase J plan calls for: an operator grepping for
// `egress-deny` sees the protocol and destination. It is also the
// "stop and consider" tripwire — an unexpected protocol or host
// showing up here means a workload needs a lane we haven't built.
func denyLog(proto, dst string, port uint16) {
	oops.Log("sandboxnet: egress-deny",
		fmt.Errorf("proto=%s dst=%s:%d", proto, dst, port))
}

// tcpForwarder mirrors gvisor-tap-vsock's stock TCP forwarder but
// consults the egress policy after NAT translation. Allowed dials
// take the identical net.Dial → tcpproxy path the upstream library
// uses; refused dials are completed with RST and logged.
func (n *Network) tcpForwarder(s *stack.Stack) *tcp.Forwarder {
	return tcp.NewForwarder(s, 0, 10, func(r *tcp.ForwarderRequest) {
		id := r.ID()
		local := id.LocalAddress
		if linkLocal().Contains(local) {
			r.Complete(true)
			return
		}
		dst := n.translate(local).String()
		if !egressPermitted(n.deny, dst) {
			denyLog("tcp", dst, id.LocalPort)
			r.Complete(true)
			return
		}

		outbound, err := net.Dial("tcp", net.JoinHostPort(dst, strconv.Itoa(int(id.LocalPort))))
		if err != nil {
			oops.Log("sandboxnet: tcp dial", err)
			r.Complete(true)
			return
		}

		var wq waiter.Queue
		ep, tcpErr := tcpCreateEndpoint(r, &wq)
		r.Complete(false)
		if tcpErr != nil {
			oops.Skip("sandboxnet: close outbound after endpoint failure",
				outbound.Close(), net.ErrClosed)
			oops.Log("sandboxnet: tcp create endpoint", errors.New(tcpErr.String()))
			return
		}

		remote := tcpproxy.DialProxy{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return outbound, nil
			},
		}
		remote.HandleConn(gonet.NewTCPConn(&wq, ep))
	})
}

// udpForwarder mirrors the stock UDP forwarder with the same policy
// check. In deny mode no legitimate UDP egress remains (DNS is
// served in-stack on the gateway, not through this forwarder), so
// every datagram to the WAN is dropped and logged.
func (n *Network) udpForwarder(s *stack.Stack) *udp.Forwarder {
	return udp.NewForwarder(s, func(r *udp.ForwarderRequest) {
		id := r.ID()
		local := id.LocalAddress
		if linkLocal().Contains(local) || local == header.IPv4Broadcast {
			return
		}
		dst := n.translate(local).String()
		if !egressPermitted(n.deny, dst) {
			denyLog("udp", dst, id.LocalPort)
			return
		}

		var wq waiter.Queue
		ep, tcpErr := udpCreateEndpoint(r, &wq)
		if tcpErr != nil {
			oops.Log("sandboxnet: udp create endpoint", errors.New(tcpErr.String()))
			return
		}

		proxy, err := forwarder.NewUDPProxy(
			autoStopUDPConn{gonet.NewUDPConn(&wq, ep)},
			func() (net.Conn, error) {
				return net.Dial("udp", net.JoinHostPort(dst, strconv.Itoa(int(id.LocalPort))))
			},
		)
		// NewUDPProxy's current implementation never returns an error;
		// route it through oops so a future change can't slip past us.
		oops.Skip("sandboxnet: new udp proxy", err)
		go func() {
			proxy.Run()
			ep.Close()
		}()
	})
}

// autoStopUDPConn reproduces gvisor-tap-vsock's unexported
// autoStoppingListener: it bumps a read deadline on every datagram
// so an idle UDP flow's reply loop unblocks and the endpoint is
// reclaimed after UDPConnTrackTimeout. NewUDPProxy's listener
// parameter is an unexported interface whose method set is fully
// exported, so we satisfy it from here without forking the library.
type autoStopUDPConn struct {
	*gonet.UDPConn
}

func (c autoStopUDPConn) ReadFrom(b []byte) (int, net.Addr, error) {
	c.bumpDeadline()
	return c.UDPConn.ReadFrom(b)
}

func (c autoStopUDPConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	c.bumpDeadline()
	return c.UDPConn.WriteTo(b, addr)
}

func (c autoStopUDPConn) bumpDeadline() {
	oops.Skip("sandboxnet: udp read deadline",
		c.SetReadDeadline(time.Now().Add(forwarder.UDPConnTrackTimeout)),
		net.ErrClosed)
}

// linkLocal parses the link-local subnet for the EC2-metadata guard.
func linkLocal() *tcpip.Subnet {
	_, parsed, _ := net.ParseCIDR(linkLocalSubnet)
	subnet, _ := tcpip.NewSubnet(
		tcpip.AddrFromSlice(parsed.IP),
		tcpip.MaskFromBytes(parsed.Mask),
	)
	return &subnet
}

// createNetStack builds the gvisor stack for one VM: IPv4 + ARP
// network protocols, TCP/UDP/ICMP transports, a single NIC bound to
// endpoint at the gateway IP, with spoofing + promiscuous mode on so
// the netstack answers for the gateway and any GatewayVirtualIPs.
// Faithful to gvisor-tap-vsock's createStack minus the bits we don't
// use (capture file, DHCP static leases).
func createNetStack(cfg Config, endpoint stack.LinkEndpoint) (*stack.Stack, error) {
	s := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol,
			arp.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
			udp.NewProtocol,
			icmp.NewProtocol4,
		},
	})

	if err := s.CreateNIC(1, endpoint); err != nil {
		return nil, errors.New(err.String())
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4Slice(net.ParseIP(cfg.GatewayIP).To4()).WithPrefix(),
	}, stack.AddressProperties{}); err != nil {
		return nil, errors.New(err.String())
	}
	s.SetSpoofing(1, true)
	s.SetPromiscuousMode(1, true)

	_, parsedSubnet, err := net.ParseCIDR(cfg.Subnet)
	if err != nil {
		return nil, fmt.Errorf("sandboxnet: subnet %q: %w", cfg.Subnet, err)
	}
	subnet, err := tcpip.NewSubnet(
		tcpip.AddrFromSlice(parsedSubnet.IP),
		tcpip.MaskFromBytes(parsedSubnet.Mask),
	)
	if err != nil {
		return nil, fmt.Errorf("sandboxnet: build subnet: %w", err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: subnet, NIC: 1}})
	return s, nil
}

// startDNSServer stands up the in-stack DNS server on the gateway's
// :53 (UDP + TCP), matching gvisor-tap-vsock. The guest's
// resolv.conf points at the gateway, so this is the resolver every
// container query hits; it answers configured zones and otherwise
// falls back to the host resolver. Phase J3 will swap the upstream
// fallback for a caching/record-replay resolver.
func startDNSServer(cfg Config, s *stack.Stack) error {
	gwAddr := tcpip.AddrFrom4Slice(net.ParseIP(cfg.GatewayIP).To4())
	udpConn, err := gonet.DialUDP(s, &tcpip.FullAddress{NIC: 1, Addr: gwAddr, Port: 53}, nil, ipv4.ProtocolNumber)
	if err != nil {
		return fmt.Errorf("sandboxnet: dns udp listen: %w", err)
	}
	tcpLn, err := gonet.ListenTCP(s, tcpip.FullAddress{NIC: 1, Addr: gwAddr, Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		return fmt.Errorf("sandboxnet: dns tcp listen: %w", err)
	}
	server, err := dns.New(udpConn, tcpLn, cfg.dnsZones)
	if err != nil {
		return fmt.Errorf("sandboxnet: dns server: %w", err)
	}
	go func() {
		oops.Skip("sandboxnet: dns serve udp", server.Serve(), net.ErrClosed)
	}()
	go func() {
		oops.Skip("sandboxnet: dns serve tcp", server.ServeTCP(), net.ErrClosed)
	}()
	return nil
}

// natTable converts the string NAT map into the tcpip-typed form the
// forwarder closures index. Built once at New time.
func natTable(m map[string]string) map[tcpip.Address]tcpip.Address {
	out := make(map[tcpip.Address]tcpip.Address, len(m))
	for src, dst := range m {
		out[tcpip.AddrFrom4Slice(net.ParseIP(src).To4())] = tcpip.AddrFrom4Slice(net.ParseIP(dst).To4())
	}
	return out
}
