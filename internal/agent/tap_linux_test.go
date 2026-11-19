//go:build linux

package agent

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// fakeLink is the minimal netlink.Link the configureTapLinuxWith
// tests need. Index 12 is arbitrary — the RouteAdd fake just pins
// that the right link's index is plumbed into the route.
type fakeLink struct{ attrs netlink.LinkAttrs }

func (f *fakeLink) Attrs() *netlink.LinkAttrs { return &f.attrs }
func (f *fakeLink) Type() string              { return "tap" }

func newFakeLink(index int) *fakeLink {
	return &fakeLink{attrs: netlink.LinkAttrs{Index: index, Name: "vmjail0"}}
}

func TestOpenTapLinuxWithRejectsLongName(t *testing.T) {
	long := strings.Repeat("a", unix.IFNAMSIZ)
	_, _, err := openTapLinuxWith(long, tapLinuxDeps{
		OpenTun: func() (*os.File, error) {
			t.Fatal("OpenTun should not be reached when name is invalid")
			return nil, nil //nolint:nilnil // unreachable
		},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds IFNAMSIZ") {
		t.Fatalf("err = %v, want IFNAMSIZ error", err)
	}
}

func TestOpenTapLinuxWithSurfacesOpenError(t *testing.T) {
	_, _, err := openTapLinuxWith("vmjail0", tapLinuxDeps{
		OpenTun: func() (*os.File, error) { return nil, errors.New("synthetic open") },
	})
	if err == nil || !strings.Contains(err.Error(), "/dev/net/tun") {
		t.Fatalf("err = %v, want wrapped open error", err)
	}
}

func TestOpenTapLinuxWithSurfacesIoctlError(t *testing.T) {
	f, cleanup := tempTunFile(t)
	defer cleanup()
	_, _, err := openTapLinuxWith("vmjail0", tapLinuxDeps{
		OpenTun:   func() (*os.File, error) { return f, nil },
		TUNSETIFF: func(uintptr, *tapIfReq) error { return errors.New("EPERM") },
	})
	if err == nil || !strings.Contains(err.Error(), "TUNSETIFF") {
		t.Fatalf("err = %v, want TUNSETIFF error", err)
	}
}

func TestOpenTapLinuxWithSuccess(t *testing.T) {
	f, cleanup := tempTunFile(t)
	defer cleanup()
	var ioctlReq tapIfReq
	got, name, err := openTapLinuxWith("vmjail0", tapLinuxDeps{
		OpenTun: func() (*os.File, error) { return f, nil },
		TUNSETIFF: func(_ uintptr, req *tapIfReq) error {
			ioctlReq = *req
			// Simulate the kernel rewriting Name to the final
			// device name (it doesn't, for a requested name, but
			// it would for a blank one — we want to cover the
			// trim-trailing-NUL branch either way).
			copy(req.Name[:], "vmjail0\x00")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("openTapLinuxWith: %v", err)
	}
	if name != "vmjail0" {
		t.Fatalf("name = %q, want vmjail0", name)
	}
	if got != f {
		t.Fatalf("returned file = %v, want %v", got, f)
	}
	if ioctlReq.Flags != unix.IFF_TAP|unix.IFF_NO_PI {
		t.Fatalf("ioctl flags = 0x%x, want IFF_TAP|IFF_NO_PI", ioctlReq.Flags)
	}
}

func TestConfigureTapLinuxWithLinkByNameError(t *testing.T) {
	err := configureTapLinuxWith("vmjail0", "192.168.127.2/24", "192.168.127.1", 1500, tapLinuxDeps{
		LinkByName: func(string) (netlink.Link, error) { return nil, errors.New("not found") },
	})
	if err == nil || !strings.Contains(err.Error(), "LinkByName") {
		t.Fatalf("err = %v, want LinkByName error", err)
	}
}

func TestConfigureTapLinuxWithLinkSetMTUError(t *testing.T) {
	link := newFakeLink(7)
	err := configureTapLinuxWith("vmjail0", "192.168.127.2/24", "192.168.127.1", 1500, tapLinuxDeps{
		LinkByName: func(string) (netlink.Link, error) { return link, nil },
		LinkSetMTU: func(netlink.Link, int) error { return errors.New("E2BIG") },
	})
	if err == nil || !strings.Contains(err.Error(), "LinkSetMTU") {
		t.Fatalf("err = %v, want LinkSetMTU error", err)
	}
}

func TestConfigureTapLinuxWithBadAddr(t *testing.T) {
	link := newFakeLink(7)
	err := configureTapLinuxWith("vmjail0", "not-a-cidr", "192.168.127.1", 1500, tapLinuxDeps{
		LinkByName: func(string) (netlink.Link, error) { return link, nil },
		LinkSetMTU: func(netlink.Link, int) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "parse addr") {
		t.Fatalf("err = %v, want parse addr error", err)
	}
}

func TestConfigureTapLinuxWithAddrAddError(t *testing.T) {
	link := newFakeLink(7)
	err := configureTapLinuxWith("vmjail0", "192.168.127.2/24", "192.168.127.1", 1500, tapLinuxDeps{
		LinkByName: func(string) (netlink.Link, error) { return link, nil },
		LinkSetMTU: func(netlink.Link, int) error { return nil },
		AddrAdd:    func(netlink.Link, *netlink.Addr) error { return errors.New("EEXIST") },
	})
	if err == nil || !strings.Contains(err.Error(), "AddrAdd") {
		t.Fatalf("err = %v, want AddrAdd error", err)
	}
}

func TestConfigureTapLinuxWithLinkSetUpError(t *testing.T) {
	link := newFakeLink(7)
	err := configureTapLinuxWith("vmjail0", "192.168.127.2/24", "192.168.127.1", 1500, tapLinuxDeps{
		LinkByName: func(string) (netlink.Link, error) { return link, nil },
		LinkSetMTU: func(netlink.Link, int) error { return nil },
		AddrAdd:    func(netlink.Link, *netlink.Addr) error { return nil },
		LinkSetUp:  func(netlink.Link) error { return errors.New("EPERM") },
	})
	if err == nil || !strings.Contains(err.Error(), "LinkSetUp") {
		t.Fatalf("err = %v, want LinkSetUp error", err)
	}
}

func TestConfigureTapLinuxWithBadGateway(t *testing.T) {
	link := newFakeLink(7)
	err := configureTapLinuxWith("vmjail0", "192.168.127.2/24", "not-an-ip", 1500, tapLinuxDeps{
		LinkByName: func(string) (netlink.Link, error) { return link, nil },
		LinkSetMTU: func(netlink.Link, int) error { return nil },
		AddrAdd:    func(netlink.Link, *netlink.Addr) error { return nil },
		LinkSetUp:  func(netlink.Link) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "parse gateway") {
		t.Fatalf("err = %v, want parse gateway error", err)
	}
}

func TestConfigureTapLinuxWithRouteAddError(t *testing.T) {
	link := newFakeLink(7)
	err := configureTapLinuxWith("vmjail0", "192.168.127.2/24", "192.168.127.1", 1500, tapLinuxDeps{
		LinkByName: func(string) (netlink.Link, error) { return link, nil },
		LinkSetMTU: func(netlink.Link, int) error { return nil },
		AddrAdd:    func(netlink.Link, *netlink.Addr) error { return nil },
		LinkSetUp:  func(netlink.Link) error { return nil },
		RouteAdd: func(r *netlink.Route) error {
			if r.LinkIndex != 7 {
				t.Fatalf("route LinkIndex = %d, want 7", r.LinkIndex)
			}
			return errors.New("ENETUNREACH")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "RouteAdd") {
		t.Fatalf("err = %v, want RouteAdd error", err)
	}
}

func TestConfigureTapLinuxWithSuccess(t *testing.T) {
	link := newFakeLink(7)
	var got struct {
		mtu    int
		addr   string
		linkUp bool
		gw     string
	}
	err := configureTapLinuxWith("vmjail0", "192.168.127.2/24", "192.168.127.1", 1500, tapLinuxDeps{
		LinkByName: func(string) (netlink.Link, error) { return link, nil },
		LinkSetMTU: func(_ netlink.Link, mtu int) error { got.mtu = mtu; return nil },
		AddrAdd: func(_ netlink.Link, a *netlink.Addr) error {
			got.addr = a.IPNet.String()
			return nil
		},
		LinkSetUp: func(netlink.Link) error { got.linkUp = true; return nil },
		RouteAdd: func(r *netlink.Route) error {
			got.gw = r.Gw.String()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("configureTapLinuxWith: %v", err)
	}
	if got.mtu != 1500 || got.addr != "192.168.127.2/24" || !got.linkUp || got.gw != "192.168.127.1" {
		t.Fatalf("got %+v", got)
	}
}

// TestRealTunSetIffSurfacesEnotty pins that the production
// realTunSetIff path is reachable from tests: we call it against a
// regular file fd, which is guaranteed to fail with ENOTTY (or
// similar) since regular files don't implement the ioctl. This
// covers the syscall + errno-nonzero branch without needing root.
func TestRealTunSetIffSurfacesEnotty(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "fake-tun-")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	var req tapIfReq
	if err := realTunSetIff(f.Fd(), &req); err == nil {
		t.Fatal("expected ioctl error against a regular file fd")
	}
}

// TestDefaultTapLinuxDepsWiresRealHelpers pins that
// defaultTapLinuxDeps cascades through the public openTapLinux /
// configureTapLinux entry points so the field-assignment statements
// are covered. configureTapLinux against a nonexistent link is
// guaranteed to fail at LinkByName without requiring root.
func TestDefaultTapLinuxDepsWiresRealHelpers(t *testing.T) {
	if err := configureTapLinux("vmjail-no-such-1c4f0a8e", "192.168.127.2/24", "192.168.127.1", 1500); err == nil {
		t.Fatal("configureTapLinux against nonexistent link: want error")
	}
}

// TestOpenTapLinuxRejectsLongNameThroughPublicEntry pins that the
// public openTapLinux delegation is reachable from tests by hitting
// an error path that doesn't need root (name length is checked
// before any syscall).
func TestOpenTapLinuxRejectsLongNameThroughPublicEntry(t *testing.T) {
	_, _, err := openTapLinux(strings.Repeat("a", unix.IFNAMSIZ))
	if err == nil || !strings.Contains(err.Error(), "exceeds IFNAMSIZ") {
		t.Fatalf("err = %v, want IFNAMSIZ error", err)
	}
}

// TestOpenDevNetTunOpensFile covers the production openDevNetTun
// helper. On every linux dev host /dev/net/tun is present and the
// open is non-privileged — the TUNSETIFF ioctl is what requires
// CAP_NET_ADMIN, not the open itself.
func TestOpenDevNetTunOpensFile(t *testing.T) {
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skipf("/dev/net/tun not present: %v", err)
	}
	f, err := openDevNetTun()
	if err != nil {
		t.Fatalf("openDevNetTun: %v", err)
	}
	_ = f.Close()
}

// TestRealTunSetIffSuccessAgainstDevNetTun pins the errno==0 branch
// of realTunSetIff. /dev/net/tun + a valid TUNSETIFF call needs
// CAP_NET_ADMIN — skip if absent. Without this branch the func
// reports 75% coverage; the per-file gate doesn't need it but the
// total-coverage gate is tight.
func TestRealTunSetIffSuccessAgainstDevNetTun(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("TUNSETIFF needs CAP_NET_ADMIN; running unprivileged")
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		t.Skipf("/dev/net/tun not present: %v", err)
	}
	f, err := openDevNetTun()
	if err != nil {
		t.Fatalf("openDevNetTun: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	var req tapIfReq
	req.Flags = unix.IFF_TAP | unix.IFF_NO_PI
	if err := realTunSetIff(f.Fd(), &req); err != nil {
		t.Fatalf("realTunSetIff: %v", err)
	}
}

// tempTunFile is the stand-in *os.File the openTapLinuxWith tests
// hand to a fake OpenTun. A temp file is fine — none of the tests
// exercise the ioctl path against a real fd.
func tempTunFile(t *testing.T) (f *os.File, cleanup func()) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "fake-tun-")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	return f, func() { _ = f.Close() }
}
