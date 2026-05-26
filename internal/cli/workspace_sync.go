package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/localai-org/darkaifactory/internal/brokerclient"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/trace"
	"github.com/localai-org/darkaifactory/internal/workspace"
)

const (
	pushSubName = "push"
	// pullCatalogSubName is the catalog-side pull verb. The legacy
	// chunk-sync `daft workspace pull <dest>` (pullSubName) coexists at
	// the top level; this one resolves dir-local mounts and rides the
	// orchestrator relay, taking no positional args.
	pullCatalogSubName = "pull"
)

// syncDirection selects whether runWorkspaceSync uploads or downloads
// each dir-local mount.
type syncDirection int

const (
	syncPush syncDirection = iota
	syncPull
)

func newWorkspacePushCmd() *cobra.Command {
	var opts workspaceClientOpts
	var casDir string
	cmd := &cobra.Command{
		Use:   pushSubName,
		Short: "Push recorded dir-local mounts up to the workspace's node CAS",
		Long: "Walks the local↔in-VM mappings recorded by `daft workspace mount`\n" +
			"in ./.daft/config.json, builds a content-addressed manifest of each\n" +
			"local directory, and uploads only the chunks the node doesn't\n" +
			"already hold. The bytes land in the assigned node's persistent CAS\n" +
			"(through the orchestrator relay), ready for the next launch to\n" +
			"materialise. Requires the workspace to have a target node set.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkspaceSync(cmd, opts, casDir, syncPush)
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	cmd.Flags().StringVar(&casDir, "cas-dir", "",
		"override the local CAS chunk-store dir (default: <config-dir>/cas)")
	return cmd
}

func newWorkspacePullCatalogCmd() *cobra.Command {
	var opts workspaceClientOpts
	var casDir string
	var preserveTime bool
	cmd := &cobra.Command{
		Use:   pullCatalogSubName,
		Short: "Pull recorded dir-local mounts down from the workspace's node CAS",
		Long: "The symmetric counterpart to `daft workspace push`: for each\n" +
			"recorded local↔in-VM mapping, fetches the node's manifest and\n" +
			"reconstructs it into the local directory, pulling only missing\n" +
			"chunks. Read-only mounts are skipped — the VM can't have changed\n" +
			"them. Requires the workspace to have a target node set.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runWorkspaceSync(cmd, opts, casDir, syncPull,
				workspace.ApplyOptions{PreserveModTime: preserveTime})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	cmd.Flags().StringVar(&casDir, "cas-dir", "",
		"override the local CAS chunk-store dir (default: <config-dir>/cas)")
	cmd.Flags().BoolVar(&preserveTime, "preserve-mtime", false,
		"restore file modification times from the manifest")
	return cmd
}

// runWorkspaceSync drives push or pull for every dir-local mount of
// the cwd's workspace. It resolves the workspace's target node from
// the orchestrator, opens a broker relay to that node's CAS-backed
// WorkspaceService, and runs workspace.Push / workspace.Pull per
// mount keyed by SyncKey(uuid, in-VM target).
func runWorkspaceSync(
	cmd *cobra.Command,
	opts workspaceClientOpts,
	casDir string,
	dir syncDirection,
	applyOpts ...workspace.ApplyOptions,
) error {
	mounts, err := dirLocalMounts()
	if err != nil {
		return err
	}
	if len(mounts) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no mounts recorded in ./.daft/config.json")
		return nil
	}
	handle, err := opts.resolveHandle()
	if err != nil {
		return err
	}
	orchestrator, err := resolveOrchestrator(opts.orchestrator, opts.configDir)
	if err != nil {
		return err
	}

	verb := pushSubName
	if dir == syncPull {
		verb = pullCatalogSubName
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()
	tracer, owned := tracerForCommand(ctx)
	ctx, sp := tracer.Start(ctx, spanName(verb),
		trace.WithAttributes(trace.KV{Key: rpcAddrAttrKey, Value: orchestrator}))
	defer sp.End()

	catalog, httpc, err := dialWorkspaceCatalog(opts.configDir, orchestrator, tracer)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	uuid, nodeFP, err := resolveSyncTarget(ctx, catalog, handle)
	if err != nil {
		return trace.RecordError(ctx, err)
	}

	cc, err := openClientCtx(opts.configDir, casDir)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	wsClient, closeRelay, err := dialNodeWorkspaceService(ctx, cc, orchestrator, uuid, nodeFP, tracer)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	defer func() { _ = closeRelay() }()

	if err := syncMounts(ctx, cmd.OutOrStdout(), wsClient, cc, uuid, mounts, dir, applyOpts...); err != nil {
		return trace.RecordError(ctx, err)
	}
	if owned {
		shipTraces(ctx, tracer, httpc, trimURLScheme(orchestrator), spanName(verb))
	}
	return nil
}

// resolveSyncTarget fetches the workspace and returns its UUID (the
// stable SyncKey namespace) and the parsed target node fingerprint.
// A workspace with no target can't be synced — the bytes have nowhere
// to land — so that's a clear, actionable error.
func resolveSyncTarget(
	ctx context.Context,
	catalog workspacev1connect.WorkspaceCatalogServiceClient,
	handle *workspacev1.WorkspaceHandle,
) (uuid string, nodeFP identity.Fingerprint, err error) {
	resp, err := catalog.Get(ctx, connect.NewRequest(&workspacev1.GetWorkspaceRequest{Handle: handle}))
	if err != nil {
		return "", identity.Fingerprint{}, fmt.Errorf("daft workspace: get workspace: %w", err)
	}
	ws := resp.Msg.GetWorkspace()
	fpStr := ws.GetSpec().GetTargetSandboxFingerprint()
	if fpStr == "" {
		return "", identity.Fingerprint{}, errors.New(
			"daft workspace: workspace has no target node; run `daft workspace target <node-fingerprint>` first",
		)
	}
	nodeFP, err = identity.ParseFingerprint(fpStr)
	if err != nil {
		return "", identity.Fingerprint{}, fmt.Errorf("daft workspace: parse target fingerprint %q: %w", fpStr, err)
	}
	return ws.GetMetadata().GetUuid(), nodeFP, nil
}

// dialNodeWorkspaceService opens a brokered relay to the target
// node's CAS-backed WorkspaceService and returns a client over it
// plus a close func that tears the relay down. The client is the
// broker serve owner and initiator (its own fingerprint), so it may
// legally RequestAttach naming the node.
func dialNodeWorkspaceService(
	ctx context.Context,
	cc *clientCtx,
	orchestrator, serveKey string,
	nodeFP identity.Fingerprint,
	tracer *trace.Tracer,
) (workspacev1connect.WorkspaceServiceClient, func() error, error) {
	bc, err := brokerclient.New(brokerclient.Config{
		URL: orchestrator, Identity: cc.id, Peers: cc.peers,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("daft workspace: broker client: %w", err)
	}
	ic, err := bc.DialBrokeredServe(ctx, brokerclient.DialServeOptions{
		Workspace:       serveKey,
		NodeFingerprint: nodeFP,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("daft workspace: dial node %s: %w", nodeFP, err)
	}
	interceptor := connect.WithInterceptors(tracer.ClientInterceptor())
	client := workspacev1connect.NewWorkspaceServiceClient(ic.HTTPClient, ic.BaseURL, interceptor)
	return client, ic.Close, nil
}

// syncMounts runs the chosen direction over every recorded mount.
// Local paths are resolved against the cwd at call time; pull skips
// read-only mounts (the VM can't have changed them).
func syncMounts(
	ctx context.Context,
	out io.Writer,
	wsClient workspacev1connect.WorkspaceServiceClient,
	cc *clientCtx,
	uuid string,
	mounts []mountMapping,
	dir syncDirection,
	applyOpts ...workspace.ApplyOptions,
) error {
	for _, m := range mounts {
		absLocal, err := filepath.Abs(m.Local)
		if err != nil {
			return fmt.Errorf("daft workspace: resolve local path %q: %w", m.Local, err)
		}
		key := workspace.SyncKey(uuid, m.Remote)
		if dir == syncPush {
			if err := pushMount(ctx, out, wsClient, cc, key, absLocal, m.Remote); err != nil {
				return err
			}
			continue
		}
		if m.ReadOnly {
			_, _ = fmt.Fprintf(out, "skip %s (read-only)\n", m.Remote)
			continue
		}
		if err := pullMount(ctx, out, wsClient, cc, key, absLocal, m.Remote, applyOpts...); err != nil {
			return err
		}
	}
	return nil
}

func pushMount(
	ctx context.Context,
	out io.Writer,
	wsClient workspacev1connect.WorkspaceServiceClient,
	cc *clientCtx,
	key, absLocal, remote string,
) error {
	manifest, err := workspace.BuildManifest(absLocal, cc.store, workspace.BuildOptions{})
	if err != nil {
		return fmt.Errorf("daft workspace push: build manifest for %s: %w", absLocal, err)
	}
	if err := workspace.Push(ctx, wsClient, cc.store, key, manifest); err != nil {
		return fmt.Errorf("daft workspace push: %s -> %s: %w", absLocal, remote, err)
	}
	_, _ = fmt.Fprintf(out, "pushed %s -> %s (%d entries)\n", absLocal, remote, len(manifest.GetEntries()))
	return nil
}

func pullMount(
	ctx context.Context,
	out io.Writer,
	wsClient workspacev1connect.WorkspaceServiceClient,
	cc *clientCtx,
	key, absLocal, remote string,
	applyOpts ...workspace.ApplyOptions,
) error {
	if err := os.MkdirAll(absLocal, 0o700); err != nil {
		return fmt.Errorf("daft workspace pull: mkdir %s: %w", absLocal, err)
	}
	var opts workspace.ApplyOptions
	if len(applyOpts) > 0 {
		opts = applyOpts[0]
	}
	if err := workspace.Pull(ctx, wsClient, cc.store, key, absLocal, opts); err != nil {
		return fmt.Errorf("daft workspace pull: %s -> %s: %w", remote, absLocal, err)
	}
	_, _ = fmt.Fprintf(out, "pulled %s -> %s\n", remote, absLocal)
	return nil
}
