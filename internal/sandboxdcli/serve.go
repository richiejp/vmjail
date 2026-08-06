package sandboxdcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"github.com/richiejp/darkaifactory/internal/brokerclient"
	"github.com/richiejp/darkaifactory/internal/bundled"
	"github.com/richiejp/darkaifactory/internal/cas"
	"github.com/richiejp/darkaifactory/internal/casindex"
	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/httpcache"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/imagestore"
	"github.com/richiejp/darkaifactory/internal/mitmproxy"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/nodectrlclient"
	"github.com/richiejp/darkaifactory/internal/nodejob"
	"github.com/richiejp/darkaifactory/internal/ociregistry"
	"github.com/richiejp/darkaifactory/internal/sandbox"
	"github.com/richiejp/darkaifactory/internal/sandboxboot"
	"github.com/richiejp/darkaifactory/internal/sandboxexec"
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
	"github.com/richiejp/darkaifactory/internal/sandboxjob"
	"github.com/richiejp/darkaifactory/internal/sandboxsecret"
	"github.com/richiejp/darkaifactory/internal/version"
	"github.com/richiejp/darkaifactory/internal/workspace"
	"github.com/richiejp/darkaifactory/internal/workspacedisk"
)

const serveCmdName = "serve"

// newServeCmd returns `daft-sandboxd serve`, the daemon-mode entry
// point that opens an outbound mTLS control channel to the
// orchestrator, registers itself with a host-resource
// advertisement, and emits heartbeats until SIGTERM / SIGINT.
//
// Slice A1 of the workflow reorientation (PLAN.md). Slice A4 will
// extend the same stream with Dispatch frames driving the existing
// Hello pipeline.
func newServeCmd() *cobra.Command {
	var (
		orchestratorURL   string
		configDir         string
		vsockDir          string
		heartbeatInterval time.Duration
	)
	cmd := &cobra.Command{
		Use:           serveCmdName,
		Short:         "Run daft-sandboxd in daemon mode (register with the orchestrator)",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(),
				os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return runServe(ctx, cmd.OutOrStdout(),
				orchestratorURL, configDir, vsockDir, heartbeatInterval)
		},
	}
	cmd.Flags().StringVar(&orchestratorURL, "orchestrator", "",
		"orchestrator endpoint URL (e.g. https://orchestrator.example:14443)")
	cmd.Flags().StringVar(&configDir, "config-dir", "", configDirFlagDesc)
	cmd.Flags().StringVar(&vsockDir, "vsock-dir", "",
		"parent directory for per-Sandbox hybrid-vsock UDS files; defaults under $RUNTIME_DIRECTORY, $XDG_RUNTIME_DIR, or the temporary directory")
	cmd.Flags().DurationVar(&heartbeatInterval, "heartbeat-interval",
		nodectrlclient.DefaultHeartbeatInterval,
		"how often to ping the orchestrator with a Heartbeat")
	return cmd
}

// runServe is the testable core of `daft-sandboxd serve`. ctx
// must be cancellable — runServe blocks for the lifetime of the
// daemon and exits cleanly when ctx fires.
//
//nolint:funlen // Daemon construction keeps resource ownership and cleanup ordering visible in one lifecycle.
func runServe(
	ctx context.Context,
	stdout io.Writer,
	orchestratorURL, configDirOverride, vsockDirOverride string,
	heartbeatInterval time.Duration,
) error {
	if orchestratorURL == "" {
		return errors.New("daft-sandboxd serve: --orchestrator is required")
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

	resources, err := nodectrlclient.DetectResources()
	if err != nil {
		return err
	}

	vsockDir, cleanupVsockDir, err := resolveVsockDir(vsockDirOverride)
	if err != nil {
		return err
	}
	defer cleanupVsockDir()

	sync, err := buildNodeSync(dir, orchestratorURL, id, peers)
	if err != nil {
		return err
	}
	defer func() { _ = sync.close() }()
	syncCtx, cancelSync := context.WithCancel(ctx)
	defer cancelSync()

	bridger := sandboxgateway.New()
	attachRegistry := sandboxgateway.NewAttachRegistry()
	jobRegistry := sandboxgateway.NewJobRegistry()
	observerRegistry := sandboxgateway.NewObserverRegistry()
	summaryRegistry := sandboxgateway.NewSummaryRegistry()
	mountBackingDir := filepath.Join(dir, "mounts")
	diskStore, err := workspacedisk.OpenStore(filepath.Join(dir, "workspace-disks"), mountBackingDir)
	if err != nil {
		return err
	}
	hostedRT := hostedRegistryTransport(orchestratorURL, id, peers)
	dispatcher, err := buildDispatcher(stdout, bridger, attachRegistry, jobRegistry,
		observerRegistry, summaryRegistry, vsockDir, sync.server, mountBackingDir, diskStore, hostedRT)
	if err != nil {
		return err
	}
	sync.server.SetWorkspaceDiskController(&nodeWorkspaceDiskControl{executor: dispatcher, store: diskStore})
	jobManager, err := newJobManager(ctx, jobRegistry)
	if err != nil {
		return err
	}
	secretDeliverer, err := sandboxsecret.New(jobRegistry.Dial)
	if err != nil {
		return err
	}

	client, err := newServeNodeClient(orchestratorURL, id, peers, heartbeatInterval,
		dispatcher, func(snapshotContext context.Context) []*nodectrlv1.WorkspaceStorageReport {
			return workspaceStorageSnapshot(snapshotContext, dispatcher, sync.server)
		}, bridger, attachRegistry, observerRegistry, summaryRegistry, jobManager, secretDeliverer, stdout)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(stdout,
		"daft-sandboxd serve: registering with %s\n"+
			"daft-sandboxd serve: fingerprint %s\n"+
			"daft-sandboxd serve: advertising %d CPUs, %s\n"+
			"daft-sandboxd serve: workspace sync cas %s\n",
		orchestratorURL, id.Fingerprint(),
		resources.CPUs, formatBytes(resources.MemoryBytes),
		sync.casRoot)

	// The node hosts its CAS-backed WorkspaceService over the broker
	// relay for the daemon's lifetime, alongside the nodectrl control
	// channel. It's a passive sandbox registration; the user-host
	// client (the serve owner / initiator) drives push/pull. Run it
	// concurrently with RegisterAndServe; ctx cancel stops both.
	return runNodeServices(ctx, syncCtx, sync, client, resources, stdout, cancelSync)
}

type nodeWorkspaceDiskControl struct {
	executor workspaceDiskExecutor
	store    *workspacedisk.Store
}

type workspaceDiskExecutor interface {
	WorkspaceDiskStatus(string) (sandboxexec.WorkspaceDiskStatus, error)
	ResizeWorkspaceDisk(context.Context, string, uint64) error
	UnfreezeWorkspaceDisk(context.Context, string) error
}

func (c *nodeWorkspaceDiskControl) Status(workspaceID string) (workspace.WorkspaceDiskStatus, error) {
	status, err := c.executor.WorkspaceDiskStatus(workspaceID)
	if err == nil {
		return workspace.WorkspaceDiskStatus{
			UsedBytes: status.Usage.Used, AvailableBytes: status.Usage.Available,
			AllowanceBytes: status.Allowance, Frozen: status.Frozen, Running: true,
		}, nil
	}
	if !errors.Is(err, sandboxexec.ErrWorkspaceDiskNotRunning) {
		return workspace.WorkspaceDiskStatus{}, err
	}
	size, _, sizeErr := c.store.Size(workspaceID)
	if sizeErr != nil {
		return workspace.WorkspaceDiskStatus{}, sizeErr
	}
	return workspace.WorkspaceDiskStatus{AllowanceBytes: size, Running: false}, nil
}

func (c *nodeWorkspaceDiskControl) Unfreeze(ctx context.Context, workspaceID string) error {
	return c.executor.UnfreezeWorkspaceDisk(ctx, workspaceID)
}

func (c *nodeWorkspaceDiskControl) Resize(ctx context.Context, workspaceID string, desired uint64) error {
	if _, err := c.executor.WorkspaceDiskStatus(workspaceID); err == nil {
		return c.executor.ResizeWorkspaceDisk(ctx, workspaceID, desired)
	} else if !errors.Is(err, sandboxexec.ErrWorkspaceDiskNotRunning) {
		return err
	}
	current, exists, err := c.store.Size(workspaceID)
	if err != nil {
		return err
	}
	if !exists {
		return fs.ErrNotExist
	}
	if desired <= current {
		return fmt.Errorf("workspace disk resize must increase allowance: current=%d requested=%d", current, desired)
	}
	return c.store.Grow(workspaceID, desired)
}

func (c *nodeWorkspaceDiskControl) Delete(_ context.Context, workspaceID string) error {
	if _, err := c.executor.WorkspaceDiskStatus(workspaceID); err == nil {
		return errors.New("daft-sandboxd: cannot delete a running workspace disk")
	} else if !errors.Is(err, sandboxexec.ErrWorkspaceDiskNotRunning) {
		return err
	}
	return c.store.Delete(workspaceID)
}

func runNodeServices(
	ctx, syncCtx context.Context,
	sync *nodeSync,
	client *nodectrlclient.Client,
	resources nodectrl.Resources,
	stdout io.Writer,
	cancelSync context.CancelFunc,
) error {
	syncDone := make(chan struct{})
	go func() {
		defer close(syncDone)
		sync.run(syncCtx, stdout)
	}()
	runErr := runRegisterAndServe(ctx, client, resources)
	cancelSync()
	<-syncDone
	return runErr
}

func newJobManager(ctx context.Context, registry *sandboxgateway.JobRegistry) (*nodejob.Manager, error) {
	runner, err := sandboxjob.NewRunner(registry.Dial)
	if err != nil {
		return nil, err
	}
	return nodejob.New(ctx, runner, nodejob.Limits{})
}

func newServeNodeClient(
	orchestratorURL string,
	id *identity.Identity,
	peers *identity.PeerStore,
	heartbeatInterval time.Duration,
	dispatcher *sandboxexec.Executor,
	workspaceStorage func(context.Context) []*nodectrlv1.WorkspaceStorageReport,
	bridger *sandboxgateway.Bridger,
	attachRegistry *sandboxgateway.AttachRegistry,
	observerRegistry *sandboxgateway.ObserverRegistry,
	summaryRegistry *sandboxgateway.SummaryRegistry,
	jobManager *nodejob.Manager,
	secretDeliverer *sandboxsecret.Deliverer,
	stderr io.Writer,
) (*nodectrlclient.Client, error) {
	return nodectrlclient.New(nodectrlclient.Config{
		URL: orchestratorURL, Identity: id, Peers: peers, Version: version.Version, Hostname: detectHostname(),
		HeartbeatInterval: heartbeatInterval, SandboxEvents: dispatcher, LiveSnapshot: dispatcher.LiveSnapshot,
		WorkspaceStorageSnapshot: workspaceStorage,
		Bridger:                  bridger, AttachBridger: attachRegistry, ObserverResolver: observerRegistry,
		SummaryResolver: summaryRegistry, JobManager: jobManager, SecretDeliverer: secretDeliverer, Stderr: stderr,
	})
}

type workspaceDiskSnapshotter interface {
	WorkspaceDiskStatuses() []sandboxexec.WorkspaceDiskStatus
}

func workspaceStorageSnapshot(ctx context.Context, executor workspaceDiskSnapshotter, server *workspace.Server) []*nodectrlv1.WorkspaceStorageReport {
	if executor == nil || server == nil {
		return nil
	}
	statuses := executor.WorkspaceDiskStatuses()
	reports := make([]*nodectrlv1.WorkspaceStorageReport, 0, len(statuses))
	for _, status := range statuses {
		response, err := server.GetWorkspaceDiskStatus(ctx, connect.NewRequest(
			&workspacev1.GetWorkspaceDiskStatusRequest{WorkspaceUuid: status.WorkspaceID},
		))
		if err != nil {
			continue
		}
		var digest string
		var generation uint64
		for _, manifest := range response.Msg.GetCurrentManifests() {
			if manifest.GetGeneration() > generation {
				digest, generation = manifest.GetManifestDigest(), manifest.GetGeneration()
			}
		}
		reports = append(reports, &nodectrlv1.WorkspaceStorageReport{
			WorkspaceUuid: status.WorkspaceID, UsedBytes: response.Msg.GetUsedBytes(),
			AvailableBytes: response.Msg.GetAvailableBytes(), AllowanceBytes: response.Msg.GetAllowanceBytes(),
			ProtectedBytes: response.Msg.GetProtectedBytes(), ManifestDigest: digest,
			ManifestGeneration: generation, Frozen: response.Msg.GetFrozen(),
		})
	}
	return reports
}

// nodeSync bundles the node's persistent CAS-backed WorkspaceService
// handler and the broker client that hosts it over the orchestrator
// relay. sandboxID is the broker key the user-host names to reach it
// — the node's own fingerprint string, so the workspace's target
// fingerprint doubles as the sync rendezvous key.
type nodeSync struct {
	bc        *brokerclient.Client
	handler   http.Handler
	sandboxID string
	casRoot   string
	// server is the CAS-backed WorkspaceService the broker hosts. It's
	// shared with the dispatch executor so launch can materialise pushed
	// mounts straight from the same persistent CAS (Slice S3).
	server *workspace.Server
	index  *casindex.Index
}

// buildNodeSync opens the persistent node CAS under <dir>/cas and
// wires a WorkspaceService handler over it plus a broker client to
// the orchestrator. The CAS persists across daemon restarts, so a
// re-pushed mostly-unchanged tree only moves its delta.
//
//nolint:gochecknoglobals // single test seam, not state.
var buildNodeSync = func(
	dir, orchestratorURL string,
	id *identity.Identity,
	peers *identity.PeerStore,
) (*nodeSync, error) {
	store, err := cas.Open(filepath.Join(dir, "cas"))
	if err != nil {
		return nil, fmt.Errorf("daft-sandboxd serve: open sync cas: %w", err)
	}
	index, err := casindex.Open(store.Root(), nil)
	if err != nil {
		return nil, fmt.Errorf("daft-sandboxd serve: open sync CAS index: %w", err)
	}
	wsServer, err := workspace.NewIndexedServer(store, index)
	if err != nil {
		_ = index.Close()
		return nil, fmt.Errorf("daft-sandboxd serve: workspace server: %w", err)
	}
	mux := http.NewServeMux()
	path, handler := workspacev1connect.NewWorkspaceServiceHandler(wsServer)
	mux.Handle(path, handler)
	bc, err := brokerclient.New(brokerclient.Config{
		URL: orchestratorURL, Identity: id, Peers: peers,
	})
	if err != nil {
		_ = index.Close()
		return nil, fmt.Errorf("daft-sandboxd serve: broker client: %w", err)
	}
	return &nodeSync{
		bc:        bc,
		handler:   mux,
		sandboxID: id.Fingerprint().String(),
		casRoot:   store.Root(),
		server:    wsServer,
		index:     index,
	}, nil
}

// run hosts the WorkspaceService over the broker until ctx is
// cancelled. A serve-loop exit that isn't a ctx cancel (e.g. the
// orchestrator stream dropped) is logged; the daemon keeps running on
// its nodectrl channel and sync resumes on the next daemon start.
func (n *nodeSync) run(ctx context.Context, logw io.Writer) {
	go func() {
		if err := n.server.RunCleanup(ctx); err != nil && ctx.Err() == nil {
			_, _ = fmt.Fprintf(logw, "daft-sandboxd serve: CAS cleanup stopped: %v\n", err)
		}
	}()
	err := n.bc.ServeBrokeredAsSandbox(ctx, n.sandboxID, brokerclient.ServeOptions{
		Handler: n.handler,
	})
	if err != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(logw, "daft-sandboxd serve: workspace sync stopped: %v\n", err)
	}
}

func (n *nodeSync) close() error {
	if n == nil || n.index == nil {
		return nil
	}
	return n.index.Close()
}

// runRegisterAndServe is a single test seam — production swaps in
// the real client.RegisterAndServe, tests can swap a fake.
//
//nolint:gochecknoglobals // single test seam, not state.
var runRegisterAndServe = func(
	ctx context.Context,
	c *nodectrlclient.Client,
	res nodectrl.Resources,
) error {
	return c.RegisterAndServe(ctx, res)
}

// buildDispatcher resolves the sandbox.Config baseline from the
// DAFT_* env vars and wires an Executor with it. The Executor is
// the DispatchHandler the nodectrlclient invokes on every
// Dispatch event from the orchestrator. The production Assembler
// pulls + bundles images via the local imagestore so dispatches
// carrying a spec.image_ref actually run a container.
//
// bridger and vsockDir are the C1.3b port-ingress wiring: the
// Executor registers per-(workspace, port) DialGuestVsock closures
// on bridger as dispatches arrive, and the same bridger is shared
// with the nodectrlclient so its OpenPortStream handler finds them.
// attachRegistry is the D2b.3 counterpart for AttachStream — the
// same instance is shared with the nodectrlclient so AttachRequest
// events route through to the executor-populated dial closures.
//
//nolint:gochecknoglobals // single test seam, not state.
var buildDispatcher = func(
	stdout io.Writer,
	bridger *sandboxgateway.Bridger,
	attachRegistry *sandboxgateway.AttachRegistry,
	jobRegistry *sandboxgateway.JobRegistry,
	observerRegistry *sandboxgateway.ObserverRegistry,
	summaryRegistry *sandboxgateway.SummaryRegistry,
	vsockDir string,
	materializer sandboxexec.MountMaterializer,
	mountBackingDir string,
	workspaceDisks sandboxexec.WorkspaceDiskPreparer,
	hostedRT http.RoundTripper,
) (*sandboxexec.Executor, error) {
	// A bundled binary (Nix build) carries the artefacts embedded, so
	// the DAFT_KERNEL/INITRAMFS/CLOUD_HYPERVISOR paths are not required —
	// they are read only as an override. A plain `go build` daft-sandboxd
	// has stub assets and must fall back to env-supplied paths, so require
	// them there. extractWithEnvFallback resolves the same precedence at
	// dispatch (bundled assets win; env paths are the no-bundle fallback).
	base, err := sandbox.ConfigFromEnv(!bundled.IsBundled())
	if err != nil {
		return nil, err
	}
	store, err := openSandboxbootStore(hostedRT)
	if err != nil {
		return nil, err
	}
	if err := store.SweepOrphanClones(); err != nil {
		return nil, err
	}
	assembler := sandboxexec.ProductionAssembler(sandboxboot.Deps{
		OpenStore: func() (sandboxboot.Store, error) { return store, nil },
		CacheDir:  userCacheDir,
		Extract:   extractWithEnvFallback(base),
	})
	egress, err := egressProxyFromEnv(stdout)
	if err != nil {
		return nil, err
	}
	// Default every dispatched VM's observer log under the per-sandbox
	// state dir, keyed by the orchestrator-assigned uid so
	// `observer-tail` can find it. Best-effort: a state-dir resolution
	// failure leaves ObserverDir empty (no per-VM observer) rather than
	// refusing to build the dispatcher — observability never gates the
	// daemon coming up.
	observerDir, err := sandboxesStateRoot()
	if err != nil {
		_, _ = fmt.Fprintf(stdout, "daft-sandboxd serve: observer logs disabled: %v\n", err)
		observerDir = ""
	}
	return sandboxexec.New(sandboxexec.Config{
		Base:               base,
		Stdout:             stdout,
		Assembler:          assembler,
		Bridger:            bridger,
		AttachRegistry:     attachRegistry,
		JobRegistry:        jobRegistry,
		ObserverRegistry:   observerRegistry,
		SummaryRegistry:    summaryRegistry,
		ObserveSummary:     observeSummaryFromEnv(),
		VsockDir:           vsockDir,
		NetEnabled:         true,
		EgressProxy:        egress,
		MountMaterializer:  materializer,
		MountBackingDir:    mountBackingDir,
		WorkspaceDisks:     workspaceDisks,
		ObserverDir:        observerDir,
		BuildImageRef:      resolveBuildImageRef(hostedRT),
		BuildPushTransport: hostedRT,
	})
}

// envBuildImageRef names the orchestrator-hosted ref of the static image
// builder VM (buildah + dash + a CA bundle). A build-based devcontainer is
// built in an ephemeral sandbox from this ref before the workload boots
// (#101 slice 3c). It is OPTIONAL: unset, the node defaults to the
// orchestrator's auto-seeded built-in builder (ociregistry.BuiltinBuildRef);
// set it only to override with a custom builder image.
const envBuildImageRef = "DAFT_BUILD_IMAGE_REF"

// resolveBuildImageRef resolves the build image ref: the
// DAFT_BUILD_IMAGE_REF override when set, else the orchestrator's
// auto-seeded built-in builder. The built-in default is a hosted ref
// (sentinel host) pulled over the same mTLS transport as any workspace
// image, so it is only meaningful when that transport exists — without it
// we leave the ref empty (build path off, as before) rather than hand
// sandboxexec a hosted ref it can't reach. On a non-bundled orchestrator
// the seed is simply absent and the pull fails loudly at build dispatch.
func resolveBuildImageRef(hostedRT http.RoundTripper) string {
	if ref := os.Getenv(envBuildImageRef); ref != "" {
		return ref
	}
	if hostedRT == nil {
		return ""
	}
	return ociregistry.BuiltinBuildRef()
}

// envObserveSummary gates the Tier-2 observe share (open-snoop gadget +
// build-progress summary) on every image-backed dispatch. It is ON BY
// DEFAULT so the `daft sandbox observe` progress summary works out of the
// box; an operator who does not want the gadget load + extra virtiofs
// device per VM sets it to a falsy value ("0"/"false"/"no"/"off",
// case-insensitive) to opt a node out.
const envObserveSummary = "DAFT_OBSERVE_SUMMARY"

func observeSummaryFromEnv() bool {
	return envEnabledByDefault(envObserveSummary)
}

// Env vars that configure the in-stack egress proxy (Phase J6). Set by
// the node operator (and by the test cluster's WithEgressProxy option).
// The proxy itself runs in-process inside sandboxd, served on each VM's
// netstack gateway — there is no separate process or proxy URL/CA path
// to distribute; sandboxd generates the CA and injects it into bundles.
const (
	// envEgressProxy gates the in-stack proxy. It is ON BY DEFAULT
	// (Phase J hermetic egress): unset/empty enables it, so a stock
	// `daft-sandboxd serve` runs default-deny with the proxy lane. Set
	// it to a falsy value ("0"/"false"/"no"/"off") to opt a node out
	// and fall back to direct allow-all egress.
	envEgressProxy = "DAFT_EGRESS_PROXY"
	// envEgressProxyCacheDir enables record/replay caching at this dir
	// (empty = pure pass-through).
	envEgressProxyCacheDir = "DAFT_EGRESS_PROXY_CACHE_DIR"
	// envEgressProxyCADir persists the proxy CA cert+key here so the
	// trust anchor survives restarts (empty = fresh ephemeral CA).
	envEgressProxyCADir = "DAFT_EGRESS_PROXY_CA_DIR"
	// envEgressProxyOffline serves only from the cache; a miss 504s
	// instead of reaching upstream.
	envEgressProxyOffline = "DAFT_EGRESS_PROXY_OFFLINE"
	// envEgressProxyInsecureUpstream skips upstream TLS verification —
	// for tests whose upstream is a self-signed peer.
	envEgressProxyInsecureUpstream = "DAFT_EGRESS_PROXY_INSECURE_UPSTREAM"
)

// egressProxyFromEnv builds the executor's in-stack EgressProxy from
// the DAFT_EGRESS_PROXY* env vars. The proxy is ON BY DEFAULT; it
// returns nil (feature off) only when DAFT_EGRESS_PROXY is explicitly
// falsy. sandboxd owns the proxy: it mints/loads the CA, optionally
// opens the record/replay cache, and hands the resulting http.Handler +
// CA PEM to the executor, which serves the handler on every VM's
// gateway and injects the matching https_proxy + CA. logSink receives
// the proxy's per-request log lines.
func egressProxyFromEnv(logSink io.Writer) (*sandboxexec.EgressProxy, error) {
	if !egressProxyEnabled() {
		return nil, nil //nolint:nilnil // nil config = feature off, not an error.
	}
	caDir, err := absoluteEnvDir(envEgressProxyCADir)
	if err != nil {
		return nil, err
	}
	cacheDir, err := absoluteEnvDir(envEgressProxyCacheDir)
	if err != nil {
		return nil, err
	}
	certs, err := mitmproxy.LoadOrCreateCertCache(caDir)
	if err != nil {
		return nil, fmt.Errorf("sandboxd: egress proxy CA: %w", err)
	}
	proxy := mitmproxy.New(certs, envTruthy(os.Getenv(envEgressProxyInsecureUpstream)), logSink)
	if cacheDir != "" {
		cache, cerr := httpcache.Open(cacheDir, envTruthy(os.Getenv(envEgressProxyOffline)), 0)
		if cerr != nil {
			return nil, fmt.Errorf("sandboxd: egress proxy cache: %w", cerr)
		}
		proxy.SetCache(cache)
	}
	return &sandboxexec.EgressProxy{Handler: proxy, CACertPEM: certs.CACertPEM()}, nil
}

func absoluteEnvDir(name string) (string, error) {
	dir := os.Getenv(name)
	if dir != "" && !filepath.IsAbs(dir) {
		return "", fmt.Errorf("sandboxd: %s %q must be absolute", name, dir)
	}
	return dir, nil
}

// egressProxyEnabled reports whether the in-stack proxy is on. It is on
// by default — only an explicit falsy DAFT_EGRESS_PROXY ("0"/"false"/
// "no"/"off", case-insensitive) opts a node out.
func egressProxyEnabled() bool {
	return envEnabledByDefault(envEgressProxy)
}

// envEnabledByDefault reports whether the env var named key is on. On by
// default; only an explicit falsy value ("0"/"false"/"no"/"off",
// case-insensitive) returns false. The dual of envTruthy, for the
// opt-out flags (egress proxy, observe summary).
func envEnabledByDefault(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// envTruthy reports whether an env value means "on": "1" or "true"
// (case-insensitive). Everything else, including empty, is off. Used
// for the optional sub-flags that default off (cache offline, insecure
// upstream).
func envTruthy(v string) bool {
	return v == "1" || strings.EqualFold(v, "true")
}

// extractWithEnvFallback wraps bundled.Extract: when the binary
// has the artefacts embedded (Nix build) it returns those;
// otherwise (plain `go build`) it falls back to the artefact
// paths sandbox.ConfigFromEnv resolved into base. This is the
// "callers should fall back to env-var-supplied paths" pattern
// the bundled package's doc comment specifies. Returns the
// embed error untouched when neither source is available, so a
// genuinely misconfigured daemon still produces a clean
// ErrNotBundled at dispatch time.
func extractWithEnvFallback(base sandbox.Config) func(dir string) (bundled.Paths, error) {
	return func(dir string) (bundled.Paths, error) {
		paths, err := bundled.Extract(dir)
		if !errors.Is(err, bundled.ErrNotBundled) {
			return paths, err
		}
		envPaths := bundled.Paths{
			CloudHypervisor: base.CloudHypervisor,
			Kernel:          base.Kernel,
			Initramfs:       base.Initramfs,
			Virtiofsd:       base.Virtiofsd,
		}
		if envPaths.CloudHypervisor == "" || envPaths.Kernel == "" ||
			envPaths.Initramfs == "" || envPaths.Virtiofsd == "" {
			return bundled.Paths{}, err
		}
		return envPaths, nil
	}
}

// detectHostname returns the OS hostname for node registration, or ""
// if the host can't report one. Best-effort: the fingerprint is the
// canonical node id, so a missing hostname only costs the friendly
// `daft workspace target <hostname>` alias, never correctness.
func detectHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// resolveVsockDir returns the absolute, mkdir'd parent directory for
// per-Sandbox hybrid-vsock UDS files and a cleanup function. An explicit
// override must already be absolute and remains caller-owned. Otherwise a
// short application directory is selected below systemd's RUNTIME_DIRECTORY
// or the user's XDG_RUNTIME_DIR. The unmanaged os.TempDir fallback is private
// to this process and removed at clean shutdown. Socket listeners pre-clean
// their exact stale paths before binding.
func resolveVsockDir(override string) (dir string, cleanup func(), err error) {
	dir = override
	owned := false
	if dir == "" {
		dir, owned = defaultVsockDir()
	}
	if !filepath.IsAbs(dir) {
		return "", nil, fmt.Errorf("daft-sandboxd serve: --vsock-dir %q must be absolute", dir)
	}
	if err := validateVsockDirLength(dir); err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("daft-sandboxd serve: mkdir vsock dir: %w", err)
	}
	cleanup = func() {}
	if owned {
		cleanup = func() {
			// The directory contains only daemon-owned Unix sockets. Removal is
			// best-effort during shutdown; startup always selects a fresh path.
			_ = os.RemoveAll(dir)
		}
	}
	return dir, cleanup, nil
}

func validateVsockDirLength(dir string) error {
	// Linux sockaddr_un.sun_path is 108 bytes including its terminating NUL.
	// The longest current listener path is a 36-byte Sandbox UUID plus the
	// devcontainer-build and hybrid-vsock port suffixes.
	const maxUnixPathBytes = 107
	const longestSocketLeaf = "00000000-0000-0000-0000-000000000000-build.sock_7000"
	path := filepath.Join(dir, longestSocketLeaf)
	if len(path) > maxUnixPathBytes {
		return fmt.Errorf("daft-sandboxd serve: vsock directory %q leaves no room for Unix socket names (%d-byte path exceeds %d)",
			dir, len(path), maxUnixPathBytes)
	}
	return nil
}

func defaultVsockDir() (string, bool) {
	if dir := firstAbsolutePath(os.Getenv("RUNTIME_DIRECTORY")); dir != "" {
		return filepath.Join(dir, "vsock"), false
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "daft-vsock"), false
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("daft-vsock-%d", os.Getpid())), true
}

// firstAbsolutePath selects the first absolute entry from a systemd
// RUNTIME_DIRECTORY value, which may contain a colon-separated list when a
// unit declares more than one RuntimeDirectory.
func firstAbsolutePath(value string) string {
	for _, dir := range filepath.SplitList(value) {
		if filepath.IsAbs(dir) {
			return dir
		}
	}
	return ""
}

// openSandboxbootStore opens the same production imagestore the legacy `run`
// command uses. The concrete type is retained so daemon startup can sweep
// orphan clones before exposing it to the executor through sandboxboot.Store.
// When hostedRT is non-nil it is the mTLS transport for orchestrator-hosted
// image refs.
func openSandboxbootStore(hostedRT http.RoundTripper) (*imagestore.Store, error) {
	st, err := openStore()
	if err != nil {
		return nil, err
	}
	if hostedRT != nil {
		st.UseHostedRegistry(hostedRT)
	}
	return st, nil
}

// hostedRegistryTransport builds the mTLS transport the imagestore uses
// to pull image refs hosted on the orchestrator's embedded registry: it
// carries the sandbox-host client cert and redirects the dial to the
// orchestrator's address (see internal/ociregistry). Returns nil — which
// disables the hosted-pull path — on the (practically unreachable) errors
// from parsing the already-validated orchestrator URL or building the
// client TLS config; the same ClientTLSConfig is rebuilt immediately
// after by nodectrlclient.New, which surfaces any real failure there.
func hostedRegistryTransport(orchestratorURL string, id *identity.Identity, peers *identity.PeerStore) http.RoundTripper {
	u, err := url.Parse(orchestratorURL)
	if err != nil {
		return nil
	}
	tlsCfg, err := identity.ClientTLSConfig(id, peers, "")
	if err != nil {
		return nil
	}
	return ociregistry.Transport(u.Host, tlsCfg)
}

// formatBytes is a small KiB/MiB/GiB renderer for the startup
// banner. Production traffic doesn't read this output, so a
// best-effort, three-significant-digit rendering is fine.
func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	suffix := "KMGTPE"[exp]
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), suffix)
}
