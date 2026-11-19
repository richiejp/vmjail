// vmjail-iso-fixture is the init process for the nested-ISO test fixture.
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/vishvananda/netlink"
)

const sshBanner = "SSH-2.0-vmjail-iso-fixture\r\n"

var (
	linkByName         = netlink.LinkByName
	parseAddr          = netlink.ParseAddr
	addrReplace        = netlink.AddrReplace
	linkSetUp          = netlink.LinkSetUp
	configureNetworkFn = configureNetwork
	listen             = net.Listen
	serveLoop          = serve
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(out io.Writer) error {
	if err := configureNetworkFn("eth0", "10.0.2.15/24"); err != nil {
		return fmt.Errorf("configure network: %w", err)
	}
	ln, err := listen("tcp", ":22")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if _, err := fmt.Fprintln(out, "vmjail ISO fixture ready on tcp/22"); err != nil {
		_ = ln.Close()
		return fmt.Errorf("write readiness: %w", err)
	}
	return serveLoop(context.Background(), ln)
}

func configureNetwork(name, cidr string) error {
	link, err := linkByName(name)
	if err != nil {
		return err
	}
	addr, err := parseAddr(cidr)
	if err != nil {
		return err
	}
	if err := addrReplace(link, addr); err != nil {
		return err
	}
	return linkSetUp(link)
}

func serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	//vmjail:lint:disable=forloopbound reason="bounded by context cancellation, listener closure, or an accept error"
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil //nolint:nilerr // listener closure is the expected cancellation path.
			}
			return err
		}
		_, _ = io.WriteString(conn, sshBanner)
		_ = conn.Close()
	}
}
