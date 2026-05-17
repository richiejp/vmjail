package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/trace"
)

// AttachPortCmdlineKey is the kernel cmdline parameter that asks the
// agent to vsock-listen on this port for incoming attach sessions
// from the sandboxd host. Format: `daft.attach_port=<port>`. Absent
// token disables the attach listener (the agent runs hello-only).
//
// Mirrors PortForwardCmdlineKey in shape so the host's cmdline
// builder stays uniform.
const AttachPortCmdlineKey = "daft.attach_port"

// FormatAttachPortCmdline renders an attach-port to the wire form.
// Used by the host to add `daft.attach_port=N` to the cmdline.
func FormatAttachPortCmdline(port uint32) string {
	return fmt.Sprintf("%s=%d", AttachPortCmdlineKey, port)
}

// ParseAttachPortCmdline returns the parsed attach port plus
// ok=true when the token is present. Absent ⇒ ("",false,nil) is
// the "no attach listener" path. Malformed (non-numeric, zero) is
// an error so a typo on the host surfaces.
func ParseAttachPortCmdline(cmdline string) (port uint32, ok bool, err error) {
	return parseNonZeroUintCmdline(cmdline, AttachPortCmdlineKey)
}

// attachReportPrefix is the console prefix for attach-listener
// status lines. Same shape as portForwardReportPrefix.
const attachReportPrefix = "attach"

// attachMaxFrame caps a single framed AgentAttachFrame at 4 MiB.
// Same ceiling as trace.streamMaxMessage. Big enough for typical
// PTY chunks (rarely > 64 KiB) but bounded so a malformed length
// prefix can't make the agent allocate gigabytes.
const attachMaxFrame = 4 * 1024 * 1024

// attachLenPrefix is the fixed 4-byte big-endian length prefix
// before each marshaled AgentAttachFrame / AgentAttachHello.
const attachLenPrefix = 4

// attachCopyBufSize is the buffer size for PTY ↔ frame copying.
// 32 KiB matches the attachCopyBufSize on the orchestrator side.
const attachCopyBufSize = 32 * 1024

// AttachProcess is the live-process view the attach listener pumps
// frames against. The ExecAttach implementation owns the underlying
// PTY master / stdio pair; the listener treats it as a single
// byte-stream plus a side-channel for resize.
type AttachProcess interface {
	io.ReadWriteCloser
	// Resize forwards a window-resize event to the process. Called
	// only when the session was started with TTY=true; a nil error
	// is the success path. Honouring or ignoring the resize is up
	// to the implementation.
	Resize(cols, rows uint32) error
	// Wait blocks until the process exits and returns its exit code.
	// Called by the listener exactly once, after both pump
	// goroutines have returned.
	Wait() (int32, error)
}

// ExecAttach starts an attach process per Hello. The production
// impl spawns `crun exec -t <id> <cmd...>` against the running
// container; tests inject a fake that wraps an in-process io.Pipe
// pair so the listener can be exercised without a real container.
type ExecAttach interface {
	Start(ctx context.Context, req AttachExecRequest) (AttachProcess, error)
}

// AttachExecRequest is the agent-internal exec request derived from
// AgentAttachHello. Same shape but wire-free so the ExecAttach
// implementation never sees a proto type.
type AttachExecRequest struct {
	Command []string
	TTY     bool
	Cols    uint32
	Rows    uint32
}

// attachDeps factors the side-effect surfaces of the attach
// listener so tests can swap in fakes. Production wires the real
// AF_VSOCK listener + ExecAttach implementation.
type attachDeps struct {
	listenVsock func(port uint32) (net.Listener, error)
	exec        ExecAttach
}

// ErrAttachExecUnavailable is the error unavailableAttachExec
// returns on non-Linux builds (see attach_exec_other.go). It
// surfaces to the attaching client as the AgentAttachExit error
// string so the failure is visible end-to-end.
var ErrAttachExecUnavailable = errors.New("agent: attach exec unsupported off Linux")

type unavailableAttachExec struct{}

func (unavailableAttachExec) Start(context.Context, AttachExecRequest) (AttachProcess, error) {
	return nil, ErrAttachExecUnavailable
}

// startAttachListenerFromCmdline parses the cmdline, reports
// errors to the agent console, and kicks off runAttachListener on
// a detached goroutine. Like startPortForwardersFromCmdline it
// must not block — the listener lives for the agent's lifetime.
func startAttachListenerFromCmdline(ctx context.Context, w io.Writer, d runDeps, cmdline string) {
	port, ok, err := ParseAttachPortCmdline(cmdline)
	if err != nil {
		writeReport(w, attachReportPrefix, "cmdline: "+err.Error())
		return
	}
	if !ok {
		return
	}
	go runAttachListener(ctx, w, port, d.attach)
}

// runAttachListener vsock-listens on port and serves each accepted
// connection via handleAttach. Listener-bind errors are reported
// on the console and the goroutine exits — there's nothing
// useful to do otherwise. ctx.Done triggers a Close on the
// listener so Accept returns net.ErrClosed.
func runAttachListener(ctx context.Context, w io.Writer, port uint32, d attachDeps) {
	_, span := trace.Start(ctx, "agent.attach.listener.start",
		trace.WithAttributes(trace.KV{Key: "vsock_port", Value: strconv.FormatUint(uint64(port), 10)}))
	defer span.End()
	ln, err := d.listenVsock(port)
	if err != nil {
		writeReport(w, attachReportPrefix,
			fmt.Sprintf("listen vsock %d: %v", port, err))
		return
	}
	writeReport(w, attachReportPrefix, fmt.Sprintf("ready vsock %d", port))
	go func() {
		<-ctx.Done()
		oops.Skip("agent: close attach listener", ln.Close(), net.ErrClosed)
	}()
	attachAcceptLoop(ctx, w, ln, d.exec)
}

// attachAcceptLoop accepts connections on ln and spawns
// handleAttach per connection. Returns on ErrClosed (ctx-driven
// listener close) or any non-temporary error.
func attachAcceptLoop(ctx context.Context, w io.Writer, ln net.Listener, exec ExecAttach) {
	//daft:lint:disable=forloopbound reason="bounded by ln.Accept returning ErrClosed when ctx.Done closes the listener"
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			writeReport(w, attachReportPrefix, fmt.Sprintf("accept: %v", err))
			return
		}
		go handleAttach(ctx, w, conn, exec)
	}
}

// handleAttach owns one attach connection end-to-end: reads the
// Hello, starts the process via ExecAttach, splices frames both
// directions, and writes a terminal AgentAttachExit before closing.
//
// All error paths surface to the client as an Exit frame with a
// non-empty error string so the attaching `daft workspace attach`
// session sees the failure rather than a silent EOF.
func handleAttach(ctx context.Context, w io.Writer, conn net.Conn, exec ExecAttach) {
	defer func() { oops.Skip("agent: close attach conn", conn.Close(), net.ErrClosed) }()
	hello, err := ReadAttachHello(conn)
	if err != nil {
		writeReport(w, attachReportPrefix, "read hello: "+err.Error())
		writeAttachExit(conn, 1, "agent: read hello: "+err.Error())
		return
	}
	req := AttachExecRequest{
		Command: hello.GetCommand(),
		TTY:     hello.GetTty(),
		Cols:    hello.GetCols(),
		Rows:    hello.GetRows(),
	}
	pumpAttachProcess(ctx, w, conn, exec, req)
}

// pumpAttachProcess runs the started process's pumps, then writes
// the terminal Exit frame on the still-open conn before returning
// (handleAttach's defer closes the conn). The ordering matters:
// closing the conn first would drop the Exit frame, leaving the
// client to guess at exit status.
func pumpAttachProcess(ctx context.Context, w io.Writer, conn net.Conn, exec ExecAttach, req AttachExecRequest) {
	proc, err := exec.Start(ctx, req)
	if err != nil {
		writeReport(w, attachReportPrefix, "exec start: "+err.Error())
		writeAttachExit(conn, 1, "agent: exec start: "+err.Error())
		return
	}
	defer func() { oops.Skip("agent: close attach process", proc.Close(), net.ErrClosed, io.ErrClosedPipe) }()

	// pumpAttachConnToProcess (client → proc) exits when the client
	// closes the conn or sends an Exit frame. On exit it closes
	// proc so the other pump's blocked proc.Read returns EOF.
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		pumpAttachConnToProcess(w, conn, proc)
		oops.Skip("agent: close attach process (client pump exit)", proc.Close(), net.ErrClosed, io.ErrClosedPipe)
	}()

	// pumpAttachProcessToConn (proc → client) exits when proc EOFs,
	// which is either a natural process exit or the client pump
	// having closed proc above.
	pumpAttachProcessToConn(w, conn, proc)

	// Process side has finished. Write the Exit frame *before*
	// the handleAttach defer closes the conn so the client sees it.
	code, werr := proc.Wait()
	if werr != nil {
		writeReport(w, attachReportPrefix, "exec wait: "+werr.Error())
		writeAttachExit(conn, 1, "agent: exec wait: "+werr.Error())
	} else {
		writeAttachExit(conn, code, "")
	}

	// Closing the conn here unblocks pumpAttachConnToProcess if
	// it's still reading (the process exited but the client hasn't
	// hung up yet). handleAttach's defer would do this too — we
	// just want it to happen before we return so wg-style waiters
	// (if any) don't outlive us.
	oops.Skip("agent: close attach conn (post-exit)", conn.Close(), net.ErrClosed)
	<-clientDone
}

// pumpAttachConnToProcess reads frames off the conn and dispatches:
// Data → proc.Write, Resize → proc.Resize, Hello → error (already
// consumed once). Returns when the conn EOFs or a write to proc
// fails.
func pumpAttachConnToProcess(w io.Writer, conn net.Conn, proc AttachProcess) {
	//daft:lint:disable=forloopbound reason="bounded by ReadFrame returning io.EOF / net.ErrClosed when the conn closes"
	for {
		frame, err := readAttachFrame(conn)
		if err != nil {
			if !oops.IsExpectedPumpExit(err) {
				writeReport(w, attachReportPrefix, "read frame: "+err.Error())
			}
			return
		}
		switch body := frame.GetBody().(type) {
		case *agentv1.AgentAttachFrame_Data:
			if _, werr := proc.Write(body.Data); werr != nil {
				if !oops.IsExpectedPumpExit(werr) {
					writeReport(w, attachReportPrefix, "proc write: "+werr.Error())
				}
				return
			}
		case *agentv1.AgentAttachFrame_Resize:
			if rerr := proc.Resize(body.Resize.GetCols(), body.Resize.GetRows()); rerr != nil {
				writeReport(w, attachReportPrefix, "resize: "+rerr.Error())
				// Resize errors are not fatal — keep pumping data.
			}
		case *agentv1.AgentAttachFrame_Exit:
			// Client-sent Exit is meaningless from the agent's
			// side; treat as graceful EOF.
			return
		default:
			writeReport(w, attachReportPrefix, "unexpected frame body type")
			return
		}
	}
}

// pumpAttachProcessToConn reads proc's output and wraps each chunk
// in a Data frame on conn. Returns when proc EOFs (the process
// exited) or a frame write fails.
func pumpAttachProcessToConn(w io.Writer, conn net.Conn, proc AttachProcess) {
	buf := make([]byte, attachCopyBufSize)
	//daft:lint:disable=forloopbound reason="bounded by proc.Read returning io.EOF when the process exits"
	for {
		n, err := proc.Read(buf)
		if n > 0 {
			if werr := writeAttachData(conn, buf[:n]); werr != nil {
				if !oops.IsExpectedPumpExit(werr) {
					writeReport(w, attachReportPrefix, "write data frame: "+werr.Error())
				}
				return
			}
		}
		if err != nil {
			if !oops.IsExpectedPumpExit(err) {
				writeReport(w, attachReportPrefix, "proc read: "+err.Error())
			}
			return
		}
	}
}

// ReadAttachHello reads exactly one length-prefixed
// AgentAttachHello off r. Any other frame type is a protocol
// error. Exported as the symmetric counterpart of WriteAttachHello
// so test fakes / integration harnesses don't open-code the
// length-prefix decoder.
func ReadAttachHello(r io.Reader) (*agentv1.AgentAttachHello, error) {
	payload, err := readAttachPayload(r)
	if err != nil {
		return nil, err
	}
	var hello agentv1.AgentAttachHello
	if err := proto.Unmarshal(payload, &hello); err != nil {
		return nil, fmt.Errorf("unmarshal hello: %w", err)
	}
	return &hello, nil
}

// readAttachFrame reads one length-prefixed AgentAttachFrame off r.
func readAttachFrame(r io.Reader) (*agentv1.AgentAttachFrame, error) {
	payload, err := readAttachPayload(r)
	if err != nil {
		return nil, err
	}
	var frame agentv1.AgentAttachFrame
	if err := proto.Unmarshal(payload, &frame); err != nil {
		return nil, fmt.Errorf("unmarshal frame: %w", err)
	}
	return &frame, nil
}

// readAttachPayload reads a length-prefixed payload from r. Returns
// the payload bytes without the prefix; caller unmarshals. An
// empty payload (n=0) is legal — an all-default proto message
// marshals to zero bytes and the listener treats that as a
// valid (if uninformative) Hello.
func readAttachPayload(r io.Reader) ([]byte, error) {
	var hdr [attachLenPrefix]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > attachMaxFrame {
		return nil, fmt.Errorf("agent: attach frame %d > cap %d", n, attachMaxFrame)
	}
	if n == 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// WriteAttachHello marshals + length-prefixes the Hello onto w.
// Exported so the sandboxd-side AttachBridger can use the same
// framing without duplicating the encode loop.
func WriteAttachHello(w io.Writer, hello *agentv1.AgentAttachHello) error {
	payload, err := proto.Marshal(hello)
	if err != nil {
		return fmt.Errorf("marshal hello: %w", err)
	}
	return writeAttachLenPrefixed(w, payload)
}

// WriteAttachFrame marshals + length-prefixes the frame onto w.
// Exported alongside WriteAttachHello for the sandboxd side.
func WriteAttachFrame(w io.Writer, frame *agentv1.AgentAttachFrame) error {
	payload, err := proto.Marshal(frame)
	if err != nil {
		return fmt.Errorf("marshal frame: %w", err)
	}
	return writeAttachLenPrefixed(w, payload)
}

// ReadAttachFrame is the exported counterpart of readAttachFrame
// for the sandboxd-side AttachBridger to consume agent responses.
func ReadAttachFrame(r io.Reader) (*agentv1.AgentAttachFrame, error) {
	return readAttachFrame(r)
}

// writeAttachData wraps b in a Data frame and writes it to w.
func writeAttachData(w io.Writer, b []byte) error {
	frame := &agentv1.AgentAttachFrame{
		Body: &agentv1.AgentAttachFrame_Data{Data: b},
	}
	return WriteAttachFrame(w, frame)
}

// writeAttachExit wraps code+errMsg in an Exit frame and writes it
// to w. Errors writing the Exit are reported via oops.Log because
// the conn is about to close anyway.
func writeAttachExit(w io.Writer, code int32, errMsg string) {
	frame := &agentv1.AgentAttachFrame{
		Body: &agentv1.AgentAttachFrame_Exit{
			Exit: &agentv1.AgentAttachExit{Code: code, Error: errMsg},
		},
	}
	oops.Skip("agent: write attach exit", WriteAttachFrame(w, frame), net.ErrClosed, io.ErrClosedPipe)
}

// writeAttachLenPrefixed writes the 4-byte BE length prefix
// followed by payload. The header is on the stack; the payload is
// the caller's existing buffer. Two separate Writes match the
// trace stream's pattern and avoid an allocation per frame on the
// data hot path. A zero-length payload skips the second Write —
// otherwise a synchronous transport like net.Pipe in tests would
// block on the empty Write waiting for a matching Read.
func writeAttachLenPrefixed(w io.Writer, payload []byte) error {
	if len(payload) > attachMaxFrame {
		return fmt.Errorf("agent: attach payload %d > cap %d", len(payload), attachMaxFrame)
	}
	var hdr [attachLenPrefix]byte
	// len(payload) was just bounds-checked above.
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload))) //nolint:gosec // G115: bounded by attachMaxFrame above
	if _, err := w.Write(hdr[:]); err != nil {
		return fmt.Errorf("write length prefix: %w", err)
	}
	if len(payload) == 0 {
		return nil
	}
	if _, err := w.Write(payload); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	return nil
}
