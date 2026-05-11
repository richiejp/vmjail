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
var vsockDialer = func(port uint32) (net.Conn, error) {
	return vsock.Dial(vmaddrCIDHost, port, nil)
}

// DialHostVsock dials the host on (VMADDR_CID_HOST, port) and
// returns the connection. Used by the in-VM agent to ship spans
// to the sandbox-host via Cloud Hypervisor's hybrid-vsock proxy
// (which forwards the connection to <HostSocket>_<port> on the
// host's filesystem).
//
// ctx's deadline (if any) is enforced after dialing by setting
// the conn's read+write deadlines; vsock.Dial itself doesn't
// accept a ctx.
func DialHostVsock(ctx context.Context, port uint32) (net.Conn, error) {
	conn, err := vsockDialer(port)
	if err != nil {
		return nil, fmt.Errorf("agent: vsock dial cid=%d port=%d: %w", vmaddrCIDHost, port, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("agent: SetDeadline: %w", err)
		}
	}
	return conn, nil
}
