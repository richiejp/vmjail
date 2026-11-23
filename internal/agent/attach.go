package agent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
	"github.com/richiejp/vmjail/internal/oops"
	"github.com/richiejp/vmjail/internal/trace"
)

// AttachPortCmdlineKey is the kernel cmdline parameter that asks the
// agent to vsock-listen on this port for incoming attach sessions
// from the sandboxd host. Format: `vmjail.attach_port=<port>`. Absent
// token disables the attach listener (the agent runs hello-only).
//
// Mirrors PortForwardCmdlineKey in shape so the host's cmdline
// builder stays uniform.
const AttachPortCmdlineKey = "vmjail.attach_port"

// DefaultAttachVsockPort is the AF_VSOCK port the agent listens on
// for attach sessions when the host opts in. Picked to sit
// between the trace port (8000) and the port-forward base
// (BaseVsockPort = 10000), well clear of CH's reserved
// low-numbered ports. There is no per-sandbox attach port — each
// VM has one agent and one attach listener.
const DefaultAttachVsockPort uint32 = 9000

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
	// Env is the container's environment (read from the bundle's
	// config.json), forwarded so an exec'd process sees the same env
	// as the container's entrypoint — including the Phase-J egress
	// proxy vars. crun exec otherwise starts with a bare env.
	Env []string
	// User is the "uid:gid" the exec'd process runs as, read from the
	// bundle's config.json process.user. Empty means crun's default
	// (root) — so a root container is unchanged, while a non-root image
	// gets exec'd as its declared user instead of silently as root.
	User string
	// Cwd is the container process working directory from config.json.
	Cwd string
	// Shell is an executable shell discovered in the container rootfs.
	// Empty retains the historical /bin/sh fallback.
	Shell string
}

// containerProcess is the per-container exec context read once from the
// bundle's config.json: the env every exec inherits and the user it runs as.
// Bundling them keeps the listener-chain signatures from sprouting a second
// parallel parameter.
type containerProcess struct {
	Env   []string
	User  string
	Cwd   string
	Shell string
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

// ErrContainerNotRunning is the stable client-facing error for attach/job
// requests that race workload shutdown. It deliberately hides crun's internal
// state-file path, which is an implementation detail rather than a diagnosis.
var ErrContainerNotRunning = errors.New("agent: container is not running")

type unavailableAttachExec struct{}

func (unavailableAttachExec) Start(context.Context, AttachExecRequest) (AttachProcess, error) {
	return nil, ErrAttachExecUnavailable
}

// startAttachListener brings up the exec-attach listener when the
// GuestConfig's attach_enabled bit is set, on the fixed
// DefaultAttachVsockPort, and kicks off runAttachListener on a detached
// goroutine. Like startPortForwarders it must not block — the listener
// lives for the agent's lifetime.
func startAttachListener(ctx context.Context, w io.Writer, d runDeps) {
	if !d.cfg.GetAttachEnabled() {
		return
	}
	// The exec'd processes inherit the container's env + user, so the
	// bundle path comes from the same config the container launch uses.
	proc := readContainerProcess(w, d.cfg.GetBundle())
	go runAttachListener(ctx, w, DefaultAttachVsockPort, d.attach, proc)
}

// readContainerProcess reads the container env + user from the bundle's
// config.json so exec'd processes inherit both. Best-effort: a missing or
// malformed config (e.g. the bare hello-bundle, or no bundle at all) reports
// on the console and yields the zero value — exec then runs with crun's
// default env and user (root), the pre-existing behaviour. A root container
// (uid 0, gid 0) also yields an empty User, since crun already defaults to
// root and there's no need to pass --user.
func readContainerProcess(w io.Writer, bundle string) containerProcess {
	if bundle == "" {
		return containerProcess{}
	}
	path := filepath.Join(bundle, "config.json")
	//nolint:gosec // G304: bundle path is kernel-cmdline-supplied in a single-tenant VM.
	raw, err := os.ReadFile(path)
	if err != nil {
		writeReport(w, attachReportPrefix, "read container process: "+err.Error())
		return containerProcess{}
	}
	var spec struct {
		Process struct {
			Env  []string `json:"env"`
			Cwd  string   `json:"cwd"`
			User struct {
				UID int `json:"uid"`
				GID int `json:"gid"`
			} `json:"user"`
		} `json:"process"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		writeReport(w, attachReportPrefix, "parse container process: "+err.Error())
		return containerProcess{}
	}
	user := ""
	if spec.Process.User.UID != 0 || spec.Process.User.GID != 0 {
		user = fmt.Sprintf("%d:%d", spec.Process.User.UID, spec.Process.User.GID)
	}
	return containerProcess{Env: spec.Process.Env, User: user, Cwd: spec.Process.Cwd, Shell: resolveContainerShell(bundle, spec.Process.Env)}
}

func resolveContainerShell(bundle string, env []string) string {
	candidates := make([]string, 0, 6)
	for _, assignment := range env {
		if shell, ok := strings.CutPrefix(assignment, "SHELL="); ok {
			candidates = append(candidates, shell)
			break
		}
	}
	candidates = append(candidates, "/bin/sh", "/bin/bash", "/bin/dash", "/bin/ash", "/busybox/sh")
	rootfs := filepath.Join(bundle, "rootfs")
	for _, candidate := range candidates {
		if !filepath.IsAbs(candidate) || filepath.Clean(candidate) != candidate {
			continue
		}
		info, err := os.Stat(filepath.Join(rootfs, strings.TrimPrefix(candidate, "/")))
		if err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// runAttachListener vsock-listens on port and serves each accepted
// connection via handleAttach. Listener-bind errors are reported
// on the console and the goroutine exits — there's nothing
// useful to do otherwise. ctx.Done triggers a Close on the
// listener so Accept returns net.ErrClosed.
func runAttachListener(ctx context.Context, w io.Writer, port uint32, d attachDeps, proc containerProcess) {
	_, span := trace.Start(ctx, "agent.attach.listener.start",
		trace.WithAttributes(trace.KV{Key: traceAttrVsockPort, Value: strconv.FormatUint(uint64(port), 10)}))
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
	attachAcceptLoop(ctx, w, ln, d.exec, proc)
}

// attachAcceptLoop accepts connections on ln and spawns
// handleAttach per connection. Returns on ErrClosed (ctx-driven
// listener close) or any non-temporary error.
func attachAcceptLoop(ctx context.Context, w io.Writer, ln net.Listener, exec ExecAttach, proc containerProcess) {
	//vmjail:lint:disable=forloopbound reason="bounded by ln.Accept returning ErrClosed when ctx.Done closes the listener"
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			writeReport(w, attachReportPrefix, fmt.Sprintf("accept: %v", err))
			return
		}
		go handleAttach(ctx, w, conn, exec, proc)
	}
}

// handleAttach owns one attach connection end-to-end: reads the
// Hello, starts the process via ExecAttach, splices frames both
// directions, and writes a terminal AgentAttachExit before closing.
//
// All error paths surface to the client as an Exit frame with a
// non-empty error string so the attaching `vmjail workspace attach`
// session sees the failure rather than a silent EOF.
func handleAttach(ctx context.Context, w io.Writer, conn net.Conn, exec ExecAttach, proc containerProcess) {
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
		Env:     proc.Env,
		User:    proc.User,
		Cwd:     proc.Cwd,
		Shell:   proc.Shell,
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
	//vmjail:lint:disable=forloopbound reason="bounded by ReadFrame returning io.EOF / net.ErrClosed when the conn closes"
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
	//vmjail:lint:disable=forloopbound reason="bounded by proc.Read returning io.EOF when the process exits"
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
	if err := writeStreamBytes(w, hdr[:]); err != nil {
		return fmt.Errorf("write length prefix: %w", err)
	}
	if len(payload) == 0 {
		return nil
	}
	if err := writeStreamBytes(w, payload); err != nil {
		return fmt.Errorf("write payload: %w", err)
	}
	return nil
}
