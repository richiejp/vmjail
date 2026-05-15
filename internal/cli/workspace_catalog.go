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

	sandboxctrlv1 "github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/trace"
)

// orchestratorFlag is the long form of --orchestrator. Shared with
// the sandbox-list / ping commands by convention but kept local
// here so renaming this command's flag doesn't require touching
// unrelated subtrees.
const orchestratorFlag = "orchestrator"

// rpcAddrAttrKey is the span attribute key the catalog commands
// stamp with the orchestrator URL.
const rpcAddrAttrKey = "rpc.addr"

// Subcommand verb names. Promoted to constants so the cobra Use
// fields, span names, and tests reference the same string.
const (
	createSubName   = "create"
	statusSubName   = "status"
	imageSubName    = "image"
	targetSubName   = "target"
	mountSubName    = "mount"
	envSubName      = "env"
	portSubName     = "port"
	validateSubName = "validate"
	launchSubName   = "launch"
	stopSubName     = "stop"
	addSubName      = "add"
	rmSubName       = "rm"
)

// errOrchestratorRequired surfaces the up-front guard that every
// catalog subcommand applies before any RPC plumbing.
var errOrchestratorRequired = errors.New("daft workspace: --orchestrator is required")

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
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("daft workspace: getwd: %w", err)
		}
		name = filepath.Base(cwd)
	}
	if name == "" || name == "." || name == "/" {
		return nil, errors.New("daft workspace: could not derive workspace name; pass --name explicitly")
	}
	return &workspacev1.WorkspaceHandle{
		Handle: &workspacev1.WorkspaceHandle_Name{Name: name},
	}, nil
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

// catalogFunc is the per-command body. The handle is pre-resolved;
// commands that take no handle (Create, List) receive a nil one.
type catalogFunc func(
	ctx context.Context,
	client workspacev1connect.WorkspaceCatalogServiceClient,
	handle *workspacev1.WorkspaceHandle,
) error

// runCatalogCmd is the boilerplate every catalog subcommand reuses:
// flag-guard → handle-resolve → trace span → dial → fn → trace
// shipping. needHandle=false skips the basename-derivation for
// commands that operate on the whole catalog (Create, List).
func runCatalogCmd(
	cmd *cobra.Command,
	opts workspaceClientOpts,
	span string,
	needHandle bool,
	fn catalogFunc,
) error {
	if opts.orchestrator == "" {
		return errOrchestratorRequired
	}
	var handle *workspacev1.WorkspaceHandle
	if needHandle {
		h, err := opts.resolveHandle()
		if err != nil {
			return err
		}
		handle = h
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), opts.timeout)
	defer cancel()

	tracer, owned := tracerForCommand(ctx)
	ctx, sp := tracer.Start(
		ctx, span,
		trace.WithAttributes(trace.KV{Key: rpcAddrAttrKey, Value: opts.orchestrator}),
	)
	defer sp.End()

	client, httpc, err := dialWorkspaceCatalog(opts.configDir, opts.orchestrator, tracer)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	if err := fn(ctx, client, handle); err != nil {
		return trace.RecordError(ctx, err)
	}
	if owned {
		shipTraces(ctx, tracer, httpc, trimURLScheme(opts.orchestrator), span)
	}
	return nil
}

// newWorkspaceCatalogSubcommands returns every B3 verb wired under
// `daft workspace`. Exposed so newWorkspaceCmd can mix them with
// the existing deprecated chunk-sync subcommands without those
// files importing the catalog plumbing.
func newWorkspaceCatalogSubcommands() []*cobra.Command {
	return []*cobra.Command{
		newWorkspaceCreateCmd(),
		newWorkspaceStatusCmd(),
		newWorkspaceListCmd(),
		newWorkspaceImageCmd(),
		newWorkspaceTargetCmd(),
		newWorkspaceMountCmd(),
		newWorkspaceEnvCmd(),
		newWorkspacePortCmd(),
		newWorkspaceValidateCmd(),
		newWorkspaceLaunchCmd(),
		newWorkspaceStopCmd(),
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
			return runCatalogCmd(cmd, opts, "daft.workspace."+createSubName, false,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, _ *workspacev1.WorkspaceHandle) error {
					name, err := workspaceNameForCreate(opts)
					if err != nil {
						return err
					}
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
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("daft workspace create: getwd: %w", err)
	}
	name := filepath.Base(cwd)
	if name == "" || name == "." || name == "/" {
		return "", errors.New("daft workspace create: cannot derive a name; pass --name")
	}
	return name, nil
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
			return runCatalogCmd(cmd, opts, "daft.workspace."+statusSubName, true,
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
			return runCatalogCmd(cmd, opts, "daft.workspace."+listSubName, false,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, _ *workspacev1.WorkspaceHandle) error {
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
			ref, err := readSingleArg(args, "image ref")
			if err != nil {
				return err
			}
			return runCatalogCmd(cmd, opts, "daft.workspace."+imageSubName, true,
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
func newWorkspaceTargetCmd() *cobra.Command {
	var opts workspaceClientOpts
	cmd := &cobra.Command{
		Use:           targetSubName + " <sandbox-fingerprint>",
		Short:         "Pin the workspace to a specific sandbox-host (or clear with no arg)",
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fp, err := readSingleArg(args, "sandbox fingerprint")
			if err != nil {
				return err
			}
			return runCatalogCmd(cmd, opts, "daft.workspace."+targetSubName, true,
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

func newWorkspaceMountCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   mountSubName,
		Short: "Manage workspace mounts (add | rm)",
	}
	cmd.AddCommand(newWorkspaceMountAddCmd())
	cmd.AddCommand(newWorkspaceMountRmCmd())
	return cmd
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
			return runCatalogCmd(cmd, opts, "daft.workspace."+mountSubName+"."+addSubName, true,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resolvedSource, err := defaultMountSource(ctx, c, h, source)
					if err != nil {
						return err
					}
					resp, err := c.AddMount(ctx, connect.NewRequest(
						&workspacev1.AddMountRequest{
							Handle: h,
							Mount: &sandboxctrlv1.Mount{
								Target:   target,
								Source:   resolvedSource,
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
		"logical source identifier (default: workspace UUID)")
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "mount read-only")
	return cmd
}

// defaultMountSource picks the right Source value when the caller
// didn't pass --source. The catalog rejects free-form host paths
// today (Phase F adoption is where those land), so we default to
// the workspace's own UUID — which the sandbox-host resolves to
// the daft-fuse view of the workspace.
func defaultMountSource(
	ctx context.Context,
	c workspacev1connect.WorkspaceCatalogServiceClient,
	handle *workspacev1.WorkspaceHandle,
	override string,
) (string, error) {
	if override != "" {
		return override, nil
	}
	resp, err := c.Get(ctx, connect.NewRequest(&workspacev1.GetWorkspaceRequest{Handle: handle}))
	if err != nil {
		return "", fmt.Errorf("daft workspace mount add: resolve workspace: %w", err)
	}
	return resp.Msg.GetWorkspace().GetUuid(), nil
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
			return runCatalogCmd(cmd, opts, "daft.workspace."+mountSubName+"."+rmSubName, true,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.RemoveMount(ctx, connect.NewRequest(
						&workspacev1.RemoveMountRequest{Handle: h, Target: target},
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
			return runCatalogCmd(cmd, opts, "daft.workspace."+envSubName+"."+addSubName, true,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.AddEnv(ctx, connect.NewRequest(
						&workspacev1.AddEnvRequest{
							Handle: h,
							Env:    &sandboxctrlv1.EnvVar{Name: name, Value: value},
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
			return runCatalogCmd(cmd, opts, "daft.workspace."+envSubName+"."+rmSubName, true,
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
			return runCatalogCmd(cmd, opts, "daft.workspace."+portSubName+"."+addSubName, true,
				func(ctx context.Context, c workspacev1connect.WorkspaceCatalogServiceClient, h *workspacev1.WorkspaceHandle) error {
					resp, err := c.AddPort(ctx, connect.NewRequest(
						&workspacev1.AddPortRequest{
							Handle: h,
							Port:   &sandboxctrlv1.PortMapping{ContainerPort: port, Protocol: proto},
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
			return runCatalogCmd(cmd, opts, "daft.workspace."+portSubName+"."+rmSubName, true,
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
func parsePortSpec(spec string) (uint32, sandboxctrlv1.Protocol, error) {
	portStr := spec
	proto := sandboxctrlv1.Protocol_PROTOCOL_TCP
	if i := strings.Index(spec, "/"); i >= 0 {
		portStr = spec[:i]
		switch strings.ToLower(spec[i+1:]) {
		case "tcp":
			proto = sandboxctrlv1.Protocol_PROTOCOL_TCP
		case "udp":
			proto = sandboxctrlv1.Protocol_PROTOCOL_UDP
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
			return runCatalogCmd(cmd, opts, "daft.workspace."+validateSubName, true,
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
			return runCatalogCmd(cmd, opts, "daft.workspace."+launchSubName, true,
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

//nolint:dupl // catalog subcommands are structurally parallel by design — runCatalogCmd factors the body.
func newWorkspaceStopCmd() *cobra.Command {
	var opts workspaceClientOpts
	cmd := &cobra.Command{
		Use:           stopSubName,
		Short:         "Stop the running workspace",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCatalogCmd(cmd, opts, "daft.workspace."+stopSubName, true,
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

// readSingleArg returns args[0] for set-or-clear commands that take
// an optional value. Empty (no arg) means "clear".
func readSingleArg(args []string, _ string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	return args[0], nil
}

// writeWorkspaceSummary renders the workspace as a key-value block.
// Used by every command that returns a single Workspace.
func writeWorkspaceSummary(w io.Writer, ws *workspacev1.Workspace) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	rows := [][2]string{
		{"UUID", ws.GetUuid()},
		{"Name", ws.GetName()},
		{"Status", trimStatusPrefix(ws.GetStatus().String())},
		{"Owner", ws.GetOwnerFingerprint()},
		{"Image", emptyOrDash(ws.GetImageRef())},
		{"Target", emptyOrDash(ws.GetTargetSandboxFingerprint())},
		{"Dispatch", emptyOrDash(ws.GetCurrentDispatchId())},
	}
	if len(ws.GetMissing()) > 0 {
		rows = append(rows, [2]string{"Missing", strings.Join(ws.GetMissing(), ", ")})
	}
	rows = append(
		rows,
		[2]string{"Mounts", joinMounts(ws.GetMounts())},
		[2]string{"Env", joinEnv(ws.GetEnv())},
		[2]string{"Ports", joinPorts(ws.GetPorts())},
	)
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
		if _, err := fmt.Fprintf(
			tw, "%s\t%s\t%s\t%s\t%s\n",
			ws.GetName(),
			ws.GetUuid(),
			trimStatusPrefix(ws.GetStatus().String()),
			emptyOrDash(ws.GetImageRef()),
			emptyOrDash(ws.GetTargetSandboxFingerprint()),
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func trimStatusPrefix(s string) string {
	return strings.TrimPrefix(s, "WORKSPACE_STATUS_")
}

func emptyOrDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func joinMounts(mounts []*sandboxctrlv1.Mount) string {
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

func joinEnv(env []*sandboxctrlv1.EnvVar) string {
	if len(env) == 0 {
		return "—"
	}
	parts := make([]string, len(env))
	for i, e := range env {
		parts[i] = e.GetName()
	}
	return strings.Join(parts, ", ")
}

func joinPorts(ports []*sandboxctrlv1.PortMapping) string {
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
