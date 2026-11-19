package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestServeWritesSSHBanner(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	got, err := bufio.NewReader(conn).ReadString('\n')
	_ = conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got != sshBanner {
		t.Fatalf("banner = %q, want %q", got, sshBanner)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve shutdown: %v", err)
	}
}

func TestConfigureNetwork(t *testing.T) {
	savedLink, savedParse := linkByName, parseAddr
	savedReplace, savedUp := addrReplace, linkSetUp
	t.Cleanup(func() {
		linkByName, parseAddr = savedLink, savedParse
		addrReplace, linkSetUp = savedReplace, savedUp
	})
	want := errors.New("failed")
	link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "eth0"}}
	linkByName = func(string) (netlink.Link, error) { return nil, want }
	if !errors.Is(configureNetwork("eth0", "10.0.2.15/24"), want) {
		t.Fatal("link lookup error not returned")
	}
	linkByName = func(string) (netlink.Link, error) { return link, nil }
	if configureNetwork("eth0", "bad") == nil {
		t.Fatal("invalid CIDR accepted")
	}
	parseAddr = savedParse
	addrReplace = func(netlink.Link, *netlink.Addr) error { return want }
	if !errors.Is(configureNetwork("eth0", "10.0.2.15/24"), want) {
		t.Fatal("address error not returned")
	}
	addrReplace = func(netlink.Link, *netlink.Addr) error { return nil }
	linkSetUp = func(netlink.Link) error { return want }
	if !errors.Is(configureNetwork("eth0", "10.0.2.15/24"), want) {
		t.Fatal("link-up error not returned")
	}
	linkSetUp = func(netlink.Link) error { return nil }
	if err := configureNetwork("eth0", "10.0.2.15/24"); err != nil {
		t.Fatalf("configureNetwork: %v", err)
	}
}

func TestRun(t *testing.T) {
	savedConfigure, savedListen, savedServe := configureNetworkFn, listen, serveLoop
	t.Cleanup(func() {
		configureNetworkFn, listen, serveLoop = savedConfigure, savedListen, savedServe
	})
	want := errors.New("failed")
	configureNetworkFn = func(string, string) error { return want }
	if err := run(&bytes.Buffer{}); !errors.Is(err, want) {
		t.Fatalf("configure error = %v", err)
	}
	configureNetworkFn = func(string, string) error { return nil }
	listen = func(string, string) (net.Listener, error) { return nil, want }
	if err := run(&bytes.Buffer{}); !errors.Is(err, want) {
		t.Fatalf("listen error = %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen = func(string, string) (net.Listener, error) { return ln, nil }
	serveLoop = func(context.Context, net.Listener) error { return want }
	var out bytes.Buffer
	if err := run(&out); !errors.Is(err, want) {
		t.Fatalf("serve error = %v", err)
	}
	if !strings.Contains(out.String(), "ready on tcp/22") {
		t.Fatalf("output = %q", out.String())
	}
}
