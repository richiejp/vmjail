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

	"github.com/localai-org/darkaifactory/internal/brokerclient"
	"github.com/localai-org/darkaifactory/internal/cas"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/mtls"
	"github.com/localai-org/darkaifactory/internal/trace"
	"github.com/localai-org/darkaifactory/internal/workspace"
)

const (
	workspaceCmdName = "workspace"
	debugSubName     = "debug"
	serveSubName     = "serve"
	pullSubName      = "pull"
	attachSubName    = "attach"
	diffSubName      = "diff"
	execSubName      = "exec"
)

// newWorkspaceCmd returns the `daft workspace` subtree: `serve`
// exposes a directory, `pull` reconstructs one at a destination,
// `attach` triggers an orchestrator-brokered pairing.
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
	for _, sub := range newWorkspaceCatalogSubcommands() {
		cmd.AddCommand(sub)
	}
	cmd.AddCommand(newWorkspaceDebugCmd())
	cmd.AddCommand(newWorkspaceForwardCmd())
	cmd.AddCommand(newWorkspaceUpCmd())
	cmd.AddCommand(newWorkspaceExecCmd())
	for _, sub := range newWorkspaceConfigCmds() {
		cmd.AddCommand(sub)
	}
	return cmd
}

// newWorkspaceDebugCmd groups the deprecated chunk-sync commands
// (serve / pull / attach / diff) under `daft workspace debug`. They
// predate the catalog-driven workflow and the broker-relayed
// `daft workspace push`/`pull`; moving them here frees the top-level
// `pull` name for the catalog verb and keeps them available for
// one-shot manual content-addressed transfers and diagnostics.
func newWorkspaceDebugCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   debugSubName,
		Short: "Low-level content-addressed transfer + diagnostics (advanced)",
		Long: "Direct manifest/chunk tools predating the workspace catalog:\n" +
			"  serve  — expose a directory as a content-addressed workspace\n" +
			"  pull   — reconstruct a served workspace at a destination\n" +
			"  attach — trigger an orchestrator-brokered serve↔sandbox pairing\n" +
			"  diff   — compare a local directory against a remote workspace\n\n" +
			"For the everyday flow use `daft workspace push` / `pull`, which\n" +
			"resolve dir-local mounts and relay through the orchestrator.",
	}
	cmd.AddCommand(newWorkspaceServeCmd())
	cmd.AddCommand(newWorkspacePullCmd())
	cmd.AddCommand(newWorkspaceAttachCmd())
	cmd.AddCommand(newWorkspaceDiffCmd())
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

func openClientCtx(configDirOverride, casDirOverride, orchFlag string) (*clientCtx, error) {
	dir, id, peers, err := openClientIdentity(configDirOverride, orchFlag)
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
	opts := workspaceServeOpts{}
	cmd := &cobra.Command{
		Use:   serveSubName,
		Short: "Serve a directory as a workspace over mTLS",
		Long: "Two modes:\n" +
			"  Direct: --addr <host:port> binds a TCP listener and accepts mTLS\n" +
			"          dials from approved peers. LAN/dev only.\n" +
			"  Broker: --orchestrator <addr> registers with an orchestrator and\n" +
			"          accepts brokered attaches over the relay. Works through\n" +
			"          NAT — the user-host has no inbound port.\n" +
			"Direct is the default for backward compatibility; --orchestrator\n" +
			"takes precedence when set.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(),
				os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return runWorkspaceServe(ctx, cmd.OutOrStdout(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.workspaceFlag, "workspace", "",
		"workspace to expose, in name=path form (e.g. dev=./repo)")
	cmd.Flags().StringVar(&opts.addr, "addr", "0.0.0.0:9443",
		"TCP listen address for direct-dial mode (host:port)")
	cmd.Flags().StringVar(&opts.orchestrator, "orchestrator", "",
		"orchestrator URL (https://host:port) for broker mode; takes precedence over --addr")
	bindClientDirFlags(cmd, &opts.configDir, &opts.casDir)
	return cmd
}

// workspaceServeOpts gathers `daft workspace serve` flags so adding
// a new one is a single struct field plus one Flags() binding.
type workspaceServeOpts struct {
	workspaceFlag string
	addr          string
	orchestrator  string
	configDir     string
	casDir        string
}

func runWorkspaceServe(ctx context.Context, stdout io.Writer, opts workspaceServeOpts) error {
	tracer, _ := tracerForCommand(ctx)
	ctx = trace.ContextWithTracer(ctx, tracer)
	if opts.workspaceFlag == "" {
		return errors.New("daft workspace serve: --workspace is required")
	}
	name, srcPath, err := parseWorkspaceFlag(opts.workspaceFlag)
	if err != nil {
		return err
	}
	cc, err := openClientCtx(opts.configDir, opts.casDir, opts.orchestrator)
	if err != nil {
		return err
	}
	manifest, err := workspace.BuildManifest(srcPath, cc.store, workspace.BuildOptions{})
	if err != nil {
		return err
	}
	if opts.orchestrator != "" {
		return runWorkspaceServeBrokered(ctx, stdout, opts.orchestrator, name, srcPath, cc, manifest, tracer)
	}
	return runWorkspaceServeDirect(ctx, stdout, opts.addr, name, srcPath, cc, manifest, tracer)
}

func runWorkspaceServeDirect(
	ctx context.Context, stdout io.Writer, addr, name, srcPath string,
	cc *clientCtx, manifest *workspacev1.Manifest, tracer *trace.Tracer,
) error {
	srv, err := buildWorkspaceListener(name, addr, cc, manifest, tracer)
	if err != nil {
		return err
	}
	if err := srv.Listen(); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout,
		"daft workspace: serving %q from %s on %s\n"+
			"daft workspace: fingerprint %s\n"+
			"daft workspace: cas %s (%d entries)\n",
		name, srcPath, srv.Addr(), cc.id.Fingerprint(),
		cc.store.Root(), len(manifest.GetEntries()))
	return runUntilCancel(ctx, srv)
}

// runWorkspaceServeBrokered registers with the orchestrator's
// BrokerService and accepts inbound attaches over the spliced
// relay. Inner mTLS terminates here; each attach gets a fresh
// http2.Server running the same WorkspaceService handler as the
// direct path.
func runWorkspaceServeBrokered(
	ctx context.Context, stdout io.Writer, orchestratorURL, name, srcPath string,
	cc *clientCtx, manifest *workspacev1.Manifest, tracer *trace.Tracer,
) error {
	wsServer, err := workspace.NewServer(cc.store)
	if err != nil {
		return err
	}
	if err := wsServer.Add(name, manifest); err != nil {
		return err
	}
	mux := http.NewServeMux()
	path, handler := workspacev1connect.NewWorkspaceServiceHandler(wsServer,
		connect.WithInterceptors(tracer.ServerInterceptor()))
	mux.Handle(path, handler)

	bc, err := brokerclient.New(brokerclient.Config{
		URL: orchestratorURL, Identity: cc.id, Peers: cc.peers,
	})
	if err != nil {
		return fmt.Errorf("daft workspace serve: %w", err)
	}
	_, _ = fmt.Fprintf(stdout,
		"daft workspace: brokered %q from %s via %s\n"+
			"daft workspace: fingerprint %s\n"+
			"daft workspace: cas %s (%d entries)\n",
		name, srcPath, orchestratorURL, cc.id.Fingerprint(),
		cc.store.Root(), len(manifest.GetEntries()))
	return bc.ServeBrokered(ctx, brokerclient.ServeOptions{
		Workspace: name, Handler: mux,
	})
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
			trace.KV{Key: workspaceAttrKey, Value: workspaceName},
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
	cc, err := openClientCtx(configDirOverride, casDirOverride, "")
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	httpc, client, err := dialWorkspaceClient(cc, from, tracer)
	if err != nil {
		return trace.RecordError(ctx, err)
	}

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

// workspaceAttachOpts gathers `daft workspace attach` flags.
type workspaceAttachOpts struct {
	orchestrator string
	workspace    string
	sandbox      string
	configDir    string
	timeout      time.Duration
}

func newWorkspaceAttachCmd() *cobra.Command {
	var opts workspaceAttachOpts
	cmd := &cobra.Command{
		Use:   attachSubName,
		Short: "Tell the orchestrator to broker a registered sandbox to a running serve",
		Long: "After `daft workspace serve --orchestrator ...` is up on this host\n" +
			"and a sandbox-host has `daft-sandboxd run --workspace-broker ...`\n" +
			"waiting, this command issues RequestAttach so the broker pairs them.\n" +
			"The v1 broker ACL accepts RequestAttach only from the serve owner,\n" +
			"so the caller's own fingerprint is used as the serve identity.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
			defer cancel()
			return runWorkspaceAttach(ctx, cmd.OutOrStdout(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.orchestrator, "orchestrator", "",
		"orchestrator URL (https://host:port)")
	cmd.Flags().StringVar(&opts.workspace, "workspace", "",
		"workspace name registered by `daft workspace serve`")
	cmd.Flags().StringVar(&opts.sandbox, "sandbox", "",
		"sandbox id registered by `daft-sandboxd run`")
	cmd.Flags().StringVar(&opts.configDir, configDirFlag, "",
		"override the daft client config dir (default: $XDG_CONFIG_HOME/daft/client)")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 30*time.Second,
		"overall timeout for the attach request")
	return cmd
}

// workspaceDiffOpts gathers `daft workspace diff` flags.
type workspaceDiffOpts struct {
	from          string
	workspaceName string
	configDir     string
	casDir        string
	detail        bool
	timeout       time.Duration
}

func newWorkspaceDiffCmd() *cobra.Command {
	var opts workspaceDiffOpts
	cmd := &cobra.Command{
		Use:   diffSubName + " <local-dir>",
		Short: "Compare a local directory against a remote workspace",
		Long: "Builds a manifest of <local-dir> and fetches the remote workspace's\n" +
			"manifest from --from. Prints the entry-level diff:\n" +
			"  + path   added on the remote (would be pulled)\n" +
			"  - path   removed on the remote (would be left intact locally)\n" +
			"  ~ path   modified content\n" +
			"  m path   mode change only\n" +
			"  t path   entry kind changed (file <-> symlink etc.)\n" +
			"Exit code 0 always; an empty diff prints nothing.",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
			defer cancel()
			return runWorkspaceDiff(ctx, cmd.OutOrStdout(), args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.from, "from", "",
		"server address (host:port)")
	cmd.Flags().StringVar(&opts.workspaceName, "workspace", "",
		"workspace name to fetch")
	bindClientDirFlags(cmd, &opts.configDir, &opts.casDir)
	cmd.Flags().BoolVar(&opts.detail, "detail", false,
		"include kind/mode transitions on changed lines")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 60*time.Second,
		"overall timeout for the diff")
	return cmd
}

func runWorkspaceDiff(ctx context.Context, stdout io.Writer, localDir string, opts workspaceDiffOpts) error {
	switch {
	case opts.from == "":
		return errors.New("daft workspace diff: --from is required")
	case opts.workspaceName == "":
		return errors.New("daft workspace diff: --workspace is required")
	}
	cc, err := openClientCtx(opts.configDir, opts.casDir, "")
	if err != nil {
		return err
	}
	localManifest, err := workspace.BuildManifest(localDir, cc.store, workspace.BuildOptions{})
	if err != nil {
		return fmt.Errorf("daft workspace diff: build local manifest: %w", err)
	}
	tracer, _ := tracerForCommand(ctx)
	httpc, client, err := dialWorkspaceClient(cc, opts.from, tracer)
	if err != nil {
		return err
	}
	defer httpc.CloseIdleConnections()
	resp, err := client.GetManifest(ctx,
		connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: opts.workspaceName}))
	if err != nil {
		return fmt.Errorf("daft workspace diff: GetManifest: %w", err)
	}
	d := workspace.DiffManifests(localManifest, resp.Msg.GetManifest())
	for _, c := range d.Changes {
		printChange(stdout, c, opts.detail)
	}
	return nil
}

func printChange(w io.Writer, c workspace.Change, detail bool) {
	if detail && c.Kind == workspace.ModeChanged {
		_, _ = fmt.Fprintf(w, "%c %s  mode %04o -> %04o\n", c.Kind.Glyph(), c.Path,
			c.From.GetMode()&0o7777, c.To.GetMode()&0o7777)
		return
	}
	if detail && c.Kind == workspace.TypeChanged {
		_, _ = fmt.Fprintf(w, "%c %s  %s -> %s\n", c.Kind.Glyph(), c.Path,
			entryKindLabel(c.From), entryKindLabel(c.To))
		return
	}
	_, _ = fmt.Fprintf(w, "%c %s\n", c.Kind.Glyph(), c.Path)
}

func entryKindLabel(e *workspacev1.FileEntry) string {
	switch e.GetKind() {
	case workspacev1.EntryKind_ENTRY_KIND_FILE:
		return "file"
	case workspacev1.EntryKind_ENTRY_KIND_DIR:
		return "dir"
	case workspacev1.EntryKind_ENTRY_KIND_SYMLINK:
		return "symlink"
	default:
		return "?"
	}
}

// dialWorkspaceClient builds an mTLS HTTP/2 client + Connect-RPC
// WorkspaceServiceClient pointed at addr, wired through the
// tracer's ClientInterceptor. Returned httpc must be closed by the
// caller via CloseIdleConnections.
func dialWorkspaceClient(cc *clientCtx, addr string, tracer *trace.Tracer) (*http.Client, workspacev1connect.WorkspaceServiceClient, error) {
	tlsCfg, err := identity.ClientTLSConfig(cc.id, cc.peers, "")
	if err != nil {
		return nil, nil, err
	}
	httpc := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}}
	interceptor := connect.WithInterceptors(tracer.ClientInterceptor())
	return httpc, workspacev1connect.NewWorkspaceServiceClient(httpc, "https://"+addr, interceptor), nil
}

func runWorkspaceAttach(ctx context.Context, stdout io.Writer, opts workspaceAttachOpts) error {
	switch {
	case opts.workspace == "":
		return errors.New("daft workspace attach: --workspace is required")
	case opts.sandbox == "":
		return errors.New("daft workspace attach: --sandbox is required")
	}
	orchestrator, err := resolveOrchestrator(opts.orchestrator, opts.configDir)
	if err != nil {
		return err
	}
	opts.orchestrator = orchestrator
	_, id, peers, err := openClientIdentity(opts.configDir, opts.orchestrator)
	if err != nil {
		return err
	}
	bc, err := brokerclient.New(brokerclient.Config{
		URL: opts.orchestrator, Identity: id, Peers: peers,
	})
	if err != nil {
		return fmt.Errorf("daft workspace attach: %w", err)
	}
	sessionID, _, err := bc.Attach(ctx, id.Fingerprint(), opts.workspace, opts.sandbox)
	if err != nil {
		return fmt.Errorf("daft workspace attach: %w", err)
	}
	_, _ = fmt.Fprintf(stdout,
		"daft workspace: requested attach of sandbox %q to workspace %q (session %s)\n",
		opts.sandbox, opts.workspace, sessionID)
	return nil
}
