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

	"github.com/richiejp/darkaifactory/internal/brokerclient"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/trace"
	"github.com/richiejp/darkaifactory/internal/workspace"
	"github.com/richiejp/darkaifactory/internal/workspacesync"
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

func newWorkspaceChangesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: changesSubName, Short: "Manage unadopted sandbox workspace changes"}
	var opts workspaceClientOpts
	var casDir string
	reject := &cobra.Command{
		Use: "reject", Short: "Reject exact unadopted sandbox manifests", Args: cobra.NoArgs, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mounts, err := dirLocalMounts()
			if err != nil {
				return err
			}
			return runNodeWorkspaceRPC(cmd, opts, casDir, "changes.reject", func(
				ctx context.Context, client workspacev1connect.WorkspaceServiceClient, uuid string,
			) error {
				return rejectWorkspaceChanges(ctx, cmd.OutOrStdout(), client, uuid, mounts)
			})
		},
	}
	bindWorkspaceClientFlags(reject, &opts)
	reject.Flags().StringVar(&casDir, "cas-dir", "", "override the local CAS directory used while opening the broker identity")
	cmd.AddCommand(reject)
	return cmd
}

func newWorkspaceStorageCmd() *cobra.Command {
	cmd := &cobra.Command{Use: storageSubName, Short: "Inspect the target node's workspace CAS"}
	cmd.AddCommand(newWorkspaceStorageStatusCmd(), newWorkspaceStoragePruneCmd())
	return cmd
}

func newWorkspaceUnfreezeCmd() *cobra.Command {
	var opts workspaceClientOpts
	var casDir string
	cmd := &cobra.Command{
		Use: unfreezeSubName, Short: "Explicitly resume a storage-frozen workspace after remediation",
		Args: cobra.NoArgs, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runNodeWorkspaceRPC(cmd, opts, casDir, unfreezeSubName, func(
				ctx context.Context, client workspacev1connect.WorkspaceServiceClient, uuid string,
			) error {
				expected, err := workspaceDiskManifestExpectations(ctx, client, uuid)
				if err != nil {
					return err
				}
				response, err := client.UnfreezeWorkspaceDisk(ctx, connect.NewRequest(&workspacev1.UnfreezeWorkspaceDiskRequest{
					WorkspaceUuid: uuid, ExpectedManifests: expected,
				}))
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "workspace %s unfrozen: used=%d available=%d allowance=%d\n",
					uuid, response.Msg.GetUsedBytes(), response.Msg.GetAvailableBytes(), response.Msg.GetAllowanceBytes())
				return err
			})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	cmd.Flags().StringVar(&casDir, "cas-dir", "", "override the local CAS directory used while opening the broker identity")
	return cmd
}

func newWorkspaceDeleteCmd() *cobra.Command {
	var opts workspaceClientOpts
	var casDir string
	cmd := &cobra.Command{
		Use: deleteSubName, Short: "Permanently delete a stopped workspace and its retained data",
		Args: cobra.NoArgs, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mounts, err := dirLocalMounts()
			if err != nil {
				return err
			}
			return runWorkspaceDelete(cmd, opts, casDir, mounts)
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	cmd.Flags().StringVar(&casDir, "cas-dir", "", "override the local CAS directory used while opening the broker identity")
	return cmd
}

func runWorkspaceDelete(cmd *cobra.Command, opts workspaceClientOpts, casDir string, mounts []mountMapping) error {
	handle, err := opts.resolveHandle()
	if err != nil {
		return err
	}
	orchestrator, err := resolveOrchestrator(opts.orchestrator, opts.configDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()
	tracer, _ := tracerForCommand(ctx)
	catalog, _, err := dialWorkspaceCatalog(opts.configDir, orchestrator, tracer)
	if err != nil {
		return err
	}
	workspaceResponse, err := catalog.Get(ctx, connect.NewRequest(&workspacev1.GetWorkspaceRequest{Handle: handle}))
	if err != nil {
		return fmt.Errorf("daft workspace delete: get workspace: %w", err)
	}
	resource := workspaceResponse.Msg.GetWorkspace()
	uuid := resource.GetMetadata().GetUuid()
	target := resource.GetSpec().GetTargetSandboxFingerprint()
	if target == "" {
		return deleteWorkspaceCatalog(ctx, cmd.OutOrStdout(), catalog, handle, uuid, resource.GetMetadata().GetGeneration())
	}
	nodeFP, err := identity.ParseFingerprint(target)
	if err != nil {
		return fmt.Errorf("daft workspace delete: parse target fingerprint: %w", err)
	}
	clientContext, err := openClientCtx(opts.configDir, casDir, opts.orchestrator)
	if err != nil {
		return err
	}
	client, closeRelay, err := dialNodeWorkspaceService(ctx, clientContext, orchestrator, uuid, nodeFP, tracer)
	if err != nil {
		return err
	}
	defer func() { _ = closeRelay() }()
	expected, err := workspaceManifestExpectations(ctx, client, uuid, mounts)
	if err != nil {
		return err
	}
	if _, err := client.DeleteWorkspaceData(ctx, connect.NewRequest(&workspacev1.DeleteWorkspaceDataRequest{
		WorkspaceUuid: uuid, ExpectedManifests: expected,
	})); err != nil {
		return fmt.Errorf("daft workspace delete: delete Node data: %w", err)
	}
	return deleteWorkspaceCatalog(ctx, cmd.OutOrStdout(), catalog, handle, uuid, resource.GetMetadata().GetGeneration())
}

func deleteWorkspaceCatalog(
	ctx context.Context,
	out io.Writer,
	catalog workspacev1connect.WorkspaceCatalogServiceClient,
	handle *workspacev1.WorkspaceHandle,
	uuid string,
	generation uint64,
) error {
	if _, err := catalog.Delete(ctx, connect.NewRequest(&workspacev1.DeleteWorkspaceRequest{
		Handle: handle, ExpectedGeneration: generation,
	})); err != nil {
		return fmt.Errorf("daft workspace delete: delete catalog record: %w", err)
	}
	_, err := fmt.Fprintf(out, "deleted workspace %s\n", uuid)
	return err
}

func newWorkspaceStorageStatusCmd() *cobra.Command {
	var opts workspaceClientOpts
	var casDir string
	cmd := &cobra.Command{
		Use: "status", Short: "Show bounded target-node CAS accounting", Args: cobra.NoArgs, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runNodeWorkspaceRPC(cmd, opts, casDir, "storage.status", func(
				ctx context.Context, client workspacev1connect.WorkspaceServiceClient, uuid string,
			) error {
				disk, err := client.GetWorkspaceDiskStatus(ctx, connect.NewRequest(&workspacev1.GetWorkspaceDiskStatusRequest{WorkspaceUuid: uuid}))
				if err != nil {
					return err
				}
				response, err := client.GetStorageStatus(ctx, connect.NewRequest(&workspacev1.GetStorageStatusRequest{}))
				if err != nil {
					return err
				}
				return writeWorkspaceStorageStatus(cmd.OutOrStdout(), disk.Msg, response.Msg)
			})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	cmd.Flags().StringVar(&casDir, "cas-dir", "", "override the local CAS directory used while opening the broker identity")
	return cmd
}

func newWorkspaceStoragePruneCmd() *cobra.Command {
	var opts workspaceClientOpts
	var casDir string
	var dryRun bool
	var maxCandidates uint32
	cmd := &cobra.Command{
		Use: pruneSubName, Short: "Preview target-node CAS cleanup candidates", Args: cobra.NoArgs, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !dryRun {
				return errors.New("daft workspace storage prune: --dry-run is required")
			}
			return runNodeWorkspaceRPC(cmd, opts, casDir, "storage.prune", func(
				ctx context.Context, client workspacev1connect.WorkspaceServiceClient, _ string,
			) error {
				response, err := client.PruneStorage(ctx, connect.NewRequest(&workspacev1.PruneStorageRequest{DryRun: true, MaxCandidates: maxCandidates}))
				if err != nil {
					return err
				}
				return writeWorkspacePrunePlan(cmd.OutOrStdout(), response.Msg)
			})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	cmd.Flags().StringVar(&casDir, "cas-dir", "", "override the local CAS directory used while opening the broker identity")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview candidates without deleting data (required)")
	cmd.Flags().Uint32Var(&maxCandidates, "max-candidates", 50, "maximum candidates to return (1-200)")
	return cmd
}

func rejectWorkspaceChanges(
	ctx context.Context,
	out io.Writer,
	client workspacev1connect.WorkspaceServiceClient,
	uuid string,
	mounts []mountMapping,
) error {
	changed := 0
	for _, mount := range mounts {
		if mount.ReadOnly {
			continue
		}
		key := workspace.SyncKey(uuid, mount.Remote)
		current, err := client.GetManifest(ctx, connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: key}))
		if err != nil {
			return fmt.Errorf("daft workspace changes reject: inspect %s: %w", mount.Remote, err)
		}
		response, err := client.RejectManifest(ctx, connect.NewRequest(&workspacev1.RejectManifestRequest{
			Workspace: key, ManifestDigest: current.Msg.GetManifestDigest(), Generation: current.Msg.GetGeneration(),
		}))
		if err != nil {
			return fmt.Errorf("daft workspace changes reject: %s: %w", mount.Remote, err)
		}
		changed++
		if response.Msg.GetCurrentManifestPresent() {
			_, _ = fmt.Fprintf(out, "rejected %s; restored generation %d (%s)\n", mount.Remote, response.Msg.GetGeneration(), response.Msg.GetManifestDigest())
		} else {
			_, _ = fmt.Fprintf(out, "rejected %s; no prior client manifest\n", mount.Remote)
		}
	}
	if changed == 0 {
		_, _ = fmt.Fprintln(out, "no writable mounts recorded in ./.daft/config.json")
	}
	return nil
}

func workspaceManifestExpectations(
	ctx context.Context,
	client workspacev1connect.WorkspaceServiceClient,
	uuid string,
	mounts []mountMapping,
) ([]*workspacev1.ManifestExpectation, error) {
	expected := make([]*workspacev1.ManifestExpectation, 0, len(mounts))
	for _, mount := range mounts {
		key := workspace.SyncKey(uuid, mount.Remote)
		current, err := client.GetManifest(ctx, connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: key}))
		if err != nil {
			return nil, fmt.Errorf("daft workspace: inspect manifest %s: %w", mount.Remote, err)
		}
		expected = append(expected, &workspacev1.ManifestExpectation{
			Workspace: key, ManifestDigest: current.Msg.GetManifestDigest(), Generation: current.Msg.GetGeneration(),
		})
	}
	return expected, nil
}

func workspaceDiskManifestExpectations(
	ctx context.Context,
	client workspacev1connect.WorkspaceServiceClient,
	uuid string,
) ([]*workspacev1.ManifestExpectation, error) {
	status, err := client.GetWorkspaceDiskStatus(ctx, connect.NewRequest(&workspacev1.GetWorkspaceDiskStatusRequest{
		WorkspaceUuid: uuid,
	}))
	if err != nil {
		return nil, fmt.Errorf("daft workspace: inspect disk manifests: %w", err)
	}
	return status.Msg.GetCurrentManifests(), nil
}

func writeWorkspaceStorageStatus(out io.Writer, disk *workspacev1.GetWorkspaceDiskStatusResponse, status *workspacev1.GetStorageStatusResponse) error {
	_, _ = fmt.Fprintf(out, "disk_used=%d disk_available=%d disk_allowance=%d storage_frozen=%t running=%t workspace_protected=%d manifests=%d\n",
		disk.GetUsedBytes(), disk.GetAvailableBytes(), disk.GetAllowanceBytes(), disk.GetFrozen(), disk.GetRunning(), disk.GetProtectedBytes(), len(disk.GetCurrentManifests()))
	_, err := fmt.Fprintf(out, "usage=%d protected=%d recoverable=%d queued=%d chunks=%d hard=%d soft=%d target=%d expiry_seconds=%d\n",
		status.GetUsageBytes(), status.GetProtectedBytes(), status.GetRecoverableBytes(), status.GetQueuedBytes(), status.GetChunkCount(),
		status.GetHardBytes(), status.GetSoftBytes(), status.GetTargetBytes(), status.GetExpirySeconds())
	return err
}

func writeWorkspacePrunePlan(out io.Writer, response *workspacev1.PruneStorageResponse) error {
	for _, candidate := range response.GetCandidates() {
		_, _ = fmt.Fprintf(out, "%s size=%d score=%.6f disposition=%s last_use=%d\n",
			candidate.GetDigest(), candidate.GetSize(), candidate.GetScore(), candidate.GetDisposition(), candidate.GetLastUseUnix())
	}
	_, err := fmt.Fprintf(out, "usage_before=%d usage_after=%d candidates=%d truncated=%t\n",
		response.GetUsageBefore(), response.GetUsageAfter(), len(response.GetCandidates()), response.GetTruncated())
	return err
}

func runNodeWorkspaceRPC(
	cmd *cobra.Command,
	opts workspaceClientOpts,
	casDir, verb string,
	run func(context.Context, workspacev1connect.WorkspaceServiceClient, string) error,
) error {
	handle, err := opts.resolveHandle()
	if err != nil {
		return err
	}
	orchestrator, err := resolveOrchestrator(opts.orchestrator, opts.configDir)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()
	tracer, _ := tracerForCommand(ctx)
	catalog, _, err := dialWorkspaceCatalog(opts.configDir, orchestrator, tracer)
	if err != nil {
		return err
	}
	uuid, nodeFP, err := resolveSyncTarget(ctx, catalog, handle)
	if err != nil {
		return err
	}
	clientContext, err := openClientCtx(opts.configDir, casDir, opts.orchestrator)
	if err != nil {
		return err
	}
	client, closeRelay, err := dialNodeWorkspaceService(ctx, clientContext, orchestrator, uuid, nodeFP, tracer)
	if err != nil {
		return err
	}
	defer func() { _ = closeRelay() }()
	if err := run(ctx, client, uuid); err != nil {
		return fmt.Errorf("daft workspace %s: %w", verb, err)
	}
	return nil
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

	cc, err := openClientCtx(opts.configDir, casDir, opts.orchestrator)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	if dir == syncPush {
		if err := pushWorkspaceMounts(ctx, cmd.OutOrStdout(), orchestrator, uuid, cc, catalog); err != nil {
			return trace.RecordError(ctx, err)
		}
		if owned {
			shipTraces(ctx, tracer, httpc, trimURLScheme(orchestrator), spanName(verb))
		}
		return nil
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

func pushWorkspaceMounts(
	ctx context.Context,
	out io.Writer,
	orchestrator, uuid string,
	cc *clientCtx,
	catalog workspacev1connect.WorkspaceCatalogServiceClient,
) error {
	projectDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("daft workspace push: get project directory: %w", err)
	}
	result, err := workspacesync.Push(ctx, workspacesync.PushConfig{
		Orchestrator: orchestrator, ProjectDir: projectDir, WorkspaceUUID: uuid,
		Identity: cc.id, Peers: cc.peers, Store: cc.store, Catalog: catalog,
	})
	if err != nil {
		return err
	}
	for _, mount := range result.Mounts {
		_, _ = fmt.Fprintf(out, "pushed %s -> %s (%d entries)\n", mount.Local, mount.Remote, mount.Entries)
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
