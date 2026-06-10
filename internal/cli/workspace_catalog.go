package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/types/known/durationpb"

	sandboxv1 "github.com/localai-org/darkaifactory/internal/genpb/sandbox/v1"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/trace"
	"github.com/localai-org/darkaifactory/internal/workspacecatalog"
)

const orchestratorFlag = "orchestrator"

// rpcAddrAttrKey / workspaceAttrKey are span attribute keys.
// workspaceAttrKey is intentionally distinct from workspaceCmdName
// despite the shared literal — one is the cobra verb, the other is
// the value tag we stamp on spans.
const (
	rpcAddrAttrKey   = "rpc.addr"
	workspaceAttrKey = "workspace"
)

// Subcommand verb names. Promoted to constants so the cobra Use
// fields, span names, and tests reference the same string.
const (
	createSubName       = "create"
	statusSubName       = "status"
	imageSubName        = "image"
	devcontainerSubName = "devcontainer"
	targetSubName       = "target"
	mountSubName        = "mount"
	envSubName          = "env"
	portSubName         = "port"
	validateSubName     = "validate"
	launchSubName       = "launch"
	stopSubName         = "stop"
	addSubName          = "add"
	rmSubName           = "rm"
	resourcesSubName    = "resources"
	nestedSubName       = "nested"
	fuseSubName         = "privileged-fuse"
	buildSubName        = "privileged-build"
)

// workspaceClientOpts is the shared flag set every catalog
// subcommand uses to address its workspace. --name and --uuid are
// mutually exclusive; if neither is set, the name defaults to
// basename($PWD) per the PLAN's CPU workflow.
type workspaceClientOpts struct {
	orchestrator string
	configDir    string
	timeout      time.Duration
	name         string
	uuid         string
}

func bindWorkspaceClientFlags(cmd *cobra.Command, opts *workspaceClientOpts) {
	cmd.Flags().StringVar(&opts.orchestrator, orchestratorFlag, "",
		"orchestrator URL (https://host:port)")
	cmd.Flags().StringVar(&opts.configDir, configDirFlag, "",
		"override the daft client config dir (default: $XDG_CONFIG_HOME/daft/client)")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 10*time.Second,
		"overall RPC timeout")
	cmd.Flags().StringVar(&opts.name, "name", "",
		"workspace name (default: basename of the current directory)")
	cmd.Flags().StringVar(&opts.uuid, "uuid", "",
		"address the workspace by canonical UUID instead of name")
}

// resolveHandle returns the addressing handle the catalog RPCs use.
// Name lookups are scoped to the caller's mTLS fingerprint on the
// orchestrator side; the CLI doesn't need to know about that.
func (o workspaceClientOpts) resolveHandle() (*workspacev1.WorkspaceHandle, error) {
	if o.uuid != "" && o.name != "" {
		return nil, errors.New("daft workspace: --uuid and --name are mutually exclusive")
	}
	if o.uuid != "" {
		return &workspacev1.WorkspaceHandle{
			Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: o.uuid},
		}, nil
	}
	name := o.name
	if name == "" {
		derived, err := resolveWorkspaceNameDefault()
		if err != nil {
			return nil, err
		}
		name = derived
	}
	return &workspacev1.WorkspaceHandle{
		Handle: &workspacev1.WorkspaceHandle_Name{Name: name},
	}, nil
}

// deriveWorkspaceNameFromCwd returns basename($PWD), rejecting the
// degenerate cases ("", ".", "/") with a clear "pass --name"
// message. Shared by resolveHandle and workspaceNameForCreate so
// the error wording doesn't drift.
func deriveWorkspaceNameFromCwd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("daft workspace: getwd: %w", err)
	}
	name := filepath.Base(cwd)
	if name == "" || name == "." || name == "/" {
		return "", errors.New("daft workspace: cannot derive name from cwd; pass --name explicitly")
	}
	return name, nil
}

// dialWorkspaceCatalog builds the Connect client over the local
// mTLS identity. The returned *http.Client is exposed so callers
// can use it for shipTraces; cleanup is via t.Cleanup (the
// underlying transport pools by itself).
func dialWorkspaceCatalog(
	configDirOverride, orchestrator string,
	tracer *trace.Tracer,
) (workspacev1connect.WorkspaceCatalogServiceClient, *http.Client, error) {
	_, id, peers, err := openClientIdentity(configDirOverride)
	if err != nil {
		return nil, nil, err
	}
	tlsCfg, err := identity.ClientTLSConfig(id, peers, "")
	if err != nil {
		return nil, nil, err
	}
	httpc := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}}
	interceptor := connect.WithInterceptors(tracer.ClientInterceptor())
	client := workspacev1connect.NewWorkspaceCatalogServiceClient(httpc, orchestrator, interceptor)
	return client, httpc, nil
}

// spanName returns the OpenTelemetry span name for a catalog
// subcommand verb. Centralizes the "daft.workspace." prefix.
func spanName(verb string) string { return "daft.workspace." + verb }

type (
	// catalogFunc is the body of a handle-bearing subcommand.
	catalogFunc func(
		ctx context.Context,
		client workspacev1connect.WorkspaceCatalogServiceClient,
		handle *workspacev1.WorkspaceHandle,
	) error
	// catalogFuncNoHandle is the body of catalog-wide subcommands
	// (Create, List) — no per-workspace handle.
	catalogFuncNoHandle func(
		ctx context.Context,
		client workspacev1connect.WorkspaceCatalogServiceClient,
	) error
)

// runCatalogCmd handles a per-workspace subcommand: flag-guard →
// handle-resolve → trace span → dial → fn → trace ship.
func runCatalogCmd(
	cmd *cobra.Command,
	opts workspaceClientOpts,
	verb string,
	fn catalogFunc,
) error {
	handle, err := opts.resolveHandle()
	if err != nil {
		return err
	}
	return runCatalogCmdNoHandle(cmd, opts, verb,
		func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient) error {
			return fn(ctx, c, handle)
		})
}

// runCatalogCmdNoHandle is the catalog-wide variant: no handle
// resolution, no basename derivation. Also the workhorse that
// runCatalogCmd delegates to.
func runCatalogCmdNoHandle(
	cmd *cobra.Command,
	opts workspaceClientOpts,
	verb string,
	fn catalogFuncNoHandle,
) error {
	orchestrator, err := resolveOrchestrator(opts.orchestrator, opts.configDir)
	if err != nil {
		return err
	}
	opts.orchestrator = orchestrator
	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()

	tracer, owned := tracerForCommand(ctx)
	ctx, sp := tracer.Start(
		ctx, spanName(verb),
		trace.WithAttributes(trace.KV{Key: rpcAddrAttrKey, Value: opts.orchestrator}),
	)
	defer sp.End()

	client, httpc, err := dialWorkspaceCatalog(opts.configDir, opts.orchestrator, tracer)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	if err := fn(ctx, client); err != nil {
		return trace.RecordError(ctx, err)
	}
	if owned {
		shipTraces(ctx, tracer, httpc, trimURLScheme(opts.orchestrator), spanName(verb))
	}
	return nil
}

// newWorkspaceCatalogSubcommands returns every B3 verb wired under
// `daft workspace`.
func newWorkspaceCatalogSubcommands() []*cobra.Command {
	return []*cobra.Command{
		newWorkspaceCreateCmd(),
		newWorkspaceStatusCmd(),
		newWorkspaceListCmd(),
		newWorkspaceImageCmd(),
		newWorkspaceDevcontainerCmd(),
		newWorkspaceTargetCmd(),
		newWorkspaceResourcesCmd(),
		newWorkspaceNestedCmd(),
		newWorkspacePrivilegedFuseCmd(),
		newWorkspacePrivilegedBuildCmd(),
		newWorkspaceMountCmd(),
		newWorkspacePushCmd(),
		newWorkspacePullCatalogCmd(),
		newWorkspaceEnvCmd(),
		newWorkspacePortCmd(),
		newWorkspaceValidateCmd(),
		newWorkspaceLaunchCmd(),
		newWorkspaceStopCmd(),
		newWorkspaceAcquireCmd(),
		newWorkspaceReleaseCmd(),
	}
}

func newWorkspaceCreateCmd() *cobra.Command {
	var opts workspaceClientOpts
	cmd := &cobra.Command{
		Use:           createSubName,
		Short:         "Create a new workspace (name defaults to basename of cwd)",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, err := workspaceNameForCreate(opts)
			if err != nil {
				return err
			}
			return runCatalogCmdNoHandle(cmd, opts, createSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient) error {
					resp, err := c.Create(ctx, connect.NewRequest(
						&workspacev1.CreateWorkspaceRequest{Name: name},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

// workspaceNameForCreate returns --name if set, otherwise
// basename($PWD). --uuid is meaningless for Create and rejected.
func workspaceNameForCreate(opts workspaceClientOpts) (string, error) {
	if opts.uuid != "" {
		return "", errors.New("daft workspace create: --uuid is not valid for create; the orchestrator mints the UUID")
	}
	if opts.name != "" {
		return opts.name, nil
	}
	return resolveWorkspaceNameDefault()
}

//nolint:dupl // catalog subcommands are structurally parallel by design — runCatalogCmd factors the body.
func newWorkspaceStatusCmd() *cobra.Command {
	var opts workspaceClientOpts
	cmd := &cobra.Command{
		Use:           statusSubName,
		Short:         "Show the workspace's current state and missing fields",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCatalogCmd(cmd, opts, statusSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.Get(ctx, connect.NewRequest(
						&workspacev1.GetWorkspaceRequest{Handle: h},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

//nolint:dupl // catalog subcommands are structurally parallel by design — runCatalogCmd factors the body.
func newWorkspaceListCmd() *cobra.Command {
	var opts workspaceClientOpts
	var mineOnly bool
	cmd := &cobra.Command{
		Use:           listSubName,
		Short:         "List workspaces registered with the orchestrator",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCatalogCmdNoHandle(cmd, opts, listSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient) error {
					resp, err := c.List(ctx, connect.NewRequest(
						&workspacev1.ListWorkspacesRequest{MineOnly: mineOnly},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceList(cmd.OutOrStdout(), resp.Msg.GetWorkspaces())
				})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	cmd.Flags().BoolVar(&mineOnly, "mine", false, "filter to workspaces owned by this client")
	return cmd
}

//nolint:dupl // catalog subcommands are structurally parallel by design — runCatalogCmd factors the body.
func newWorkspaceImageCmd() *cobra.Command {
	var opts workspaceClientOpts
	cmd := &cobra.Command{
		Use:           imageSubName + " <ref>",
		Short:         "Set the workspace's OCI image reference (or clear it with --clear)",
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var ref string
			if len(args) > 0 {
				ref = args[0]
			}
			return runCatalogCmd(cmd, opts, imageSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.SetImage(ctx, connect.NewRequest(
						&workspacev1.SetImageRequest{Handle: h, ImageRef: ref},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

//nolint:dupl // catalog subcommands are structurally parallel by design — runCatalogCmd factors the body.
func newWorkspaceDevcontainerCmd() *cobra.Command {
	var opts workspaceClientOpts
	cmd := &cobra.Command{
		Use:   devcontainerSubName + " <path>",
		Short: "Set the path to a devcontainer.json within the workspace content (no arg clears it)",
		Long: "The path is relative to the workspace content root (e.g.\n" +
			".devcontainer/devcontainer.json), not a host path. At launch the\n" +
			"orchestrator reads it from the pushed content as a base spec and\n" +
			"overlays the workspace's explicit image/mounts/env/ports on top.",
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var path string
			if len(args) > 0 {
				path = args[0]
			}
			return runCatalogCmd(cmd, opts, devcontainerSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.SetDevcontainer(ctx, connect.NewRequest(
						&workspacev1.SetDevcontainerRequest{Handle: h, DevcontainerPath: path},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

//nolint:dupl // catalog subcommands are structurally parallel by design — runCatalogCmd factors the body.
func newWorkspaceTargetCmd() *cobra.Command {
	var opts workspaceClientOpts
	cmd := &cobra.Command{
		Use:           targetSubName + " <sandbox-fingerprint>",
		Short:         "Pin the workspace to a specific sandbox-host (or clear with no arg)",
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var fp string
			if len(args) > 0 {
				fp = args[0]
			}
			return runCatalogCmd(cmd, opts, targetSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.SetTarget(ctx, connect.NewRequest(
						&workspacev1.SetTargetRequest{Handle: h, SandboxFingerprint: fp},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

// newWorkspaceResourcesCmd sets the guest VM's RAM and vCPU
// sizing. Both flags are optional; a flag left unset sends zero,
// which the orchestrator records as "clear" (executor default).
func newWorkspaceResourcesCmd() *cobra.Command {
	var (
		opts   workspaceClientOpts
		memStr string
		cpus   uint32
	)
	cmd := &cobra.Command{
		Use:           resourcesSubName,
		Short:         "Set the workspace VM's RAM (--memory 4G) and/or vCPUs (--cpus N)",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			memBytes, err := parseMemoryBytes(memStr)
			if err != nil {
				return err
			}
			return runCatalogCmd(cmd, opts, resourcesSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.SetResources(ctx, connect.NewRequest(
						&workspacev1.SetResourcesRequest{
							Handle:      h,
							MemoryBytes: memBytes,
							CpuCores:    cpus,
						},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	cmd.Flags().StringVar(&memStr, "memory", "",
		"guest RAM, e.g. 4G / 512M (empty clears, reverts to default)")
	cmd.Flags().Uint32Var(&cpus, "cpus", 0,
		"guest vCPU count (0 clears, reverts to default)")
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

// newWorkspaceNestedCmd toggles the workspace's nested-virt grant.
// `--enable` binds /dev/kvm into the workload container at Launch so
// it can run its own (L2) VMs; `--disable` reverts to the
// unprivileged default. Exactly one of the two flags is required so
// the grant is never changed by accident.
//
// (see that function's note) — each grant keeps its own wording.
//
//nolint:dupl // Deliberately parallel to newWorkspacePrivilegedFuseCmd
func newWorkspaceNestedCmd() *cobra.Command {
	var (
		opts    workspaceClientOpts
		enable  bool
		disable bool
	)
	cmd := &cobra.Command{
		Use:           nestedSubName,
		Short:         "Enable/disable nested virtualisation (--enable binds /dev/kvm into the container)",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if enable == disable {
				return fmt.Errorf("specify exactly one of --enable / --disable")
			}
			return runCatalogCmd(cmd, opts, nestedSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.SetNestedVirtualization(ctx, connect.NewRequest(
						&workspacev1.SetNestedVirtualizationRequest{
							Handle: h,
							Enable: enable,
						},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	cmd.Flags().BoolVar(&enable, "enable", false,
		"grant nested virtualisation (bind /dev/kvm into the workload container)")
	cmd.Flags().BoolVar(&disable, "disable", false,
		"revoke nested virtualisation (the default)")
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

// newWorkspacePrivilegedFuseCmd toggles the workspace's privileged
// FUSE-mount grant. `--enable` binds /dev/fuse into the workload
// container and grants CAP_SYS_ADMIN at Launch so it can mount FUSE
// filesystems; `--disable` reverts to the unprivileged default.
// CAP_SYS_ADMIN is near-root inside the VM, so the grant is named for
// its danger and exactly one of the two flags is required — it is
// never changed by accident.
//
// are --enable/--disable grant toggles differing only by RPC and copy;
// merging them behind a generic helper would obscure the per-grant
// wording the danger-naming relies on.
//
//nolint:dupl // Deliberately parallel to newWorkspaceNestedCmd: both
func newWorkspacePrivilegedFuseCmd() *cobra.Command {
	var (
		opts    workspaceClientOpts
		enable  bool
		disable bool
	)
	cmd := &cobra.Command{
		Use:           fuseSubName,
		Short:         "Enable/disable privileged FUSE mounts (--enable grants CAP_SYS_ADMIN + /dev/fuse)",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if enable == disable {
				return fmt.Errorf("specify exactly one of --enable / --disable")
			}
			return runCatalogCmd(cmd, opts, fuseSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.SetPrivilegedFuseMounts(ctx, connect.NewRequest(
						&workspacev1.SetPrivilegedFuseMountsRequest{
							Handle: h,
							Enable: enable,
						},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	cmd.Flags().BoolVar(&enable, "enable", false,
		"grant privileged FUSE mounts (bind /dev/fuse + CAP_SYS_ADMIN into the workload container)")
	cmd.Flags().BoolVar(&disable, "disable", false,
		"revoke privileged FUSE mounts (the default)")
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

// newWorkspacePrivilegedBuildCmd toggles the workspace's build capability
// grant. `--enable` gives the workload container the standard container
// cap set at Launch so an in-sandbox buildah can build images (daft zeroes
// container caps by default); `--disable` reverts to the zero-cap default.
// The caps are near-root inside the VM, so the grant is danger-named and
// exactly one of the two flags is required.
//
// both are --enable/--disable grant toggles differing only by RPC and copy.
//
//nolint:dupl // Deliberately parallel to newWorkspacePrivilegedFuseCmd:
func newWorkspacePrivilegedBuildCmd() *cobra.Command {
	var (
		opts    workspaceClientOpts
		enable  bool
		disable bool
	)
	cmd := &cobra.Command{
		Use:           buildSubName,
		Short:         "Enable/disable the build capability grant (--enable gives the standard container cap set for in-sandbox image builds)",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if enable == disable {
				return fmt.Errorf("specify exactly one of --enable / --disable")
			}
			return runCatalogCmd(cmd, opts, buildSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.SetPrivilegedBuild(ctx, connect.NewRequest(
						&workspacev1.SetPrivilegedBuildRequest{
							Handle: h,
							Enable: enable,
						},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	cmd.Flags().BoolVar(&enable, "enable", false,
		"grant the build capability set (standard container caps for an in-sandbox buildah)")
	cmd.Flags().BoolVar(&disable, "disable", false,
		"revoke the build capability grant (the default)")
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

// parseMemoryBytes converts a human size ("4G", "512M", "1024")
// to bytes. Empty string ⇒ 0 (clear). Suffixes K/M/G/T are
// powers of 1024; a bare number is bytes.
func parseMemoryBytes(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	mult := uint64(1)
	num := s
	switch s[len(s)-1] {
	case 'K', 'k':
		mult, num = 1<<10, s[:len(s)-1]
	case 'M', 'm':
		mult, num = 1<<20, s[:len(s)-1]
	case 'G', 'g':
		mult, num = 1<<30, s[:len(s)-1]
	case 'T', 't':
		mult, num = 1<<40, s[:len(s)-1]
	}
	n, err := strconv.ParseUint(num, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid memory size %q: %w", s, err)
	}
	return n * mult, nil
}

func newWorkspaceMountCmd() *cobra.Command {
	var opts workspaceClientOpts
	var readOnly bool
	cmd := &cobra.Command{
		Use:   mountSubName + " [local] [in-vm-target]",
		Short: "Map a local directory to an in-VM path (no args: list mappings)",
		Long: "`daft workspace mount ./local /remote` records the local↔in-VM\n" +
			"mapping in ./.daft/config.json and registers the mount on the\n" +
			"workspace (target=/remote). `daft workspace push` then sends the\n" +
			"local files up; `daft workspace pull` brings changes back. Run\n" +
			"with no arguments to list the recorded mappings.\n\n" +
			"The advanced `mount add` / `mount rm` subcommands operate on the\n" +
			"server-side spec directly (logical source identifiers).",
		Args:          cobra.MaximumNArgs(2),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch len(args) {
			case 0:
				return listDirLocalMounts(cmd.OutOrStdout())
			case 2:
				return runWorkspaceMountMap(cmd, opts, args[0], args[1], readOnly)
			default:
				return errors.New("daft workspace mount: expected `<local> <in-vm-target>` or no arguments")
			}
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "mount read-only in the VM")
	cmd.AddCommand(newWorkspaceMountAddCmd())
	cmd.AddCommand(newWorkspaceMountRmCmd())
	return cmd
}

// runWorkspaceMountMap registers a local↔in-VM mount: it validates
// the paths, calls AddMount on the orchestrator (source defaults to
// the workspace server-side), then records the mapping in
// ./.daft/config.json so push/pull know which local dir feeds it. The
// local path is stored absolute so push/pull resolve the same
// directory regardless of the cwd they run from.
//
// The dir-local write happens inside the RPC session: if it fails
// after AddMount succeeded, we roll the server mount back with
// RemoveMount so the two never drift out of step.
func runWorkspaceMountMap(cmd *cobra.Command, opts workspaceClientOpts, local, remote string, readOnly bool) error {
	if !filepath.IsAbs(remote) {
		return fmt.Errorf("daft workspace mount: in-VM target must be absolute, got %q", remote)
	}
	absLocal, err := filepath.Abs(local)
	if err != nil {
		return fmt.Errorf("daft workspace mount: resolve local path: %w", err)
	}
	if info, statErr := os.Stat(absLocal); statErr != nil || !info.IsDir() {
		return fmt.Errorf("daft workspace mount: local path %q must be an existing directory", local)
	}
	return runCatalogCmd(cmd, opts, mountSubName,
		func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
			resp, err := c.AddMount(ctx, connect.NewRequest(
				&workspacev1.AddMountRequest{
					Handle: h,
					Mount:  &sandboxv1.Mount{Target: remote, ReadOnly: readOnly},
				},
			))
			if err != nil {
				return err
			}
			path, err := addDirLocalMount(mountMapping{Local: absLocal, Remote: remote, ReadOnly: readOnly})
			if err != nil {
				// Roll the just-added server mount back so a failed
				// command leaves neither side recording the mount.
				// errors.Join drops a nil rollback error, so the happy
				// rollback path returns just the config-write error.
				_, rbErr := c.RemoveMount(ctx, connect.NewRequest(
					&workspacev1.RemoveMountRequest{Handle: h, Target: remote},
				))
				return errors.Join(err, rbErr)
			}
			if err := writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace()); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "mount %s -> %s recorded in %s\n", absLocal, remote, path)
			return nil
		})
}

// listDirLocalMounts prints the mount mappings from ./.daft/config.json.
func listDirLocalMounts(out io.Writer) error {
	mounts, err := dirLocalMounts()
	if err != nil {
		return err
	}
	if len(mounts) == 0 {
		_, _ = fmt.Fprintln(out, "no mounts recorded in ./.daft/config.json")
		return nil
	}
	for _, m := range mounts {
		ro := ""
		if m.ReadOnly {
			ro = " (read-only)"
		}
		_, _ = fmt.Fprintf(out, "%s -> %s%s\n", m.Local, m.Remote, ro)
	}
	return nil
}

func newWorkspaceMountAddCmd() *cobra.Command {
	var opts workspaceClientOpts
	var source string
	var readOnly bool
	cmd := &cobra.Command{
		Use:           addSubName + " <in-vm-target>",
		Short:         "Append a mount to the workspace",
		Long:          "Target is the in-VM mount path (absolute). --source is the logical\nsource identifier the sandbox-host resolves at dispatch time;\ndefaults to the workspace's UUID when omitted.",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			return runCatalogCmd(cmd, opts, mountSubName+"."+addSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.AddMount(ctx, connect.NewRequest(
						&workspacev1.AddMountRequest{
							Handle: h,
							Mount: &sandboxv1.Mount{
								Target:   target,
								Source:   source,
								ReadOnly: readOnly,
							},
						},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	cmd.Flags().StringVar(&source, "source", "",
		"logical source identifier (default: workspace UUID, resolved server-side)")
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "mount read-only")
	return cmd
}

//nolint:dupl // catalog subcommands are structurally parallel by design — runCatalogCmd factors the body.
func newWorkspaceMountRmCmd() *cobra.Command {
	var opts workspaceClientOpts
	cmd := &cobra.Command{
		Use:           rmSubName + " <in-vm-target>",
		Short:         "Drop the mount with the given in-VM target",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			if err := runCatalogCmd(cmd, opts, mountSubName+"."+rmSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.RemoveMount(ctx, connect.NewRequest(
						&workspacev1.RemoveMountRequest{Handle: h, Target: target},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				}); err != nil {
				return err
			}
			// Keep the dir-local mapping in step with the spec.
			if found, err := removeDirLocalMount(target); err != nil {
				return err
			} else if found {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "dropped local mapping for %s\n", target)
			}
			return nil
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

func newWorkspaceEnvCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   envSubName,
		Short: "Manage workspace environment variables (add | rm)",
	}
	cmd.AddCommand(newWorkspaceEnvAddCmd())
	cmd.AddCommand(newWorkspaceEnvRmCmd())
	return cmd
}

func newWorkspaceEnvAddCmd() *cobra.Command {
	var opts workspaceClientOpts
	cmd := &cobra.Command{
		Use:           addSubName + " NAME=VALUE",
		Short:         "Append an environment variable",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, value, ok := strings.Cut(args[0], "=")
			if !ok {
				return fmt.Errorf("daft workspace env add: expected NAME=VALUE, got %q", args[0])
			}
			if name == "" {
				return errors.New("daft workspace env add: empty NAME")
			}
			return runCatalogCmd(cmd, opts, envSubName+"."+addSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.AddEnv(ctx, connect.NewRequest(
						&workspacev1.AddEnvRequest{
							Handle: h,
							Env:    &sandboxv1.EnvVar{Name: name, Value: value},
						},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

//nolint:dupl // catalog subcommands are structurally parallel by design — runCatalogCmd factors the body.
func newWorkspaceEnvRmCmd() *cobra.Command {
	var opts workspaceClientOpts
	cmd := &cobra.Command{
		Use:           rmSubName + " NAME",
		Short:         "Drop the environment variable with the given name",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			return runCatalogCmd(cmd, opts, envSubName+"."+rmSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.RemoveEnv(ctx, connect.NewRequest(
						&workspacev1.RemoveEnvRequest{Handle: h, Name: name},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

func newWorkspacePortCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   portSubName,
		Short: "Manage workspace port mappings (add | rm)",
	}
	cmd.AddCommand(newWorkspacePortAddCmd())
	cmd.AddCommand(newWorkspacePortRmCmd())
	return cmd
}

func newWorkspacePortAddCmd() *cobra.Command {
	var opts workspaceClientOpts
	cmd := &cobra.Command{
		Use:           addSubName + " PORT[/tcp|/udp]",
		Short:         "Append a container port mapping (protocol defaults to tcp)",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			port, proto, err := parsePortSpec(args[0])
			if err != nil {
				return err
			}
			return runCatalogCmd(cmd, opts, portSubName+"."+addSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.AddPort(ctx, connect.NewRequest(
						&workspacev1.AddPortRequest{
							Handle: h,
							Port:   &sandboxv1.PortMapping{ContainerPort: port, Protocol: proto},
						},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

func newWorkspacePortRmCmd() *cobra.Command {
	var opts workspaceClientOpts
	cmd := &cobra.Command{
		Use:           rmSubName + " PORT[/tcp|/udp]",
		Short:         "Drop the port mapping with the given (port, protocol)",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			port, proto, err := parsePortSpec(args[0])
			if err != nil {
				return err
			}
			return runCatalogCmd(cmd, opts, portSubName+"."+rmSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.RemovePort(ctx, connect.NewRequest(
						&workspacev1.RemovePortRequest{
							Handle: h, ContainerPort: port, Protocol: proto,
						},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

// parsePortSpec parses "<port>[/tcp|/udp]". Protocol defaults to tcp.
func parsePortSpec(spec string) (uint32, sandboxv1.Protocol, error) {
	portStr := spec
	proto := sandboxv1.Protocol_PROTOCOL_TCP
	if i := strings.Index(spec, "/"); i >= 0 {
		portStr = spec[:i]
		switch strings.ToLower(spec[i+1:]) {
		case "tcp":
			proto = sandboxv1.Protocol_PROTOCOL_TCP
		case "udp":
			proto = sandboxv1.Protocol_PROTOCOL_UDP
		default:
			return 0, 0, fmt.Errorf("daft workspace: unknown protocol %q (want tcp or udp)", spec[i+1:])
		}
	}
	p, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return 0, 0, fmt.Errorf("daft workspace: invalid port %q: %w", portStr, err)
	}
	return uint32(p), proto, nil
}

//nolint:dupl // catalog subcommands are structurally parallel by design — runCatalogCmd factors the body.
func newWorkspaceValidateCmd() *cobra.Command {
	var opts workspaceClientOpts
	cmd := &cobra.Command{
		Use:           validateSubName,
		Short:         "Recompute readiness; auto-promote DRAFT → READY when all fields are set",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCatalogCmd(cmd, opts, validateSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.Validate(ctx, connect.NewRequest(
						&workspacev1.ValidateRequest{Handle: h},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

//nolint:dupl // catalog subcommands are structurally parallel by design — runCatalogCmd factors the body.
func newWorkspaceLaunchCmd() *cobra.Command {
	var opts workspaceClientOpts
	cmd := &cobra.Command{
		Use:           launchSubName,
		Short:         "Dispatch the workspace to its pinned sandbox-host",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCatalogCmd(cmd, opts, launchSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.Launch(ctx, connect.NewRequest(
						&workspacev1.LaunchRequest{Handle: h},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

// acquireSubName / releaseSubName are the verb tokens under
// `daft workspace`. Hoisted as constants so the cobra registration
// and the e2e tests reference the same string.
const (
	acquireSubName = "acquire"
	releaseSubName = "release"
)

//nolint:dupl // catalog subcommands are structurally parallel by design — runCatalogCmd factors the body.
func newWorkspaceAcquireCmd() *cobra.Command {
	var opts workspaceClientOpts
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:           acquireSubName,
		Short:         "Acquire (or refresh) the workspace's write lock",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCatalogCmd(cmd, opts, acquireSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.Acquire(ctx, connect.NewRequest(
						&workspacev1.AcquireRequest{Handle: h, Ttl: durationpb.New(ttl)},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	cmd.Flags().DurationVar(&ttl, "ttl", 5*time.Minute,
		"how long the lock should auto-release after; refresh by re-running")
	return cmd
}

//nolint:dupl // catalog subcommands are structurally parallel by design — runCatalogCmd factors the body.
func newWorkspaceReleaseCmd() *cobra.Command {
	var opts workspaceClientOpts
	cmd := &cobra.Command{
		Use:           releaseSubName,
		Short:         "Release the workspace's write lock",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCatalogCmd(cmd, opts, releaseSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.Release(ctx, connect.NewRequest(
						&workspacev1.ReleaseRequest{Handle: h},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

//nolint:dupl // catalog subcommands are structurally parallel by design — runCatalogCmd factors the body.
func newWorkspaceStopCmd() *cobra.Command {
	var opts workspaceClientOpts
	cmd := &cobra.Command{
		Use:           stopSubName,
		Short:         "Stop the running workspace",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCatalogCmd(cmd, opts, stopSubName,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.Stop(ctx, connect.NewRequest(
						&workspacev1.StopRequest{Handle: h},
					))
					if err != nil {
						return err
					}
					return writeWorkspaceSummary(cmd.OutOrStdout(), resp.Msg.GetWorkspace())
				})
		},
	}
	bindWorkspaceClientFlags(cmd, &opts)
	return cmd
}

// writeWorkspaceSummary renders the workspace as a key-value block.
// Used by every command that returns a single Workspace.
func writeWorkspaceSummary(w io.Writer, ws *workspacev1.Workspace) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	md := ws.GetMetadata()
	spec := ws.GetSpec()
	status := ws.GetStatus()
	rows := [][2]string{
		{"UUID", md.GetUuid()},
		{"Name", md.GetName()},
		{"Status", summariseConditions(status)},
		{"Owner", md.GetOwnerFingerprint()},
		{"Image", emptyOrDash(spec.GetImageRef())},
		{"Target", emptyOrDash(spec.GetTargetSandboxFingerprint())},
		{"Sandbox", emptyOrDash(status.GetCurrentSandbox().GetUid())},
	}
	if editPending(md, status) {
		rows = append(rows, [2]string{"Edit", fmt.Sprintf("pending (gen %d, observed %d)",
			md.GetGeneration(), status.GetObservedGeneration())})
	}
	if len(status.GetMissing()) > 0 {
		rows = append(rows, [2]string{"Missing", strings.Join(status.GetMissing(), ", ")})
	}
	rows = append(
		rows,
		[2]string{"Mounts", joinMounts(spec.GetMounts())},
		[2]string{"Env", joinEnv(spec.GetEnv())},
		[2]string{"Ports", joinPorts(spec.GetPorts())},
	)
	if spec.GetNestedVirtualization().GetEnable() {
		rows = append(rows, [2]string{"Nested-virt", "enabled"})
	}
	if spec.GetPrivilegedFuseMounts().GetEnable() {
		rows = append(rows, [2]string{"Privileged-FUSE", "enabled (CAP_SYS_ADMIN)"})
	}
	if spec.GetPrivilegedBuild().GetEnable() {
		rows = append(rows, [2]string{"Privileged-Build", "enabled (standard container caps)"})
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(tw, "%s:\t%s\n", row[0], row[1]); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// writeWorkspaceList renders the catalog snapshot in a tab-aligned
// table. One row per workspace, columns chosen for at-a-glance
// triage.
func writeWorkspaceList(w io.Writer, all []*workspacev1.Workspace) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tUUID\tSTATUS\tIMAGE\tTARGET"); err != nil {
		return err
	}
	for _, ws := range all {
		md := ws.GetMetadata()
		spec := ws.GetSpec()
		if _, err := fmt.Fprintf(
			tw, "%s\t%s\t%s\t%s\t%s\n",
			md.GetName(),
			md.GetUuid(),
			summariseConditions(ws.GetStatus()),
			emptyOrDash(spec.GetImageRef()),
			emptyOrDash(spec.GetTargetSandboxFingerprint()),
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// summariseConditions renders a single status token for table
// columns and the summary block: Dispatched > Ready > NotReady.
// Mirrors the prior enum's user-visible vocabulary at a glance
// while delegating the truth to the conditions array.
func summariseConditions(status *workspacev1.WorkspaceStatus) string {
	dispatched, ready := false, false
	for _, c := range status.GetConditions() {
		if c.GetStatus() != sandboxv1.ConditionStatus_CONDITION_STATUS_TRUE {
			continue
		}
		switch c.GetType() {
		case workspacecatalog.ConditionDispatched:
			dispatched = true
		case workspacecatalog.ConditionReady:
			ready = true
		}
	}
	switch {
	case dispatched:
		return workspacecatalog.ConditionDispatched
	case ready:
		return workspacecatalog.ConditionReady
	default:
		return "NotReady"
	}
}

// editPending reports whether the workspace has spec edits the
// running Sandbox hasn't picked up yet. Mirrors the K8s
// "observedGeneration < generation" idiom — see ARCHITECTURE.md.
// Observed=0 (never reconciled) only counts as pending when a
// Sandbox is actually in flight; otherwise it's the natural state
// of a freshly-created workspace and would be noisy.
func editPending(md *workspacev1.WorkspaceMetadata, status *workspacev1.WorkspaceStatus) bool {
	if status.GetCurrentSandbox() == nil {
		return false
	}
	return status.GetObservedGeneration() < md.GetGeneration()
}

func emptyOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func joinMounts(mounts []*sandboxv1.Mount) string {
	if len(mounts) == 0 {
		return "—"
	}
	parts := make([]string, len(mounts))
	for i, m := range mounts {
		ro := ""
		if m.GetReadOnly() {
			ro = " ro"
		}
		parts[i] = fmt.Sprintf("%s%s", m.GetTarget(), ro)
	}
	return strings.Join(parts, ", ")
}

func joinEnv(env []*sandboxv1.EnvVar) string {
	if len(env) == 0 {
		return "—"
	}
	parts := make([]string, len(env))
	for i, e := range env {
		parts[i] = e.GetName()
	}
	return strings.Join(parts, ", ")
}

func joinPorts(ports []*sandboxv1.PortMapping) string {
	if len(ports) == 0 {
		return "—"
	}
	parts := make([]string, len(ports))
	for i, p := range ports {
		proto := strings.ToLower(strings.TrimPrefix(p.GetProtocol().String(), "PROTOCOL_"))
		parts[i] = fmt.Sprintf("%d/%s", p.GetContainerPort(), proto)
	}
	return strings.Join(parts, ", ")
}
