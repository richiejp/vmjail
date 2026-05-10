package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/net/http2"

	"github.com/richiejp/darkaifactory/internal/cas"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/mtls"
	"github.com/richiejp/darkaifactory/internal/trace"
	"github.com/richiejp/darkaifactory/internal/workspace"
)

const (
	workspaceCmdName = "workspace"
	serveSubName     = "serve"
	pullSubName      = "pull"
)

// newWorkspaceCmd returns the `daft workspace` subtree. Two
// subcommands so far: `serve` exposes a directory as a workspace
// over mTLS, `pull` reconstructs one at a destination path.
func newWorkspaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   workspaceCmdName,
		Short: "Serve and fetch content-addressed workspace snapshots",
		Long: "A workspace is a content-addressed snapshot of a directory tree —\n" +
			"manifest plus zstd chunks, sha256-addressed. `daft workspace\n" +
			"serve` exposes one over mTLS for any approved peer to pull;\n" +
			"`daft workspace pull` reconstructs it at a destination.\n\n" +
			"Both sides share the daft client identity + peer store under\n" +
			"$XDG_CONFIG_HOME/daft/client/. See `daft auth show` to find\n" +
			"this peer's fingerprint.",
	}
	cmd.AddCommand(newWorkspaceServeCmd())
	cmd.AddCommand(newWorkspacePullCmd())
	return cmd
}

// parseWorkspaceFlag splits "name=path" into its components.
func parseWorkspaceFlag(s string) (name, path string, err error) {
	idx := strings.Index(s, "=")
	if idx <= 0 || idx == len(s)-1 {
		return "", "", fmt.Errorf("daft workspace: expected --workspace=name=path, got %q", s)
	}
	return s[:idx], s[idx+1:], nil
}

// clientCtx bundles the identity / peer store / local CAS that
// every `daft workspace` subcommand needs. Hoisted out so serve
// and pull share one setup path.
type clientCtx struct {
	id    *identity.Identity
	peers *identity.PeerStore
	store *cas.Store
}

func openClientCtx(configDirOverride, casDirOverride string) (*clientCtx, error) {
	dir, err := resolveClientDir(configDirOverride)
	if err != nil {
		return nil, err
	}
	id, err := loadOrGenerateClient(dir)
	if err != nil {
		return nil, err
	}
	peers, err := openClientPeerStore(dir)
	if err != nil {
		return nil, err
	}
	casPath := casDirOverride
	if casPath == "" {
		casPath = filepath.Join(dir, "cas")
	}
	store, err := cas.Open(casPath)
	if err != nil {
		return nil, err
	}
	return &clientCtx{id: id, peers: peers, store: store}, nil
}

// bindClientDirFlags adds the shared --config-dir / --cas-dir flag
// pair to cmd, both targets default to "" (XDG-aware fallback).
func bindClientDirFlags(cmd *cobra.Command, configDir, casDir *string) {
	cmd.Flags().StringVar(configDir, configDirFlag, "",
		"override the daft client config dir (default: $XDG_CONFIG_HOME/daft/client)")
	cmd.Flags().StringVar(casDir, "cas-dir", "",
		"override the local CAS chunk-store dir (default: <config-dir>/cas)")
}

func newWorkspaceServeCmd() *cobra.Command {
	var (
		workspaceFlag string
		addr          string
		configDir     string
		casDir        string
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
				workspaceFlag, addr, configDir, casDir)
		},
	}
	cmd.Flags().StringVar(&workspaceFlag, "workspace", "",
		"workspace to expose, in name=path form (e.g. dev=./repo)")
	cmd.Flags().StringVar(&addr, "addr", "0.0.0.0:9443",
		"TCP listen address (host:port)")
	bindClientDirFlags(cmd, &configDir, &casDir)
	return cmd
}

func runWorkspaceServe(
	ctx context.Context,
	stdout io.Writer,
	workspaceArg, addr, configDirOverride, casDirOverride string,
) error {
	tracer, _ := tracerForCommand(ctx)
	ctx = trace.ContextWithTracer(ctx, tracer)
	if workspaceArg == "" {
		return errors.New("daft workspace serve: --workspace is required")
	}
	name, srcPath, err := parseWorkspaceFlag(workspaceArg)
	if err != nil {
		return err
	}
	cc, err := openClientCtx(configDirOverride, casDirOverride)
	if err != nil {
		return err
	}
	manifest, err := workspace.BuildManifest(srcPath, cc.store, workspace.BuildOptions{})
	if err != nil {
		return err
	}
	srv, err := buildWorkspaceListener(name, addr, cc, manifest, tracer)
	if err != nil {
		return err
	}
	if err := srv.Listen(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout,
		"daft workspace: serving %q from %s on %s\n"+
			"daft workspace: fingerprint %s\n"+
			"daft workspace: cas %s (%d entries)\n",
		name, srcPath, srv.Addr(), cc.id.Fingerprint(),
		cc.store.Root(), len(manifest.GetEntries())); err != nil {
		return err
	}
	return runUntilCancel(ctx, srv)
}

// buildWorkspaceListener wires a workspace.Server (registered with
// name → manifest) into an mTLS HTTP/2 listener. The tracer's
// ServerInterceptor is wrapped around the WorkspaceService handler
// so every inbound GetManifest / GetChunk gets a server-side span,
// linked to the client's trace via the wire-propagated headers.
func buildWorkspaceListener(
	name, addr string, cc *clientCtx, manifest *workspacev1.Manifest, tracer *trace.Tracer,
) (*mtls.Server, error) {
	wsServer, err := workspace.NewServer(cc.store)
	if err != nil {
		return nil, err
	}
	if err := wsServer.Add(name, manifest); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	path, handler := workspacev1connect.NewWorkspaceServiceHandler(wsServer,
		connect.WithInterceptors(tracer.ServerInterceptor()))
	mux.Handle(path, handler)
	return mtls.New(mtls.Config{
		Identity: cc.id, Peers: cc.peers, Addr: addr, Handler: mux,
	})
}

// runUntilCancel blocks until the server returns or ctx is
// cancelled, doing a graceful Shutdown in the latter case.
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

func newWorkspacePullCmd() *cobra.Command {
	var (
		from          string
		workspaceName string
		configDir     string
		casDir        string
		preserveTime  bool
		timeout       time.Duration
	)
	cmd := &cobra.Command{
		Use:           pullSubName + " <dest>",
		Short:         "Pull a workspace from <addr> and reconstruct it at <dest>",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			return runWorkspacePull(ctx, cmd.OutOrStdout(),
				from, workspaceName, args[0], configDir, casDir, preserveTime)
		},
	}
	cmd.Flags().StringVar(&from, "from", "",
		"server address (host:port)")
	cmd.Flags().StringVar(&workspaceName, "workspace", "",
		"workspace name to fetch")
	bindClientDirFlags(cmd, &configDir, &casDir)
	cmd.Flags().BoolVar(&preserveTime, "preserve-mtime", false,
		"restore file modification times from the manifest")
	cmd.Flags().DurationVar(&timeout, "timeout", 60*time.Second,
		"overall timeout for the pull")
	return cmd
}

func runWorkspacePull(
	ctx context.Context,
	stdout io.Writer,
	from, workspaceName, dest, configDirOverride, casDirOverride string,
	preserveTime bool,
) error {
	tracer, owned := tracerForCommand(ctx)
	ctx = trace.ContextWithTracer(ctx, tracer)
	ctx, span := tracer.Start(ctx, "daft.workspace.pull",
		trace.WithAttributes(
			trace.KV{Key: "from", Value: from},
			trace.KV{Key: "workspace", Value: workspaceName},
			trace.KV{Key: "dest", Value: dest},
			trace.KV{Key: "preserve_mtime", Value: strconv.FormatBool(preserveTime)},
		))
	defer span.End()

	if from == "" {
		return trace.RecordError(ctx, errors.New("daft workspace pull: --from is required"))
	}
	if workspaceName == "" {
		return trace.RecordError(ctx, errors.New("daft workspace pull: --workspace is required"))
	}
	cc, err := openClientCtx(configDirOverride, casDirOverride)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	tlsCfg, err := identity.ClientTLSConfig(cc.id, cc.peers, "")
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	httpc := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}}
	interceptor := connect.WithInterceptors(tracer.ClientInterceptor())
	client := workspacev1connect.NewWorkspaceServiceClient(httpc, "https://"+from, interceptor)

	resp, err := client.GetManifest(ctx,
		connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: workspaceName}))
	if err != nil {
		return trace.RecordError(ctx, fmt.Errorf("daft workspace pull: GetManifest: %w", err))
	}
	manifest := resp.Msg.GetManifest()
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return trace.RecordError(ctx, fmt.Errorf("daft workspace pull: mkdir dest: %w", err))
	}
	if err := workspace.ApplyManifest(ctx, client, cc.store, manifest, dest,
		workspace.ApplyOptions{PreserveModTime: preserveTime}); err != nil {
		return trace.RecordError(ctx, err)
	}
	if _, err := fmt.Fprintf(stdout, "daft workspace: pulled %q to %s (%d entries)\n",
		workspaceName, dest, len(manifest.GetEntries())); err != nil {
		return trace.RecordError(ctx, err)
	}
	if owned {
		shipTraces(ctx, tracer, httpc, from, "daft workspace pull")
	}
	return nil
}
