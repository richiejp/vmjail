//go:build linux

package agent

import (
	"context"
	"fmt"
	"net"

	"github.com/mdlayher/vsock"
)

// vmaddrCIDHost is the AF_VSOCK context ID for the hypervisor host
// from the guest's perspective. Defined by the Linux ABI in
// <linux/vm_sockets.h> as VMADDR_CID_HOST = 2. Hard-coded here
// rather than imported because the constant is part of the kernel
// UAPI and won't change.
const vmaddrCIDHost = 2

// vsockDialer is the indirection that lets tests substitute an
// in-process pipe for vsock.Dial. Production wires the real
// vsock.Dial via this var; tests swap it for net.Pipe.
//
//nolint:gochecknoglobals // test-hook indirection over vsock.Dial; mutated only in tests.
var vsockDialer = func(port uint32) (net.Conn, error) {
	return vsock.Dial(vmaddrCIDHost, port, nil)
}

// vsockListener is the indirection that lets tests substitute an
// in-process net.Listener for vsock.Listen. Same shape and
// rationale as vsockDialer above.
//
//nolint:gochecknoglobals // test-hook indirection over vsock.Listen; mutated only in tests.
var vsockListener = func(port uint32) (net.Listener, error) {
	return vsock.Listen(port, nil)
}

// DialHostVsock dials the host on (VMADDR_CID_HOST, port) and
// returns the connection. ctx's deadline (if any) is propagated to
// the returned conn via SetDeadline so subsequent Read/Write honor
// it; vsock.Dial itself doesn't accept a ctx.
func DialHostVsock(ctx context.Context, port uint32) (net.Conn, error) {
	conn, err := vsockDialer(port)
	if err != nil {
		return nil, fmt.Errorf("agent: vsock dial cid=%d port=%d: %w", vmaddrCIDHost, port, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	return conn, nil
}

// vsockListenGuest binds an AF_VSOCK listener on this VM's own CID
// for the given port. The host dials in (via the Cloud Hypervisor
// hybrid vsock CONNECT proxy on its Unix socket) and the agent's
// port-forwarder accepts here. Slice C1.1: only the in-guest pump
// uses this — sandboxd-side dial-into-guest lands in C1.2.
func vsockListenGuest(port uint32) (net.Listener, error) {
	ln, err := vsockListener(port)
	if err != nil {
		return nil, fmt.Errorf("agent: vsock listen port=%d: %w", port, err)
	}
	return ln, nil
}
