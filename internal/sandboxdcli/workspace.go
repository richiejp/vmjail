package sandboxdcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/richiejp/darkaifactory/internal/cas"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/mtls"
	"github.com/richiejp/darkaifactory/internal/trace"
	"github.com/richiejp/darkaifactory/internal/workspace"
)

const (
	workspaceCmdName = "workspace"
	serveSubName     = "serve"
)

// newWorkspaceCmd returns the `daft-sandboxd workspace` subtree.
// Currently one verb — `serve` — exposes a directory back to the
// user-host for pull-back after a sandbox VM has modified it.
func newWorkspaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   workspaceCmdName,
		Short: "Serve a sandbox-host workspace dir for the user-host to pull back",
	}
	cmd.AddCommand(newWorkspaceServeCmd())
	return cmd
}

func newWorkspaceServeCmd() *cobra.Command {
	var (
		path      string
		name      string
		addr      string
		configDir string
	)
	cmd := &cobra.Command{
		Use:           serveSubName,
		Short:         "Serve a directory as a workspace over mTLS",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(),
				os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return runWorkspaceServe(ctx, cmd.OutOrStdout(),
				path, name, addr, configDir)
		},
	}
	cmd.Flags().StringVar(&path, "path", "",
		"on-disk directory to expose (e.g. a workspace tmpdir kept by --keep-workspace)")
	cmd.Flags().StringVar(&name, "workspace", "",
		"workspace name to expose under")
	cmd.Flags().StringVar(&addr, "addr", "0.0.0.0:9444",
		"TCP listen address (host:port)")
	cmd.Flags().StringVar(&configDir, "config-dir", "", configDirFlagDesc)
	return cmd
}

func runWorkspaceServe(ctx context.Context, stdout io.Writer, path, name, addr, configDirOverride string) error {
	if path == "" {
		return errors.New("daft-sandboxd workspace serve: --path is required")
	}
	if name == "" {
		return errors.New("daft-sandboxd workspace serve: --workspace is required")
	}

	dir, err := resolveSandboxdDir(configDirOverride)
	if err != nil {
		return err
	}
	id, err := loadOrGenerateSandboxd(dir)
	if err != nil {
		return err
	}
	peers, err := openSandboxdPeerStore(dir)
	if err != nil {
		return err
	}
	store, err := cas.Open(filepath.Join(dir, "cas"))
	if err != nil {
		return err
	}

	manifest, err := workspace.BuildManifest(path, store, workspace.BuildOptions{})
	if err != nil {
		return err
	}
	tracer := trace.New("daft-sandboxd")
	wsServer, err := workspace.NewServer(store)
	if err != nil {
		return err
	}
	if err := wsServer.Add(name, manifest); err != nil {
		return err
	}
	mux := http.NewServeMux()
	wsPath, handler := workspacev1connect.NewWorkspaceServiceHandler(wsServer,
		connect.WithInterceptors(tracer.ServerInterceptor()))
	mux.Handle(wsPath, handler)
	srv, err := mtls.New(mtls.Config{
		Identity: id, Peers: peers, Addr: addr, Handler: mux,
	})
	if err != nil {
		return err
	}
	if err := srv.Listen(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout,
		"daft-sandboxd workspace: serving %q from %s on %s\n"+
			"daft-sandboxd workspace: fingerprint %s\n"+
			"daft-sandboxd workspace: %d entries\n",
		name, path, srv.Addr(), id.Fingerprint(), len(manifest.GetEntries())); err != nil {
		return err
	}
	return runUntilCancel(ctx, srv)
}

// runUntilCancel blocks until the server returns or ctx is cancelled,
// doing a graceful Shutdown in the latter case. Mirrors the same
// helper in internal/cli — kept local to avoid a cross-package
// import.
func runUntilCancel(ctx context.Context, srv *mtls.Server) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve() }()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(
			context.WithoutCancel(ctx), 5*time.Second,
		)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}
