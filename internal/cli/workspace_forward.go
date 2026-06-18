package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/localai-org/darkaifactory/internal/oops"
)

const forwardSubName = "forward"

// forwardCopyBufSize is the io.Copy buffer used on each direction
// of the per-accept tunnel. 32 KiB matches Go's io.Copy default
// and is large enough to bypass bufio.Reader's small-buffer path
// on the remote stream.
const forwardCopyBufSize = 32 * 1024

// workspaceForwardOpts gathers `daft workspace forward` flags.
type workspaceForwardOpts struct {
	orchestrator string
	configDir    string
	bind         string
	// workspace and ports are positional args, captured by RunE.
	workspace     string
	localPort     uint32
	containerPort uint32
}

// newWorkspaceForwardCmd returns the cobra subcommand. The
// positional args are <local>:<container> <workspace>, matching
// the shape of `ssh -L` and `kubectl port-forward`.
func newWorkspaceForwardCmd() *cobra.Command {
	var opts workspaceForwardOpts
	cmd := &cobra.Command{
		Use:   forwardSubName + " <local>:<container> [workspace]",
		Short: "Forward a local TCP port through the orchestrator into a workspace",
		Long: "Opens a local TCP listener and tunnels each accepted connection\n" +
			"through the orchestrator's ForwardPort RPC into the named\n" +
			"workspace's container at the given port. The workspace may be\n" +
			"omitted when ./.daft/config.json sets a name (or to fall back to\n" +
			"basename of the cwd). Closest analog: `kubectl port-forward`.\n" +
			"Blocks until Ctrl-C.",
		Args:          cobra.RangeArgs(1, 2),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			local, container, err := parsePortMappingArg(args[0])
			if err != nil {
				return err
			}
			opts.localPort = local
			opts.containerPort = container
			if len(args) == 2 {
				opts.workspace = args[1]
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(),
				os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return runWorkspaceForward(ctx, cmd.OutOrStdout(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.orchestrator, "orchestrator", "",
		"orchestrator URL (https://host:port)")
	cmd.Flags().StringVar(&opts.bind, "bind", "127.0.0.1",
		"interface to bind the local listener on (127.0.0.1 for localhost-only)")
	cmd.Flags().StringVar(&opts.configDir, configDirFlag, "",
		"override the daft client config dir (default: $XDG_CONFIG_HOME/daft/client)")
	return cmd
}

// parsePortMappingArg splits "8080:80" into (local, container).
// Rejects zero ports, ports > 65535, missing colon, or extra
// colons — the CLI's first-line validation before any RPC work.
func parsePortMappingArg(s string) (local, container uint32, err error) {
	localStr, containerStr, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, fmt.Errorf("daft workspace forward: expected <local>:<container>, got %q", s)
	}
	if strings.Contains(containerStr, ":") {
		return 0, 0, fmt.Errorf("daft workspace forward: too many ':' in %q", s)
	}
	local, err = parsePortValue(localStr, "local")
	if err != nil {
		return 0, 0, err
	}
	container, err = parsePortValue(containerStr, "container")
	if err != nil {
		return 0, 0, err
	}
	return local, container, nil
}

// parsePortValue is the per-side port validator: non-empty,
// numeric, in [1, 65535].
func parsePortValue(s, side string) (uint32, error) {
	if s == "" {
		return 0, fmt.Errorf("daft workspace forward: empty %s port", side)
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("daft workspace forward: bad %s port %q: %w", side, s, err)
	}
	if n == 0 {
		return 0, fmt.Errorf("daft workspace forward: %s port must be non-zero", side)
	}
	if n > 65535 {
		return 0, fmt.Errorf("daft workspace forward: %s port %d out of range", side, n)
	}
	return uint32(n), nil
}

// forwardDeps factors the side effects runWorkspaceForward needs
// (orchestrator client, local listener, per-tunnel stream open)
// so tests can inject in-process equivalents.
type forwardDeps struct {
	dialOrchestrator func(configDirOverride, orchestrator string) (workspacev1connect.WorkspaceCatalogServiceClient, error)
	listenLocal      func(addr string) (net.Listener, error)
	// openStream is invoked per accepted conn to obtain the
	// per-tunnel bidi stream. Default wraps client.ForwardPort;
	// tests inject a stub that returns a fakeForwardClientStream.
	openStream func(ctx context.Context, client workspacev1connect.WorkspaceCatalogServiceClient) forwardClientStream
}

func defaultForwardDeps() forwardDeps {
	return forwardDeps{
		dialOrchestrator: dialWorkspaceCatalogForForward,
		listenLocal:      func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) },
		openStream: func(ctx context.Context, client workspacev1connect.WorkspaceCatalogServiceClient) forwardClientStream {
			return client.ForwardPort(ctx)
		},
	}
}

// dialWorkspaceCatalogForForward wraps dialWorkspaceCatalog with
// a fresh tracer scoped to the forward command — forward doesn't
// ship per-tunnel spans (each accepted conn would otherwise flood
// the trace channel) so we attach a discardable tracer. The
// returned *http.Client is unused outside the client itself.
func dialWorkspaceCatalogForForward(configDirOverride, orchestrator string) (workspacev1connect.WorkspaceCatalogServiceClient, error) {
	tracer, _ := tracerForCommand(context.Background())
	client, _, err := dialWorkspaceCatalog(configDirOverride, orchestrator, tracer)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func runWorkspaceForward(ctx context.Context, stdout io.Writer, opts workspaceForwardOpts) error {
	return runWorkspaceForwardWith(ctx, stdout, opts, defaultForwardDeps())
}

func runWorkspaceForwardWith(
	ctx context.Context, stdout io.Writer,
	opts workspaceForwardOpts, deps forwardDeps,
) error {
	if opts.workspace == "" {
		name, err := resolveWorkspaceNameDefault()
		if err != nil {
			return err
		}
		opts.workspace = name
	}
	orchestrator, err := resolveOrchestrator(opts.orchestrator, opts.configDir)
	if err != nil {
		return err
	}
	opts.orchestrator = orchestrator
	client, err := deps.dialOrchestrator(opts.configDir, opts.orchestrator)
	if err != nil {
		return err
	}
	return forwardOnePort(ctx, stdout, client, opts, deps)
}

// forwardOnePort opens the local listener for opts.localPort and tunnels
// every accepted connection into opts.workspace's container at
// opts.containerPort, blocking until ctx is cancelled (the listener-close
// watchdog races Accept). The orchestrator client is passed in so a caller
// forwarding several ports (daft workspace up) dials once and runs one of
// these per port concurrently.
func forwardOnePort(
	ctx context.Context, stdout io.Writer,
	client workspacev1connect.WorkspaceCatalogServiceClient,
	opts workspaceForwardOpts, deps forwardDeps,
) error {
	addr := fmt.Sprintf("%s:%d", opts.bind, opts.localPort)
	ln, err := deps.listenLocal(addr)
	if err != nil {
		return fmt.Errorf("daft workspace forward: listen %s: %w", addr, err)
	}
	defer func() { oops.Skip("daft workspace forward: close listener", ln.Close(), net.ErrClosed) }()

	_, _ = fmt.Fprintf(stdout,
		"daft workspace forward: %s -> workspace=%s container=%d (Ctrl-C to stop)\n",
		ln.Addr(), opts.workspace, opts.containerPort)

	// ctx-driven teardown: closing the listener races Accept,
	// which returns net.ErrClosed and breaks the loop.
	go func() {
		<-ctx.Done()
		oops.Skip("daft workspace forward: ctx-close listener", ln.Close(), net.ErrClosed)
	}()
	return acceptForwardLoop(ctx, stdout, ln, client, opts, deps)
}

// acceptForwardLoop accepts inbound conns and spawns a tunnel
// goroutine for each. Returns nil for the ctx-driven listener
// close path; the first non-ErrClosed accept error otherwise.
// Per-tunnel errors are logged on stdout and don't tear the
// loop down — one misbehaving client mustn't kill the listener.
func acceptForwardLoop(
	ctx context.Context, stdout io.Writer, ln net.Listener,
	client workspacev1connect.WorkspaceCatalogServiceClient,
	opts workspaceForwardOpts,
	deps forwardDeps,
) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	//daft:lint:disable=forloopbound reason="bounded by ln.Accept returning ErrClosed when ctx.Done closes the listener (watchdog goroutine above)"
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("daft workspace forward: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runForwardTunnel(ctx, conn, client, opts, deps); err != nil {
				_, _ = fmt.Fprintf(stdout, "daft workspace forward: tunnel: %v\n", err)
			}
		}()
	}
}

// runForwardTunnel opens a fresh ForwardPort stream via the deps
// factory, sends Hello, then pumps bytes between conn and the
// stream in both directions. Returns when either side closes or
// errors.
func runForwardTunnel(
	ctx context.Context, conn net.Conn,
	client workspacev1connect.WorkspaceCatalogServiceClient,
	opts workspaceForwardOpts,
	deps forwardDeps,
) error {
	defer func() { oops.Skip("daft workspace forward: close local conn", conn.Close(), net.ErrClosed) }()
	stream := deps.openStream(ctx, client)
	if stream == nil {
		return errors.New("daft workspace forward: nil stream from factory")
	}
	defer func() {
		oops.Skip("daft workspace forward: close stream recv", stream.CloseResponse(), io.EOF)
	}()
	if err := stream.Send(&workspacev1.ForwardFrame{
		Body: &workspacev1.ForwardFrame_Hello{
			Hello: &workspacev1.ForwardHello{
				Workspace:     &workspacev1.WorkspaceHandle{Handle: &workspacev1.WorkspaceHandle_Name{Name: opts.workspace}},
				ContainerPort: opts.containerPort,
			},
		},
	}); err != nil {
		return fmt.Errorf("send Hello: %w", err)
	}
	return pumpForwardConn(conn, stream)
}

// forwardClientStream narrows the connect bidi stream surface to
// what pumpForwardConn uses. Lets tests drop in an in-process
// stub without the full *connect.BidiStreamForClient. CloseResponse
// is on the interface so the pump's cross-cancellation can unblock
// a Receive that's parked on a stream the orchestrator never
// finished writing to.
type forwardClientStream interface {
	Send(*workspacev1.ForwardFrame) error
	Receive() (*workspacev1.ForwardFrame, error)
	CloseResponse() error
}

// Compile-time check that the connect-generated client stream
// satisfies the narrow interface.
var _ forwardClientStream = (*connect.BidiStreamForClient[workspacev1.ForwardFrame, workspacev1.ForwardFrame])(nil)

// pumpForwardConn runs the two-directional io-copy between the
// local TCP conn and the bidi stream. Returns the first non-EOF
// error from either direction.
//
// Cross-cancellation: when either direction exits, a watcher
// goroutine closes the other half (conn.Close + stream.CloseResponse)
// so a Receive() or Read() that's parked never gets to outlive its
// partner. Without this, a Receive blocked on an empty stream
// would hang forever even after the local TCP side has already
// closed.
func pumpForwardConn(conn net.Conn, stream forwardClientStream) error {
	errCh := make(chan error, 2)
	done := make(chan struct{})
	var doneOnce sync.Once
	signalDone := func() { doneOnce.Do(func() { close(done) }) }

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer signalDone()
		errCh <- pumpLocalToStream(conn, stream)
	}()
	go func() {
		defer wg.Done()
		defer signalDone()
		errCh <- pumpStreamToLocal(conn, stream)
	}()

	// Watcher: tear both halves down on the first goroutine exit
	// so the other one isn't parked on a half that will never
	// produce again.
	go func() {
		<-done
		oops.Skip("daft workspace forward: close conn on teardown", conn.Close(), net.ErrClosed)
		oops.Skip("daft workspace forward: close stream recv", stream.CloseResponse(), io.EOF)
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

// pumpLocalToStream reads from conn and sends Data frames on the
// stream until either side closes or errors. The per-Read
// append-copy is required because protobuf marshals []byte fields
// by reference and io.Copy reuses the same buffer across Reads —
// the next Read would overwrite the bytes the in-flight Send is
// still reading.
func pumpLocalToStream(conn net.Conn, stream forwardClientStream) error {
	buf := make([]byte, forwardCopyBufSize)
	//daft:lint:disable=forloopbound reason="exits when conn.Read returns err (peer close / cross-cancel close)"
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&workspacev1.ForwardFrame{
				Body: &workspacev1.ForwardFrame_Data{Data: append([]byte(nil), buf[:n]...)},
			}); sendErr != nil {
				return sendErr
			}
		}
		if err != nil {
			if oops.IsExpectedPumpExit(err) {
				return nil
			}
			return err
		}
	}
}

// pumpStreamToLocal reads Data frames from the stream and writes
// them to conn until either side closes or errors.
func pumpStreamToLocal(conn net.Conn, stream forwardClientStream) error {
	//daft:lint:disable=forloopbound reason="exits when stream.Receive returns err (EOF / cross-cancel close)"
	for {
		msg, err := stream.Receive()
		if err != nil {
			if oops.IsExpectedPumpExit(err) {
				return nil
			}
			return err
		}
		data := msg.GetData()
		if len(data) == 0 {
			continue
		}
		if _, werr := conn.Write(data); werr != nil {
			if oops.IsExpectedPumpExit(werr) {
				return nil
			}
			return werr
		}
	}
}
