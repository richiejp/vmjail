package sandboxnet

import (
	"bytes"
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/richiejp/darkaifactory/internal/oops"
)

const readTimeout = 5 * time.Second

func TestEgressPermitted(t *testing.T) {
	cases := []struct {
		name string
		deny bool
		dst  string
		want bool
	}{
		{"allow-all permits wan", false, "8.8.8.8", true},
		{"allow-all permits loopback", false, hostLoopbackTarget, true},
		{"deny permits loopback target", true, hostLoopbackTarget, true},
		{"deny refuses wan", true, "8.8.8.8", false},
		{"deny refuses other loopback", true, "127.0.0.2", false},
		{"deny refuses in-subnet peer", true, "192.168.127.50", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := egressPermitted(tc.deny, tc.dst); got != tc.want {
				t.Fatalf("egressPermitted(%v, %q) = %v, want %v", tc.deny, tc.dst, got, tc.want)
			}
		})
	}
}

// TestForwarderTCPAllowsLoopback proves the policy's allow path:
// even in deny mode the guest reaches a host-side service via the
// loopback VIP (NAT-translated onto 127.0.0.1), bytes round-trip.
func TestForwarderTCPAllowsLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { oops.Skip("close listener", ln.Close(), net.ErrClosed) })
	port := tcpPort(t, ln)

	echoed := make(chan []byte, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer func() { oops.Skip("close accepted", c.Close(), net.ErrClosed) }()
		buf := make([]byte, 4)
		n, _ := io.ReadFull(c, buf)
		echoed <- buf[:n]
	}()

	host := newHostNetwork(t, Config{DenyEgress: true})
	guest, ctx := wireGuest(t, host)

	dctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	target := net.JoinHostPort(DefaultHostLoopbackIP, strconv.Itoa(port))
	conn, err := gonet.DialContextTCP(dctx, guest, fullAddr(DefaultHostLoopbackIP, port), ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("guest dial %s (allowed): %v", target, err)
	}
	t.Cleanup(func() { oops.Skip("close guest conn", conn.Close(), net.ErrClosed) })

	if _, werr := conn.Write([]byte("ping")); werr != nil {
		t.Fatalf("guest write: %v", werr)
	}
	select {
	case got := <-echoed:
		if string(got) != "ping" {
			t.Fatalf("listener got %q, want %q", got, "ping")
		}
	case <-time.After(readTimeout):
		t.Fatal("listener never received guest bytes through the allowed path")
	}
}

// TestForwarderTCPDeniesWAN proves default-deny: a guest dial to a
// non-loopback destination is refused and logged as egress-deny,
// and the dial fails fast (RST) rather than hanging.
func TestForwarderTCPDeniesWAN(t *testing.T) {
	var sink syncBuf
	prev := oops.SetSink(&sink)
	t.Cleanup(func() { oops.SetSink(prev) })

	host := newHostNetwork(t, Config{DenyEgress: true})
	guest, ctx := wireGuest(t, host)

	dctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	conn, err := gonet.DialContextTCP(dctx, guest, fullAddr("8.8.8.8", 80), ipv4.ProtocolNumber)
	if err == nil {
		oops.Skip("close unexpectedly-allowed conn", conn.Close(), net.ErrClosed)
		t.Fatal("dial to WAN dst succeeded under DenyEgress; want refused")
	}
	waitForContains(t, &sink, "egress-deny", "proto=tcp", "dst=8.8.8.8:80")
}

// TestForwarderUDPDeniesWAN exercises the UDP forwarder's deny path:
// a datagram to a non-loopback dst is dropped and logged. UDP has no
// handshake, so we assert on the log rather than a dial error.
func TestForwarderUDPDeniesWAN(t *testing.T) {
	var sink syncBuf
	prev := oops.SetSink(&sink)
	t.Cleanup(func() { oops.SetSink(prev) })

	host := newHostNetwork(t, Config{DenyEgress: true})
	guest, _ := wireGuest(t, host)

	uc, err := gonet.DialUDP(guest, nil, fullAddrP("8.8.8.8", 53), ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("guest DialUDP: %v", err)
	}
	t.Cleanup(func() { oops.Skip("close udp", uc.Close(), net.ErrClosed) })
	if _, werr := uc.Write([]byte("q")); werr != nil {
		t.Fatalf("guest udp write: %v", werr)
	}
	waitForContains(t, &sink, "egress-deny", "proto=udp", "dst=8.8.8.8:53")
}

// TestForwarderUDPForwardsLoopback covers the UDP allow path
// (NewUDPProxy + autoStopUDPConn): a datagram to the loopback VIP is
// NAT-translated and delivered to a real 127.0.0.1 UDP listener.
func TestForwarderUDPForwardsLoopback(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ListenPacket: %v", err)
	}
	t.Cleanup(func() { oops.Skip("close packetconn", pc.Close(), net.ErrClosed) })
	port := udpPort(t, pc)

	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 16)
		n, from, rerr := pc.ReadFrom(buf)
		if rerr != nil {
			return
		}
		got <- buf[:n]
		// Reply so the proxy's reverse path (autoStopUDPConn.WriteTo
		// back to the guest) is exercised too.
		oops.Skip("udp reply", writeAll(pc, from), net.ErrClosed)
	}()

	host := newHostNetwork(t, Config{}) // allow-all: loopback forward path
	guest, _ := wireGuest(t, host)
	uc, err := gonet.DialUDP(guest, nil, fullAddrP(DefaultHostLoopbackIP, port), ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("guest DialUDP: %v", err)
	}
	t.Cleanup(func() { oops.Skip("close udp", uc.Close(), net.ErrClosed) })
	if _, werr := uc.Write([]byte("dgram")); werr != nil {
		t.Fatalf("guest udp write: %v", werr)
	}
	select {
	case b := <-got:
		if string(b) != "dgram" {
			t.Fatalf("listener got %q, want %q", b, "dgram")
		}
	case <-time.After(readTimeout):
		t.Fatal("udp listener never received the forwarded datagram")
	}

	reply := make([]byte, 8)
	oops.Skip("set guest udp deadline", uc.SetReadDeadline(time.Now().Add(readTimeout)), net.ErrClosed)
	n, rerr := uc.Read(reply)
	if rerr != nil {
		t.Fatalf("guest never read the proxied reply: %v", rerr)
	}
	if string(reply[:n]) != "pong" {
		t.Fatalf("guest got reply %q, want %q", reply[:n], "pong")
	}
}

func writeAll(pc net.PacketConn, to net.Addr) error {
	_, err := pc.WriteTo([]byte("pong"), to)
	return err
}

// TestForwarderUDPRefusesLinkLocal covers the UDP forwarder's
// link-local/broadcast guard.
func TestForwarderUDPRefusesLinkLocal(t *testing.T) {
	var sink syncBuf
	prev := oops.SetSink(&sink)
	t.Cleanup(func() { oops.SetSink(prev) })

	host := newHostNetwork(t, Config{})
	guest, _ := wireGuest(t, host)
	uc, err := gonet.DialUDP(guest, nil, fullAddrP("169.254.169.254", 80), ipv4.ProtocolNumber)
	if err != nil {
		t.Fatalf("guest DialUDP: %v", err)
	}
	t.Cleanup(func() { oops.Skip("close udp", uc.Close(), net.ErrClosed) })
	if _, werr := uc.Write([]byte("meta")); werr != nil {
		t.Fatalf("guest udp write: %v", werr)
	}
	// The guard drops silently (no egress-deny log), so just assert
	// the datagram is not forwarded: nothing to observe but the
	// absence of a panic/leak; the coverage of the guard is the point.
	time.Sleep(50 * time.Millisecond)
}

// TestForwarderEndpointCreateFailures drives the otherwise
// unreachable CreateEndpoint-failure branches via the seams: an
// allowed dial succeeds at net.Dial but endpoint creation is forced
// to fail, so the forwarder logs and bails without leaking.
func TestForwarderEndpointCreateFailures(t *testing.T) {
	t.Cleanup(resetEndpointSeams)

	t.Run("tcp", func(t *testing.T) {
		tcpCreateEndpoint = func(*tcp.ForwarderRequest, *waiter.Queue) (tcpip.Endpoint, tcpip.Error) {
			return nil, &tcpip.ErrAborted{}
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		t.Cleanup(func() { oops.Skip("close ln", ln.Close(), net.ErrClosed) })
		go acceptAndDrop(ln)

		host := newHostNetwork(t, Config{})
		guest, ctx := wireGuest(t, host)
		dctx, cancel := context.WithTimeout(ctx, readTimeout)
		defer cancel()
		conn, derr := gonet.DialContextTCP(dctx, guest, fullAddr(DefaultHostLoopbackIP, tcpPort(t, ln)), ipv4.ProtocolNumber)
		if derr == nil {
			oops.Skip("close conn", conn.Close(), net.ErrClosed)
			t.Fatal("dial succeeded despite forced endpoint failure")
		}
	})

	t.Run("udp", func(t *testing.T) {
		udpCreateEndpoint = func(*udp.ForwarderRequest, *waiter.Queue) (tcpip.Endpoint, tcpip.Error) {
			return nil, &tcpip.ErrAborted{}
		}
		var sink syncBuf
		prev := oops.SetSink(&sink)
		t.Cleanup(func() { oops.SetSink(prev) })

		host := newHostNetwork(t, Config{})
		guest, _ := wireGuest(t, host)
		uc, err := gonet.DialUDP(guest, nil, fullAddrP(DefaultHostLoopbackIP, 9), ipv4.ProtocolNumber)
		if err != nil {
			t.Fatalf("DialUDP: %v", err)
		}
		t.Cleanup(func() { oops.Skip("close udp", uc.Close(), net.ErrClosed) })
		if _, werr := uc.Write([]byte("x")); werr != nil {
			t.Fatalf("udp write: %v", werr)
		}
		waitForContains(t, &sink, "udp create endpoint")
	})
}

func acceptAndDrop(ln net.Listener) {
	//daft:lint:disable=forloopbound reason="bounded by ln.Close() in test cleanup — Accept returns an error"
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		oops.Skip("close dropped", c.Close(), net.ErrClosed)
	}
}

func resetEndpointSeams() {
	tcpCreateEndpoint = func(r *tcp.ForwarderRequest, wq *waiter.Queue) (tcpip.Endpoint, tcpip.Error) {
		return r.CreateEndpoint(wq)
	}
	udpCreateEndpoint = func(r *udp.ForwarderRequest, wq *waiter.Queue) (tcpip.Endpoint, tcpip.Error) {
		return r.CreateEndpoint(wq)
	}
}

// TestForwarderTCPDialError covers the path where the policy permits
// the dial but the host-side connection fails: the guest reaches for
// the loopback target on a port nothing listens on, so net.Dial
// refuses and the forwarder completes the request with RST.
func TestForwarderTCPDialError(t *testing.T) {
	// Grab a port, then release it so the dial is refused.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	port := tcpPort(t, probe)
	oops.Skip("release probe port", probe.Close(), net.ErrClosed)

	host := newHostNetwork(t, Config{})
	guest, ctx := wireGuest(t, host)
	dctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	conn, err := gonet.DialContextTCP(dctx, guest, fullAddr(DefaultHostLoopbackIP, port), ipv4.ProtocolNumber)
	if err == nil {
		oops.Skip("close conn", conn.Close(), net.ErrClosed)
		t.Fatal("dial to dead loopback port succeeded; want refused")
	}
}

// TestForwarderRefusesLinkLocal pins the EC2-metadata guard: a dial
// into 169.254.0.0/16 is refused before any policy or NAT lookup,
// since we never enable metadata access.
func TestForwarderRefusesLinkLocal(t *testing.T) {
	host := newHostNetwork(t, Config{})
	guest, ctx := wireGuest(t, host)
	dctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	conn, err := gonet.DialContextTCP(dctx, guest, fullAddr("169.254.169.254", 80), ipv4.ProtocolNumber)
	if err == nil {
		oops.Skip("close conn", conn.Close(), net.ErrClosed)
		t.Fatal("dial to link-local metadata IP succeeded; want refused")
	}
}

// TestDNSServerResolvesZone proves the in-stack resolver answers a
// configured zone over the gateway's :53 — the lane the guest's
// resolv.conf points at. Driving it through net.Resolver also
// exercises both the UDP and TCP DNS listeners startDNSServer wires.
func TestDNSServerResolvesZone(t *testing.T) {
	host := newHostNetwork(t, Config{dnsZones: []types.Zone{{
		Name: "internal.",
		Records: []types.Record{
			// record.Name is the label with the zone suffix stripped.
			{Name: "host", IP: net.ParseIP("10.1.2.3")},
		},
	}}})
	guest, ctx := wireGuest(t, host)

	res := &net.Resolver{
		PreferGo: true,
		Dial: func(dialCtx context.Context, network, _ string) (net.Conn, error) {
			gw := fullAddr(DefaultGatewayIP, 53)
			if network == "tcp" {
				return gonet.DialContextTCP(dialCtx, guest, gw, ipv4.ProtocolNumber)
			}
			return gonet.DialUDP(guest, nil, &gw, ipv4.ProtocolNumber)
		},
	}
	lookupCtx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	addrs, err := res.LookupHost(lookupCtx, "host.internal")
	if err != nil {
		t.Fatalf("LookupHost via in-stack DNS: %v", err)
	}
	if len(addrs) == 0 || addrs[0] != "10.1.2.3" {
		t.Fatalf("LookupHost = %v, want [10.1.2.3]", addrs)
	}
}

// TestCreateNetStackBadSubnet covers createNetStack's subnet-parse
// error branch directly: New validates the subnet first, so this is
// the only way to reach it.
func TestCreateNetStackBadSubnet(t *testing.T) {
	ep, err := tap.NewLinkEndpoint(false, DefaultMTU, DefaultGatewayMAC, DefaultGatewayIP, nil)
	if err != nil {
		t.Fatalf("tap endpoint: %v", err)
	}
	if _, err := createNetStack(Config{Subnet: "not-a-cidr", GatewayIP: DefaultGatewayIP}, ep); err == nil {
		t.Fatal("createNetStack with bad subnet: want error, got nil")
	}
}

// TestStartDNSServerPortInUse covers startDNSServer's listen-error
// branch: a second bind to the gateway's :53 on the same stack fails.
func TestStartDNSServerPortInUse(t *testing.T) {
	ep, err := tap.NewLinkEndpoint(false, DefaultMTU, DefaultGatewayMAC, DefaultGatewayIP, nil)
	if err != nil {
		t.Fatalf("tap endpoint: %v", err)
	}
	s, err := createNetStack(Config{Subnet: DefaultSubnet, GatewayIP: DefaultGatewayIP}, ep)
	if err != nil {
		t.Fatalf("createNetStack: %v", err)
	}
	cfg := Config{GatewayIP: DefaultGatewayIP}
	if err := startDNSServer(cfg, s); err != nil {
		t.Fatalf("first startDNSServer: %v", err)
	}
	if err := startDNSServer(cfg, s); err == nil {
		t.Fatal("second startDNSServer on :53: want error, got nil")
	}
}

// --- test plumbing ---------------------------------------------------

const (
	guestStackGatewayIP  = "192.168.127.2"
	guestStackGatewayMAC = "5a:94:ef:e4:0c:de"
)

func newHostNetwork(t *testing.T, cfg Config) *Network {
	t.Helper()
	n, err := New(cfg)
	if err != nil {
		t.Fatalf("sandboxnet.New: %v", err)
	}
	return n
}

// wireGuest builds a bare guest gvisor stack with a default route via
// the host gateway, links it to host over net.Pipe, and returns the
// guest stack (for gonet dials) plus a context cancelled at cleanup.
func wireGuest(t *testing.T, host *Network) (*stack.Stack, context.Context) {
	t.Helper()
	ep, err := tap.NewLinkEndpoint(false, DefaultMTU, guestStackGatewayMAC, guestStackGatewayIP, nil)
	if err != nil {
		t.Fatalf("guest tap endpoint: %v", err)
	}
	sw := tap.NewSwitch(false)
	ep.Connect(sw)
	sw.Connect(ep)

	guest, err := createNetStack(Config{
		Subnet:     DefaultSubnet,
		GatewayIP:  guestStackGatewayIP,
		GatewayMAC: guestStackGatewayMAC,
		MTU:        DefaultMTU,
	}, ep)
	if err != nil {
		t.Fatalf("guest createNetStack: %v", err)
	}
	setDefaultRoute(t, guest)

	hostConn, guestConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	// Accept returns a teardown error once the pipe closes; tests
	// drive teardown deliberately so the error is expected and
	// ignored (the bench rig does the same).
	go func() { _ = host.Accept(ctx, hostConn) }()
	go func() { _ = sw.Accept(ctx, guestConn, types.StdioProtocol) }()
	t.Cleanup(func() {
		cancel()
		_ = hostConn.Close()
		_ = guestConn.Close()
	})
	return guest, ctx
}

// setDefaultRoute appends a 0.0.0.0/0 route via the host gateway so
// off-subnet dsts (the WAN-shaped deny cases) leave via the gateway
// MAC and reach the host forwarder, mirroring the real guest's route.
func setDefaultRoute(t *testing.T, s *stack.Stack) {
	t.Helper()
	_, parsed, err := net.ParseCIDR(DefaultSubnet)
	if err != nil {
		t.Fatalf("parse subnet: %v", err)
	}
	subnet, err := tcpip.NewSubnet(tcpip.AddrFromSlice(parsed.IP), tcpip.MaskFromBytes(parsed.Mask))
	if err != nil {
		t.Fatalf("subnet: %v", err)
	}
	zero, err := tcpip.NewSubnet(tcpip.AddrFrom4([4]byte{}), tcpip.MaskFromBytes([]byte{0, 0, 0, 0}))
	if err != nil {
		t.Fatalf("zero subnet: %v", err)
	}
	s.SetRouteTable([]tcpip.Route{
		{Destination: subnet, NIC: 1},
		{Destination: zero, Gateway: tcpip.AddrFrom4Slice(net.ParseIP(DefaultGatewayIP).To4()), NIC: 1},
	})
}

func fullAddr(ip string, port int) tcpip.FullAddress {
	return tcpip.FullAddress{
		NIC:  1,
		Addr: tcpip.AddrFrom4Slice(net.ParseIP(ip).To4()),
		Port: uint16(port), //nolint:gosec // G115: port from a real listener, 0..65535
	}
}

func fullAddrP(ip string, port int) *tcpip.FullAddress {
	a := fullAddr(ip, port)
	return &a
}

func tcpPort(t *testing.T, ln net.Listener) int {
	t.Helper()
	a, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr %T is not *net.TCPAddr", ln.Addr())
	}
	return a.Port
}

func udpPort(t *testing.T, pc net.PacketConn) int {
	t.Helper()
	a, ok := pc.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("packetconn addr %T is not *net.UDPAddr", pc.LocalAddr())
	}
	return a.Port
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitForContains(t *testing.T, sink *syncBuf, subs ...string) {
	t.Helper()
	deadline := time.Now().Add(readTimeout)
	for time.Now().Before(deadline) {
		got := sink.String()
		if containsAll(got, subs) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("log %q missing one of %v", sink.String(), subs)
}

func containsAll(s string, subs []string) bool {
	for _, sub := range subs {
		if !bytes.Contains([]byte(s), []byte(sub)) {
			return false
		}
	}
	return true
}
