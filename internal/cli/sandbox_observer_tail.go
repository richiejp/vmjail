package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/oops"
)

// observerTailOpts gathers `daft sandbox observer-tail` flags + the
// optional workspace positional.
type observerTailOpts struct {
	orchestrator string
	configDir    string
	follow       bool
	workspace    string
}

func newSandboxObserverTailCmd() *cobra.Command {
	var opts observerTailOpts
	cmd := &cobra.Command{
		Use:   "observer-tail [workspace]",
		Short: "Stream a running sandbox's host-side observer log",
		Long: "Streams the per-sandbox Tier-1 observer trace (newline-delimited\n" +
			"JSON: cpu/memory/io/cgroup/net samples) for a workspace's running\n" +
			"sandbox, via the orchestrator's ObserverTail RPC. With --follow\n" +
			"(default) it stays open and prints new samples as the observer\n" +
			"appends them, like `tail -f`; with --follow=false it prints the\n" +
			"current log once and exits.\n\n" +
			"The workspace may be omitted when ./.daft/config.json sets a name\n" +
			"(or to fall back to basename of the cwd).\n\n" +
			"The observer log is described in OBSERVABILITY.md.",
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.workspace = args[0]
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(),
				os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return runObserverTail(ctx, cmd.OutOrStdout(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.orchestrator, "orchestrator", "",
		"orchestrator URL (https://host:port)")
	cmd.Flags().StringVar(&opts.configDir, configDirFlag, "",
		"override the daft client config dir (default: $XDG_CONFIG_HOME/daft/client)")
	cmd.Flags().BoolVar(&opts.follow, "follow", true,
		"keep streaming appended samples (tail -f); false prints the current log once and exits")
	return cmd
}

// observerClientStream narrows the connect server-stream surface to
// what the tail loop needs. Lets tests drop in an in-process stub.
type observerClientStream interface {
	Receive() bool
	Msg() *workspacev1.ObserverChunk
	Err() error
	Close() error
}

var _ observerClientStream = (*connect.ServerStreamForClient[workspacev1.ObserverChunk])(nil)

// observerTailDeps factors the side-effect surfaces runObserverTail
// needs so tests can swap in in-process stubs.
type observerTailDeps struct {
	dialOrchestrator func(configDirOverride, orchestrator string) (workspacev1connect.WorkspaceCatalogServiceClient, error)
	openStream       func(ctx context.Context, client workspacev1connect.WorkspaceCatalogServiceClient, req *workspacev1.ObserverTailRequest) (observerClientStream, error)
}

func defaultObserverTailDeps() observerTailDeps {
	return observerTailDeps{
		dialOrchestrator: dialWorkspaceCatalogForForward,
		openStream: func(ctx context.Context, client workspacev1connect.WorkspaceCatalogServiceClient, req *workspacev1.ObserverTailRequest) (observerClientStream, error) {
			return client.ObserverTail(ctx, connect.NewRequest(req))
		},
	}
}

func runObserverTail(ctx context.Context, stdout io.Writer, opts observerTailOpts) error {
	return runObserverTailWith(ctx, stdout, opts, defaultObserverTailDeps())
}

func runObserverTailWith(ctx context.Context, stdout io.Writer, opts observerTailOpts, deps observerTailDeps) error {
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
	client, err := deps.dialOrchestrator(opts.configDir, orchestrator)
	if err != nil {
		return err
	}
	req := &workspacev1.ObserverTailRequest{
		Workspace: &workspacev1.WorkspaceHandle{
			Handle: &workspacev1.WorkspaceHandle_Name{Name: opts.workspace},
		},
		Follow: opts.follow,
	}
	stream, err := deps.openStream(ctx, client, req)
	if err != nil {
		return err
	}
	if stream == nil {
		return errors.New("daft sandbox observer-tail: nil stream from factory")
	}
	return pumpObserverTail(stdout, stream)
}

// pumpObserverTail copies received ObserverChunk bytes to stdout until
// the stream ends (dump complete, or the user cancelled a follow). A
// cancelled context surfaces as a stream error that
// oops.IsExpectedPumpExit treats as a clean stop.
func pumpObserverTail(stdout io.Writer, stream observerClientStream) error {
	defer func() { oops.Skip("daft sandbox observer-tail: close stream", stream.Close(), io.EOF) }()
	//daft:lint:disable=forloopbound reason="exits when stream.Receive returns false (EOF / ctx cancel / stream error)"
	for stream.Receive() {
		data := stream.Msg().GetData()
		if len(data) == 0 {
			continue
		}
		if _, err := stdout.Write(data); err != nil {
			return fmt.Errorf("daft sandbox observer-tail: write: %w", err)
		}
	}
	if err := stream.Err(); err != nil && !oops.IsExpectedPumpExit(err) {
		return err
	}
	return nil
}
