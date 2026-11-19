package sandbox

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/richiejp/vmjail/internal/oops"
)

// dialReplyLineLimit bounds the OK/REJECT reply line Cloud
// Hypervisor sends after a CONNECT. The reply is one short ASCII
// line; anything longer is malformed and we refuse it rather than
// stream attacker bytes through a `bufio.Reader.ReadString` that
// would only stop at '\n' or EOF.
const dialReplyLineLimit = 256

// dialOKPrefix is the success prefix CH (and Firecracker, whose
// hybrid-vsock protocol CH inherits) writes on a hybrid-vsock
// CONNECT: `OK <hostside-port>\n`.
const dialOKPrefix = "OK "

// dialConnectFmt is the request line we write to CH's main UDS
// to ask for a tunnel into the given guest-side AF_VSOCK port.
const dialConnectFmt = "CONNECT %d\n"

// DialGuestVsock opens a byte tunnel into the guest by speaking
// Cloud Hypervisor's hybrid-vsock CONNECT protocol against the main
// HostSocket UDS:
//
//  1. Dial the UDS at hostSocket.
//  2. Write `CONNECT <port>\n`.
//  3. Read `OK <hostside-port>\n` on success; anything else means
//     the guest isn't listening on `port` (yet) and we error.
//
// From that point on the conn is a transparent byte tunnel. ctx's
// deadline (if any) is applied to the handshake; the returned conn
// has no deadline so the caller can manage tunnel-stream timeouts
// itself.
//
// Used by the Slice C1 port forwarder: traffic arriving at the
// sandboxd from the orchestrator on a per-port labeled stream is
// bridged into the running VM's agent (which is listening on the
// guest-side AF_VSOCK port written into the kernel cmdline via
// vmjail.port=) through this dialer.
func DialGuestVsock(ctx context.Context, hostSocket string, port uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", hostSocket)
	if err != nil {
		return nil, fmt.Errorf("sandbox: dial %s: %w", hostSocket, err)
	}
	success := false
	defer func() {
		if !success {
			oops.Skip("sandbox: close after CONNECT failure", conn.Close(), net.ErrClosed)
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		oops.Skip("sandbox: set guest-vsock dial deadline",
			conn.SetDeadline(deadline))
	}
	if _, err := fmt.Fprintf(conn, dialConnectFmt, port); err != nil {
		return nil, fmt.Errorf("sandbox: CONNECT %d: %w", port, err)
	}
	br := bufio.NewReaderSize(conn, dialReplyLineLimit)
	line, err := readBoundedLine(br, dialReplyLineLimit)
	if err != nil {
		return nil, fmt.Errorf("sandbox: read CONNECT %d reply: %w", port, err)
	}
	if !strings.HasPrefix(line, dialOKPrefix) {
		return nil, fmt.Errorf("sandbox: CONNECT %d rejected: %q", port, strings.TrimRight(line, "\n"))
	}
	oops.Skip("sandbox: clear guest-vsock dial deadline",
		conn.SetDeadline(time.Time{}))
	success = true
	return &bufferedConn{Conn: conn, r: br}, nil
}

// readBoundedLine reads up to limit bytes searching for '\n'. If
// the line exceeds limit it returns an error rather than
// consuming arbitrary bytes — bufio.Reader.ReadString would
// happily wait for ever otherwise.
func readBoundedLine(br *bufio.Reader, limit int) (string, error) {
	var sb strings.Builder
	//vmjail:lint:disable=forloopbound reason="bounded by limit via the if-break below"
	for {
		b, err := br.ReadByte()
		if err != nil {
			return sb.String(), err
		}
		sb.WriteByte(b)
		if b == '\n' {
			return sb.String(), nil
		}
		if sb.Len() >= limit {
			return sb.String(), fmt.Errorf("reply exceeded %d bytes without newline", limit)
		}
	}
}

// bufferedConn lets the returned Conn keep the bufio buffer alive
// across the handshake/tunnel boundary so any data CH appended
// after the OK line is not dropped on the first user Read.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }
