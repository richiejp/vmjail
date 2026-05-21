package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/localai-org/darkaifactory/internal/oops"
)

// NetPortCmdlineKey is the kernel cmdline parameter that asks
// the agent to dial AF_VSOCK CID 2 port N and pump Ethernet
// frames between that conn and a tap device, so the container
// can reach the sandboxd-side userspace network stack
// (internal/sandboxnet). Format: `daft.net_port=<port>`. Absent
// disables the bridge — the VM keeps loopback only.
const NetPortCmdlineKey = "daft.net_port"

// NetReportPrefix tags console lines about the net bridge —
// cmdline parse errors, dial failures, connection-up notice.
// Same separator convention as the other Report prefixes.
const NetReportPrefix = "net: "

// Guest-side defaults for the tap device the bridge runs over.
// Address is .2 because sandboxnet's gvproxy gateway sits on .1 of
// the same /24. MTU matches sandboxnet.DefaultMTU on the host side
// so frames never need fragmenting on the vsock hop.
const (
	DefaultTapName    = "daft0"
	DefaultGuestIPNet = "192.168.127.2/24"
	DefaultGuestGw    = "192.168.127.1"
	DefaultGuestMTU   = 1500
)

// FormatNetPortCmdline renders a net-port to the wire form.
func FormatNetPortCmdline(port uint32) string {
	return fmt.Sprintf("%s=%d", NetPortCmdlineKey, port)
}

// ParseNetPortCmdline returns the parsed net port plus ok=true
// when the token is present. Absent ⇒ (0, false, nil); malformed
// or zero ⇒ error so a host-side typo surfaces.
func ParseNetPortCmdline(cmdline string) (port uint32, ok bool, err error) {
	return parseNonZeroUintCmdline(cmdline, NetPortCmdlineKey)
}

// VsockDialer is the indirection that lets the agent dial host
// vsock through either the real DialHostVsock or a test fake.
// Same shape as runDeps.dialVsock.
type VsockDialer func(ctx context.Context, port uint32) (net.Conn, error)

// tapDeps factors the two side-effect surfaces the slice-2b tap
// bridge needs so tests can substitute io.Pipe pairs without
// /dev/net/tun and CAP_NET_ADMIN. The "open" half returns the
// raw-Ethernet endpoint and the kernel-assigned interface name;
// "configure" runs the netlink dance (LinkSetMTU + AddrAdd +
// LinkSetUp + RouteAdd) by name so the pump loops never have to
// touch netlink.
type tapDeps struct {
	open      func(name string) (io.ReadWriteCloser, string, error)
	configure func(name, addrCIDR, gateway string, mtu int) error
}

// defaultTapDeps wires the Linux production helpers. On non-linux
// dev builds the underlying functions are stubs that return an
// error — the agent doesn't run there anyway.
func defaultTapDeps() tapDeps {
	return tapDeps{
		open: func(name string) (io.ReadWriteCloser, string, error) {
			f, n, err := openTapLinux(name)
			if err != nil {
				return nil, "", err
			}
			return f, n, nil
		},
		configure: configureTapLinux,
	}
}

// Compile-time guard so the *os.File returned by the linux tap
// helpers continues to satisfy io.ReadWriteCloser — the pump and
// the runDeps surface depend on that contract.
var _ io.ReadWriteCloser = (*os.File)(nil)

// NetBridge owns the guest-side conn into sandboxnet plus, when
// the slice-2b tap path is wired, the tap device the pump shuttles
// raw Ethernet to. Conn-only NetBridges (no tap, no pumps) remain
// valid — that's the shape slice-2a tests assert on.
type NetBridge struct {
	conn net.Conn
	tap  io.ReadWriteCloser
}

// StartNetBridge dials the sandboxd-side net listener and
// returns a NetBridge holding the conn. The caller owns the
// returned bridge's lifetime via Close.
func StartNetBridge(ctx context.Context, dial VsockDialer, port uint32) (*NetBridge, error) {
	conn, err := dial(ctx, port)
	if err != nil {
		return nil, fmt.Errorf("agent: net bridge dial port=%d: %w", port, err)
	}
	return &NetBridge{conn: conn}, nil
}

// Close drops the underlying conn and tap (if attached). Safe to
// call repeatedly — closes are guarded so a teardown goroutine
// racing the pump-driven close can't double-Close the fd.
func (b *NetBridge) Close() error {
	if b == nil {
		return nil
	}
	var firstErr error
	if c := b.conn; c != nil {
		b.conn = nil
		if err := c.Close(); err != nil && !oops.IsExpectedPumpExit(err) {
			firstErr = err
		}
	}
	if t := b.tap; t != nil {
		b.tap = nil
		if err := t.Close(); err != nil && !oops.IsExpectedPumpExit(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Conn returns the underlying conn for the slice-2b tap pumps
// to plug into. Reads block until the host writes a frame; the
// slice-2a callers should not read here.
func (b *NetBridge) Conn() net.Conn {
	return b.conn
}

// Compile-time guard: the slice-2b pumps will treat the conn as
// a byte stream, so net.Conn must remain io.ReadWriteCloser.
var _ io.ReadWriteCloser = net.Conn(nil)

// startNetBridgeFromCmdline parses daft.net_port and, when set,
// dials the sandboxd-side listener. Reports a one-line status on
// w so the host's banner scan sees per-prefix output for this
// channel. Returns nil when the cmdline lacks the key (the
// "feature not requested" path) or on any failure — every layer
// here is best-effort; an unreachable host listener or a missing
// /dev/net/tun doesn't take down the agent.
//
// When d.tap.open is wired (production), this also opens tap0,
// configures L3 (addr/route/MTU) via netlink, and launches the
// rx/tx pumps. When d.tap.open is nil (slice-2a tests), the
// bridge is conn-only and the caller's ctx-cancel goroutine
// owns teardown.
func startNetBridgeFromCmdline(ctx context.Context, w io.Writer, d runDeps, cmdline string) *NetBridge {
	port, ok, err := ParseNetPortCmdline(cmdline)
	switch {
	case err != nil:
		writeReport(w, NetReportPrefix, "cmdline: "+err.Error())
		return nil
	case !ok:
		return nil
	}
	bridge, err := StartNetBridge(ctx, d.dialVsock, port)
	if err != nil {
		writeReport(w, NetReportPrefix, err.Error())
		return nil
	}
	writeReport(w, NetReportPrefix, fmt.Sprintf("connected port=%d", port))

	if d.tap.open != nil {
		if !attachTapToBridge(w, d.tap, bridge) {
			return nil
		}
		go pumpFrames(ctx, bridge.conn, bridge.tap)
		return bridge
	}

	// Conn-only path (slice 2a): close on ctx so the host-side
	// conn tracker unblocks promptly on VM teardown.
	go func() {
		<-ctx.Done()
		oops.Skip("close net bridge", bridge.Close(), net.ErrClosed)
	}()
	return bridge
}

// attachTapToBridge opens tap0 and runs netlink config. On any
// failure it tears down the bridge, reports the error, and
// returns false; success leaves the tap fd attached to bridge.tap
// for pumpFrames to pick up.
func attachTapToBridge(w io.Writer, td tapDeps, bridge *NetBridge) bool {
	tap, name, err := td.open(DefaultTapName)
	if err != nil {
		writeReport(w, NetReportPrefix, err.Error())
		oops.Skip("close net bridge after tap open", bridge.Close(), net.ErrClosed)
		return false
	}
	if err := td.configure(name, DefaultGuestIPNet, DefaultGuestGw, DefaultGuestMTU); err != nil {
		writeReport(w, NetReportPrefix, err.Error())
		oops.Skip("close tap after configure failure", tap.Close(), net.ErrClosed)
		oops.Skip("close net bridge after tap configure", bridge.Close(), net.ErrClosed)
		return false
	}
	bridge.tap = tap
	writeReport(w, NetReportPrefix, fmt.Sprintf("tap up name=%s ip=%s mtu=%d", name, DefaultGuestIPNet, DefaultGuestMTU))
	return true
}
