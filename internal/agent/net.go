package agent

import (
	"context"
	"fmt"
	"io"
	"net"

	"github.com/richiejp/darkaifactory/internal/oops"
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

// NetBridge owns the guest-side conn into sandboxnet. Slice 2a
// holds the conn open and exposes it via Conn() for the slice-2b
// tap rx/tx pumps to plug into.
type NetBridge struct {
	conn net.Conn
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

// Close drops the underlying conn. Safe to call repeatedly.
func (b *NetBridge) Close() error {
	if b == nil || b.conn == nil {
		return nil
	}
	c := b.conn
	b.conn = nil
	if err := c.Close(); err != nil && !oops.IsExpectedPumpExit(err) {
		return err
	}
	return nil
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
// "feature not requested" path) or on any failure — slice 2a is
// best-effort; an unreachable host listener doesn't take down
// the agent.
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
	// Close the bridge when the agent's ctx ends. Without this the
	// vsock conn would outlive the agent and the host-side conn
	// tracker would not unblock until the VM is killed.
	go func() {
		<-ctx.Done()
		oops.Skip("close net bridge", bridge.Close(), net.ErrClosed)
	}()
	return bridge
}
