package sandboxnet

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/containers/gvisor-tap-vsock/pkg/virtualnetwork"
)

const proxyDialTimeout = 5 * time.Second

// TestStartProxyServerServesGuestConn pins the J6 in-stack proxy lane:
// a guest dialing gateway:DefaultProxyPort terminates at the in-stack
// HTTP server (no NAT, no host TCP) and gets the configured handler's
// response. Two netstacks are wired link-to-link over a net.Pipe (the
// same StdioProtocol path sandboxd uses); the guest side is a barebones
// virtualnetwork used purely as a TCP source — mirrors the forwarder
// throughput bench's rig.
func TestStartProxyServerServesGuestConn(t *testing.T) {
	const wantBody = "proxied-ok"
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, wantBody)
	})

	hostNet, err := New(Config{ProxyHandler: handler})
	if err != nil {
		t.Fatalf("New(host): %v", err)
	}
	guestNet, err := virtualnetwork.New(&types.Configuration{
		Subnet:            DefaultSubnet,
		GatewayIP:         "192.168.127.2",
		GatewayMacAddress: "5a:94:ef:e4:0c:de",
		MTU:               DefaultMTU,
		Protocol:          types.StdioProtocol,
	})
	if err != nil {
		t.Fatalf("virtualnetwork.New(guest): %v", err)
	}

	hostConn, guestConn := net.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		_ = hostConn.Close()
		_ = guestConn.Close()
	})
	go func() { _ = hostNet.Accept(ctx, hostConn) }()
	go func() { _ = guestNet.AcceptStdio(ctx, guestConn) }()

	target := net.JoinHostPort(DefaultGatewayIP, strconv.Itoa(int(DefaultProxyPort)))
	dialCtx, dialCancel := context.WithTimeout(ctx, proxyDialTimeout)
	defer dialCancel()
	conn, err := guestNet.DialContextTCP(dialCtx, target)
	if err != nil {
		t.Fatalf("guest dial %s: %v", target, err)
	}
	defer func() { _ = conn.Close() }()

	if derr := conn.SetDeadline(time.Now().Add(proxyDialTimeout)); derr != nil {
		t.Fatalf("set deadline: %v", derr)
	}
	req, _ := http.NewRequestWithContext(dialCtx, http.MethodGet, "http://"+target+"/", http.NoBody)
	if werr := req.Write(conn); werr != nil {
		t.Fatalf("write request: %v", werr)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != wantBody {
		t.Errorf("body = %q, want %q", body, wantBody)
	}
}

// TestStartProxyServerListenError pins the listen-failure branch:
// binding the proxy a second time on the same gateway port fails, and
// startProxyServer surfaces the error rather than leaking a half-wired
// server.
func TestStartProxyServerListenError(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	cfg := Config{
		Subnet:       DefaultSubnet,
		GatewayIP:    DefaultGatewayIP,
		GatewayMAC:   DefaultGatewayMAC,
		MTU:          DefaultMTU,
		ProxyHandler: handler,
		ProxyPort:    DefaultProxyPort,
	}
	endpoint, err := tap.NewLinkEndpoint(false, DefaultMTU, cfg.GatewayMAC, cfg.GatewayIP, nil)
	if err != nil {
		t.Fatalf("tap endpoint: %v", err)
	}
	s, err := createNetStack(cfg, endpoint)
	if err != nil {
		t.Fatalf("createNetStack: %v", err)
	}
	if perr := startProxyServer(cfg, s); perr != nil {
		t.Fatalf("first startProxyServer: %v", perr)
	}
	if perr := startProxyServer(cfg, s); perr == nil {
		t.Error("second startProxyServer on same port: want error, got nil")
	}
}

// TestStartProxyServerNilHandlerNoop pins that a nil ProxyHandler
// leaves the proxy off (the pre-J6 separate-process path) without
// binding a listener.
func TestStartProxyServerNilHandlerNoop(t *testing.T) {
	cfg := Config{
		Subnet:     DefaultSubnet,
		GatewayIP:  DefaultGatewayIP,
		GatewayMAC: DefaultGatewayMAC,
		MTU:        DefaultMTU,
	}
	endpoint, err := tap.NewLinkEndpoint(false, DefaultMTU, cfg.GatewayMAC, cfg.GatewayIP, nil)
	if err != nil {
		t.Fatalf("tap endpoint: %v", err)
	}
	s, err := createNetStack(cfg, endpoint)
	if err != nil {
		t.Fatalf("createNetStack: %v", err)
	}
	if perr := startProxyServer(cfg, s); perr != nil {
		t.Errorf("startProxyServer(nil handler): %v", perr)
	}
}
