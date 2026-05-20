//go:build linux

package agent

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"unsafe"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// tapIfReq is the layout the TUNSETIFF ioctl expects. We don't need
// the full struct ifreq union — Name + Flags is enough; the kernel
// ignores trailing bytes for this op. Padded to the ifreq footprint
// (40 bytes) so the kernel side never reads off the end of our local.
type tapIfReq struct {
	Name  [unix.IFNAMSIZ]byte
	Flags uint16
	_pad  [22]byte
}

// tapLinuxDeps factors every syscall and netlink call openTapLinux /
// configureTapLinux make, so unit tests can substitute fakes that
// don't need CAP_NET_ADMIN. Production wires defaultTapLinuxDeps.
//
// Each function is intentionally narrowly-typed (no variadic, no
// interface wrappers) so fakes are one-liners that record args and
// return canned errors.
type tapLinuxDeps struct {
	OpenTun    func() (*os.File, error)
	TUNSETIFF  func(fd uintptr, req *tapIfReq) error
	LinkByName func(name string) (netlink.Link, error)
	LinkSetMTU func(link netlink.Link, mtu int) error
	AddrAdd    func(link netlink.Link, addr *netlink.Addr) error
	LinkSetUp  func(link netlink.Link) error
	RouteAdd   func(route *netlink.Route) error
}

// defaultTapLinuxDeps wires the real syscalls + netlink package
// calls. Each field assignment is a single statement so tests of
// the public openTapLinux / configureTapLinux entry points cover
// every wiring line in one pass.
func defaultTapLinuxDeps() tapLinuxDeps {
	return tapLinuxDeps{
		OpenTun:    openDevNetTun,
		TUNSETIFF:  realTunSetIff,
		LinkByName: netlink.LinkByName,
		LinkSetMTU: netlink.LinkSetMTU,
		AddrAdd:    netlink.AddrAdd,
		LinkSetUp:  netlink.LinkSetUp,
		RouteAdd:   netlink.RouteAdd,
	}
}

func openDevNetTun() (*os.File, error) {
	return os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
}

// realTunSetIff is the only line of unsafe.Pointer in this package
// — every other tap_linux statement is exercised via fakes.
func realTunSetIff(fd uintptr, req *tapIfReq) error {
	//nolint:gosec,forbidigo // TUNSETIFF requires passing a struct address as uintptr; req is the caller's stack-allocated local and the syscall completes before this frame unwinds.
	_, _, errno := unix.Syscall(
		unix.SYS_IOCTL, fd, uintptr(unix.TUNSETIFF), uintptr(unsafe.Pointer(req)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// openTapLinux creates a TAP device. IFF_NO_PI keeps Read/Write at
// one raw Ethernet frame per call — no 4-byte packet-info header
// to strip.
func openTapLinux(name string) (*os.File, string, error) {
	return openTapLinuxWith(name, defaultTapLinuxDeps())
}

func openTapLinuxWith(name string, d tapLinuxDeps) (*os.File, string, error) {
	if len(name) >= unix.IFNAMSIZ {
		return nil, "", fmt.Errorf("agent: tap name %q exceeds IFNAMSIZ", name)
	}
	f, err := d.OpenTun()
	if err != nil {
		return nil, "", fmt.Errorf("agent: open /dev/net/tun: %w", err)
	}
	var req tapIfReq
	copy(req.Name[:], name)
	req.Flags = unix.IFF_TAP | unix.IFF_NO_PI
	if err := d.TUNSETIFF(f.Fd(), &req); err != nil {
		_ = f.Close()
		return nil, "", fmt.Errorf("agent: TUNSETIFF: %w", err)
	}
	actual := string(bytes.TrimRight(req.Name[:], "\x00"))
	return f, actual, nil
}

// configureTapLinux brings up name with mtu, assigns addrCIDR, and
// adds a default route via gateway. All operations go through
// vishvananda/netlink so we don't hand-roll rtnetlink message
// framing.
func configureTapLinux(name, addrCIDR, gateway string, mtu int) error {
	return configureTapLinuxWith(name, addrCIDR, gateway, mtu, defaultTapLinuxDeps())
}

func configureTapLinuxWith(name, addrCIDR, gateway string, mtu int, d tapLinuxDeps) error {
	link, err := d.LinkByName(name)
	if err != nil {
		return fmt.Errorf("agent: LinkByName %q: %w", name, err)
	}
	if err := d.LinkSetMTU(link, mtu); err != nil {
		return fmt.Errorf("agent: LinkSetMTU %s mtu=%d: %w", name, mtu, err)
	}
	addr, err := netlink.ParseAddr(addrCIDR)
	if err != nil {
		return fmt.Errorf("agent: parse addr %q: %w", addrCIDR, err)
	}
	if err := d.AddrAdd(link, addr); err != nil {
		return fmt.Errorf("agent: AddrAdd %s on %s: %w", addrCIDR, name, err)
	}
	if err := d.LinkSetUp(link); err != nil {
		return fmt.Errorf("agent: LinkSetUp %s: %w", name, err)
	}
	gwIP := net.ParseIP(gateway)
	if gwIP == nil {
		return fmt.Errorf("agent: parse gateway %q", gateway)
	}
	if err := d.RouteAdd(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Gw:        gwIP,
	}); err != nil {
		return fmt.Errorf("agent: RouteAdd default via %s: %w", gateway, err)
	}
	return nil
}
