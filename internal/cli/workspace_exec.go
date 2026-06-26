package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/oops"
)

// execCopyBufSize is the stdin → Data-frame chunk size. 32 KiB
// matches the agent-side attach pump and Go's io.Copy default.
const execCopyBufSize = 32 * 1024

// workspaceExecOpts gathers `daft workspace exec` flags + positionals.
type workspaceExecOpts struct {
	orchestrator string
	configDir    string
	tty          bool
	// workspace is the positional arg; command is everything
	// after `--`.
	workspace string
	command   []string
}

func newWorkspaceExecCmd() *cobra.Command {
	var opts workspaceExecOpts
	cmd := &cobra.Command{
		Use:   execSubName + " [workspace] [-- cmd args...]",
		Short: "Run a command inside a workspace's container with an interactive PTY",
		Long: "Opens a bidi PTY stream to the workspace's running sandbox via\n" +
			"the orchestrator's AttachSandbox RPC. The local terminal goes\n" +
			"into raw mode for the duration of the session; SIGWINCH\n" +
			"events propagate to the in-VM PTY. Without a command after\n" +
			"`--`, the agent runs its default shell (/bin/sh -l).\n\n" +
			"The workspace may be omitted when ./.daft/config.json sets a\n" +
			"name (or to fall back to basename of the cwd); to pass a\n" +
			"command without naming a workspace, lead with `--`, e.g.\n" +
			"`daft workspace exec -- ls`.\n\n" +
			"Closest analog: `kubectl exec -it` / `docker exec -it`.",
		Args:          cobra.ArbitraryArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// ArgsLenAtDash()==0 means the user led with `--`, so every
			// arg is the command and the workspace comes from config.
			if cmd.ArgsLenAtDash() == 0 {
				opts.command = args
			} else if len(args) > 0 {
				opts.workspace = args[0]
				opts.command = args[1:]
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(),
				os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return runWorkspaceExec(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.orchestrator, "orchestrator", "",
		"orchestrator URL (https://host:port)")
	cmd.Flags().StringVar(&opts.configDir, configDirFlag, "",
		"override the daft client config dir (default: $XDG_CONFIG_HOME/daft/client)")
	cmd.Flags().BoolVar(&opts.tty, "tty", true,
		"allocate a PTY (false for piped/non-interactive one-shot commands)")
	return cmd
}

// execClientStream narrows the connect bidi stream surface to
// what runExecSession needs. Lets tests drop in an in-process stub.
type execClientStream interface {
	Send(*workspacev1.AttachFrame) error
	Receive() (*workspacev1.AttachFrame, error)
	CloseRequest() error
	CloseResponse() error
}

var _ execClientStream = (*connect.BidiStreamForClient[workspacev1.AttachFrame, workspacev1.AttachFrame])(nil)

// execTerminal abstracts the local TTY surface. Production wires
// a real termios+SIGWINCH implementation; tests inject a fake.
// IsTTY reports whether the input is a real terminal — when
// false, MakeRaw / Winsize / SIGWINCH are not used and the
// session runs piped (stdin → Data frames, output → stdout, no
// resize traffic).
type execTerminal interface {
	IsTTY() bool
	// Winsize reports the local terminal's window size. Called
	// once on startup (for the Hello) and on each SIGWINCH.
	Winsize() (cols, rows uint32, err error)
	// MakeRaw puts the local terminal into raw mode and returns
	// a restore func the caller defers. No-op when IsTTY()=false.
	MakeRaw() (restore func() error, err error)
	// Resize returns a channel that fires once per SIGWINCH.
	// nil when IsTTY()=false. The channel is closed on ctx done.
	Resize(ctx context.Context) <-chan struct{}
}

// execDeps factors the side-effect surfaces runWorkspaceExec
// needs so tests can swap in in-process stubs.
type execDeps struct {
	dialOrchestrator func(configDirOverride, orchestrator string) (workspacev1connect.WorkspaceCatalogServiceClient, error)
	openStream       func(ctx context.Context, client workspacev1connect.WorkspaceCatalogServiceClient) execClientStream
	stdin            io.Reader
	stdout           io.Writer
	terminal         execTerminal
}

func defaultExecDeps() execDeps {
	return execDeps{
		dialOrchestrator: dialWorkspaceCatalogForForward,
		openStream: func(ctx context.Context, client workspacev1connect.WorkspaceCatalogServiceClient) execClientStream {
			return client.AttachSandbox(ctx)
		},
		stdin:    os.Stdin,
		stdout:   os.Stdout,
		terminal: defaultExecTerminal(),
	}
}

func runWorkspaceExec(ctx context.Context, stdout, stderr io.Writer, opts workspaceExecOpts) error {
	return runWorkspaceExecWith(ctx, stdout, stderr, opts, defaultExecDeps())
}

func runWorkspaceExecWith(
	ctx context.Context, stdout, stderr io.Writer,
	opts workspaceExecOpts, deps execDeps,
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
	stream := deps.openStream(ctx, client)
	if stream == nil {
		return errors.New("daft workspace exec: nil stream from factory")
	}
	defer func() {
		oops.Skip("daft workspace exec: close stream recv", stream.CloseResponse(), io.EOF)
	}()
	return runExecSession(ctx, stdout, stderr, stream, opts, deps)
}

// runExecSession sends the Hello, drives the bidi pump, and
// (when the session was TTY-shaped) toggles the local terminal
// into raw mode + propagates SIGWINCH.
func runExecSession(
	ctx context.Context, stdout, stderr io.Writer,
	stream execClientStream,
	opts workspaceExecOpts, deps execDeps,
) error {
	useTTY := opts.tty && deps.terminal != nil && deps.terminal.IsTTY()
	hello, err := buildExecHello(opts, useTTY, deps.terminal)
	if err != nil {
		return err
	}
	if err := stream.Send(&workspacev1.AttachFrame{
		Body: &workspacev1.AttachFrame_Hello{Hello: hello},
	}); err != nil {
		return fmt.Errorf("daft workspace exec: send Hello: %w", err)
	}
	if useTTY {
		restore, rerr := deps.terminal.MakeRaw()
		if rerr != nil {
			_, _ = fmt.Fprintf(stderr, "daft workspace exec: raw mode failed: %v (continuing in cooked mode)\n", rerr)
		} else {
			defer func() { oops.Skip("daft workspace exec: restore tty", restore()) }()
		}
	}
	return pumpExecSession(ctx, deps.stdin, deps.stdout, stream, useTTY, deps.terminal)
}

// buildExecHello assembles the initial AttachHello. When useTTY
// is true the local terminal's current size is captured for the
// initial dimensions; otherwise cols/rows are zero.
func buildExecHello(opts workspaceExecOpts, useTTY bool, term execTerminal) (*workspacev1.AttachHello, error) {
	hello := &workspacev1.AttachHello{
		Workspace: &workspacev1.WorkspaceHandle{
			Handle: &workspacev1.WorkspaceHandle_Name{Name: opts.workspace},
		},
		Command: opts.command,
		Tty:     useTTY,
	}
	if useTTY {
		cols, rows, err := term.Winsize()
		if err != nil {
			return nil, fmt.Errorf("daft workspace exec: read winsize: %w", err)
		}
		hello.Cols = cols
		hello.Rows = rows
	}
	return hello, nil
}

// pumpExecSession runs three concurrent goroutines: stdin → Data
// frames, Data frames → stdout, and (when useTTY) SIGWINCH →
// Resize frames. The stream pump is the lifecycle owner —
// pumpExecSession returns as soon as it sees the server close
// the response (i.e. the in-VM command has exited). The stdin
// pump is fire-and-forget: for a one-shot non-interactive exec
// the local stdin is a never-EOFing TTY, and Go's runtime can't
// interrupt a blocking Read syscall, so we let the goroutine
// remain parked until the process exits.
func pumpExecSession(
	ctx context.Context,
	stdin io.Reader, stdout io.Writer,
	stream execClientStream, useTTY bool, term execTerminal,
) error {
	done := make(chan struct{})
	var doneOnce sync.Once
	signalDone := func() { doneOnce.Do(func() { close(done) }) }

	go func() {
		<-done
		oops.Skip("daft workspace exec: ctx-close stream recv", stream.CloseResponse(), io.EOF)
	}()

	go func() {
		// On clean EOF (interactive user hit Ctrl-D, or stdin
		// is a finite pipe), signal "no more sends" so the
		// server can react. Other errors fall through — the
		// process is about to exit anyway.
		err := pumpExecStdinToStream(stdin, stream)
		if err == nil || errors.Is(err, io.EOF) {
			oops.Skip("daft workspace exec: close stream request after stdin EOF",
				stream.CloseRequest(), io.EOF)
		}
	}()
	if useTTY && term != nil {
		go pumpExecResize(ctx, term, stream, done)
	}
	err := pumpExecStreamToStdout(stream, stdout)
	signalDone()
	if err != nil && !oops.IsExpectedPumpExit(err) {
		return err
	}
	return nil
}

// pumpExecStdinToStream reads from stdin and wraps each chunk as
// an AttachFrame{Data}. Returns when stdin EOFs or Send fails.
func pumpExecStdinToStream(stdin io.Reader, stream execClientStream) error {
	buf := make([]byte, execCopyBufSize)
	//daft:lint:disable=forloopbound reason="bounded by stdin.Read returning io.EOF when the user-host's stdin closes"
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			if sendErr := stream.Send(&workspacev1.AttachFrame{
				Body: &workspacev1.AttachFrame_Data{Data: append([]byte(nil), buf[:n]...)},
			}); sendErr != nil {
				return sendErr
			}
		}
		if err != nil {
			return err
		}
	}
}

// pumpExecStreamToStdout reads Data frames off the stream and
// writes their payload to stdout. Non-Data frames (the
// orchestrator's hello-less first frame, off-spec server-side
// resize) are dropped silently.
func pumpExecStreamToStdout(stream execClientStream, stdout io.Writer) error {
	//daft:lint:disable=forloopbound reason="bounded by stream.Receive returning io.EOF / connect canceled when the orchestrator closes the stream"
	for {
		frame, err := stream.Receive()
		if err != nil {
			return err
		}
		data := frame.GetData()
		if len(data) == 0 {
			continue
		}
		if _, werr := stdout.Write(data); werr != nil {
			return werr
		}
	}
}

// pumpExecResize listens for SIGWINCH on the channel the terminal
// provides and sends a Resize frame on each. Returns when done
// fires or ctx is cancelled.
func pumpExecResize(ctx context.Context, term execTerminal, stream execClientStream, done <-chan struct{}) {
	resize := term.Resize(ctx)
	if resize == nil {
		return
	}
	//daft:lint:disable=forloopbound reason="bounded by ctx cancellation / done channel close"
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case _, ok := <-resize:
			if !ok {
				return
			}
			cols, rows, err := term.Winsize()
			if err != nil {
				return
			}
			_ = stream.Send(&workspacev1.AttachFrame{
				Body: &workspacev1.AttachFrame_Resize{
					Resize: &workspacev1.AttachResize{Cols: cols, Rows: rows},
				},
			})
		}
	}
}
