package sandboxdcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/net/http2"

	"github.com/richiejp/darkaifactory/internal/brokerclient"
	"github.com/richiejp/darkaifactory/internal/bundled"
	"github.com/richiejp/darkaifactory/internal/cas"
	"github.com/richiejp/darkaifactory/internal/component"
	"github.com/richiejp/darkaifactory/internal/daftfuse"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/imagestore"
	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/sandbox"
	"github.com/richiejp/darkaifactory/internal/trace"
	"github.com/richiejp/darkaifactory/internal/workspace"
)

// In-VM mountpoints used by the run command. Stable so the bundle
// path the agent sees from the cmdline matches what virtiofs
// actually mounts.
const (
	bundlesMount   = "/bundles"
	workspaceMount = "/workspace"
	bundlesTag     = "bundles"
	workspaceTag   = "workspace"
)

func newRunCmd() *cobra.Command {
	opts := runOptions{}
	cmd := &cobra.Command{
		Use:   "run <image-ref> [cmd...]",
		Short: "Pull an image and run it as a container in a sandbox VM",
		Long: "Pull <image-ref> into the local store (LRU-evicting under quota),\n" +
			"unpack it into a bundle, share the bundles directory and the\n" +
			"workspace (when --workspace is given) into the VM via virtiofs,\n" +
			"and crun-run the container. The container's stdout+stderr are\n" +
			"forwarded to this terminal with `container: <…>` prefix.\n\n" +
			"--workspace mounts the given host directory at /workspace inside\n" +
			"the container as a read-write virtiofs share — files written by\n" +
			"the container persist on the host.\n\n" +
			"--workspace-from + --workspace-name fetch a workspace from a\n" +
			"running `daft workspace serve` (typically the user-host) into a\n" +
			"sandbox-host tmpdir before boot, then mount that tmpdir at\n" +
			"/workspace just like the local --workspace flag. mTLS identity\n" +
			"is bootstrapped under $XDG_CONFIG_HOME/daft/sandboxd; the\n" +
			"orchestrator/user-host must already have approved this peer's\n" +
			"fingerprint, and vice-versa.",
		Args:          cobra.MinimumNArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.ref = args[0]
			opts.cmdArgs = args[1:]
			return runRun(cmd.Context(), cmd.OutOrStdout(), opts, defaultRunDepsFn())
		},
	}
	cmd.Flags().StringVarP(&opts.workspace, "workspace", "w", "",
		"host directory to mount read-write at /workspace inside the container")
	cmd.Flags().StringVar(&opts.workspaceFrom, "workspace-from", "",
		"address (host:port) of a daft workspace serve to pull from (direct-dial mode)")
	cmd.Flags().StringVar(&opts.workspaceName, "workspace-name", "",
		"workspace name to fetch from --workspace-from or --workspace-broker")
	cmd.Flags().StringVar(&opts.workspaceBroker, "workspace-broker", "",
		"orchestrator URL (https://host:port) to register this sandbox with for a brokered workspace attach (NAT-friendly alternative to --workspace-from)")
	cmd.Flags().StringVar(&opts.workspaceServeFP, "workspace-serve-fingerprint", "",
		"required with --workspace-broker: the user-host's leaf-cert fingerprint")
	cmd.Flags().StringVar(&opts.workspaceSandboxID, "workspace-sandbox-id", "",
		"required with --workspace-broker: sandbox identifier the user-host's `daft workspace attach` will target")
	cmd.Flags().StringVar(&opts.configDir, "config-dir", "",
		"override the daft-sandboxd config dir (default: $XDG_CONFIG_HOME/daft/sandboxd)")
	cmd.Flags().BoolVar(&opts.keepWorkspace, "keep-workspace", false,
		"after the VM exits, leave the workspace tmpdir on disk and print its path "+
			"so `daft-sandboxd workspace serve --path=<dir>` can expose it for pull-back")
	cmd.Flags().BoolVar(&opts.workspaceAttach, "workspace-attach", false,
		"mount the workspace lazily via daft-fuse instead of pre-fetching all chunks. "+
			"Requires --workspace-from or --workspace-broker; chunks fetch on first "+
			"read. Mutex with --workspace and --keep-workspace.")
	return cmd
}

// runOptions consolidates the run command's flags. Hoisted so adding
// a flag is one struct field plus one Flags() call.
type runOptions struct {
	ref                string
	cmdArgs            []string
	workspace          string
	workspaceFrom      string
	workspaceName      string
	workspaceBroker    string
	workspaceServeFP   string
	workspaceSandboxID string
	configDir          string
	keepWorkspace      bool
	workspaceAttach    bool
}

// runDeps bundles the moving parts of `run` so unit tests can swap
// in fakes for the network-talking and VM-spawning bits.
type runDeps struct {
	openStore             func() (storeAPI, error)
	cacheDir              func() (string, error)
	extract               func(string) (bundled.Paths, error)
	boot                  func(context.Context, io.Writer, sandbox.Config) error
	pullWorkspaceFrom     func(ctx context.Context, opts pullWorkspaceOpts) (dir string, cleanup func(), err error)
	pullWorkspaceBroker   func(ctx context.Context, opts pullWorkspaceBrokerOpts) (dir string, cleanup func(), err error)
	attachWorkspaceFrom   func(ctx context.Context, opts pullWorkspaceOpts) (dir string, cleanup func(), err error)
	attachWorkspaceBroker func(ctx context.Context, opts pullWorkspaceBrokerOpts) (dir string, cleanup func(), err error)
}

// pullWorkspaceOpts gathers the inputs to pullWorkspaceFrom so the
// dep signature stays stable as we add fields.
type pullWorkspaceOpts struct {
	addr      string
	name      string
	configDir string
}

// pullWorkspaceBrokerOpts collects the inputs to the brokered
// pull path so the dep signature stays stable.
type pullWorkspaceBrokerOpts struct {
	orchestratorURL string
	workspace       string
	sandboxID       string
	serveFP         string
	configDir       string
}

// storeAPI is the surface of imagestore.Store the run command uses.
// Defining it as an interface keeps run-side tests from spinning up
// a real registry just to assert wiring choices.
type storeAPI interface {
	Pull(ctx context.Context, ref string) (*imagestore.Image, error)
	Bundle(ctx context.Context, digest string, opts imagestore.BundleOptions) (*imagestore.Bundle, error)
	Lock(digest string) (func(), error)
}

// defaultRunDepsFn is the production-deps factory invoked by
// newRunCmd's RunE. Hoisted to a var so tests can swap it for a
// canned set of fakes and exercise the full cobra → runRun wiring
// without spinning up an image registry.
var defaultRunDepsFn = defaultRunDeps

func defaultRunDeps() runDeps {
	return runDeps{
		openStore:             func() (storeAPI, error) { return openStore() },
		cacheDir:              userCacheDir,
		extract:               bundled.Extract,
		boot:                  sandbox.Hello,
		pullWorkspaceFrom:     pullWorkspaceFrom,
		pullWorkspaceBroker:   pullWorkspaceBroker,
		attachWorkspaceFrom:   attachLazyWorkspaceFrom,
		attachWorkspaceBroker: attachLazyWorkspaceBroker,
	}
}

// sandboxdContext bundles the per-config-dir bits both pull paths
// need: identity, peer store, CAS store, and the resolved
// configDir itself (for diagnostics).
type sandboxdContext struct {
	configDir string
	id        *identity.Identity
	peers     *identity.PeerStore
	store     *cas.Store
}

// loadSandboxdContext resolves the sandboxd config dir (XDG-aware
// when override is empty) and opens the identity + peer store +
// CAS store under it.
func loadSandboxdContext(override string) (*sandboxdContext, error) {
	configDir, err := resolveSandboxdDir(override)
	if err != nil {
		return nil, err
	}
	id, err := loadOrGenerateSandboxd(configDir)
	if err != nil {
		return nil, err
	}
	peers, err := openSandboxdPeerStore(configDir)
	if err != nil {
		return nil, err
	}
	store, err := cas.Open(filepath.Join(configDir, "cas"))
	if err != nil {
		return nil, err
	}
	return &sandboxdContext{configDir: configDir, id: id, peers: peers, store: store}, nil
}

// fetchAndApplyManifest performs the GetManifest → MkdirTemp →
// ApplyManifest tail shared by both pull-path implementations.
// On error before MkdirTemp succeeds, no temp dir leaks. On any
// error after, the temp dir is removed before returning.
func fetchAndApplyManifest(ctx context.Context, client workspacev1connect.WorkspaceServiceClient, store *cas.Store, workspaceName string) (dir string, cleanup func(), err error) {
	resp, err := client.GetManifest(ctx,
		connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: workspaceName}))
	if err != nil {
		return "", nil, fmt.Errorf("daft-sandboxd: GetManifest: %w", err)
	}
	dest, err := os.MkdirTemp("", "daft-sandboxd-workspace-")
	if err != nil {
		return "", nil, fmt.Errorf("daft-sandboxd: workspace tmpdir: %w", err)
	}
	if err := workspace.ApplyManifest(ctx, client, store, resp.Msg.GetManifest(), dest,
		workspace.ApplyOptions{}); err != nil {
		_ = os.RemoveAll(dest)
		return "", nil, err
	}
	return dest, func() {
		oops.Log("daft-sandboxd: cleanup workspace tmpdir", os.RemoveAll(dest))
	}, nil
}

// pullWorkspaceFrom dials a daft workspace serve at opts.addr over
// mTLS, fetches opts.name's manifest + chunks via ApplyManifest into
// a fresh tmpdir, and returns that dir plus a cleanup. The tmpdir is
// what the caller wires as the /workspace virtiofs share.
func pullWorkspaceFrom(ctx context.Context, opts pullWorkspaceOpts) (dir string, cleanup func(), err error) {
	wc, err := openDirectWorkspaceClient(ctx, opts.addr, opts.configDir)
	if err != nil {
		return "", nil, err
	}
	defer wc.close()
	return fetchAndApplyManifest(ctx, wc.rpc, wc.store, opts.name)
}

// workspaceClient bundles a WorkspaceService client (over either
// direct mTLS or a brokered relay) with the cas.Store backing the
// local chunk cache and a closer that tears down the underlying
// transport. The pull path closes it after ApplyManifest; the
// lazy-attach path keeps it alive past the daftfuse mount.
type workspaceClient struct {
	rpc   workspacev1connect.WorkspaceServiceClient
	store *cas.Store
	close func()
}

// openDirectWorkspaceClient builds a direct-dial mTLS transport to
// addr and produces a WorkspaceServiceClient riding it. The close
// func releases idle HTTP/2 conns; in-flight calls finish via ctx
// cancellation.
func openDirectWorkspaceClient(ctx context.Context, addr, configDir string) (*workspaceClient, error) {
	sc, err := loadSandboxdContext(configDir)
	if err != nil {
		return nil, err
	}
	tlsCfg, err := identity.ClientTLSConfig(sc.id, sc.peers, "")
	if err != nil {
		return nil, err
	}
	httpc := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}}
	tracer := trace.TracerFromContext(ctx)
	if tracer == nil {
		tracer = trace.New(component.Sandboxd)
	}
	return &workspaceClient{
		rpc: workspacev1connect.NewWorkspaceServiceClient(
			httpc, "https://"+addr,
			connect.WithInterceptors(tracer.ClientInterceptor()),
		),
		store: sc.store,
		close: httpc.CloseIdleConnections,
	}, nil
}

// openBrokeredWorkspaceClient runs the broker handshake: register
// with the orchestrator, wait for the user-host's `daft workspace
// attach`, run inner mTLS as TLS client, and produce a
// WorkspaceServiceClient riding the brokered conn.
func openBrokeredWorkspaceClient(ctx context.Context, opts pullWorkspaceBrokerOpts) (*workspaceClient, error) {
	sc, err := loadSandboxdContext(opts.configDir)
	if err != nil {
		return nil, err
	}
	serveFP, err := identity.ParseFingerprint(opts.serveFP)
	if err != nil {
		return nil, fmt.Errorf("daft-sandboxd: parse serve fingerprint: %w", err)
	}
	bc, err := brokerclient.New(brokerclient.Config{
		URL: opts.orchestratorURL, Identity: sc.id, Peers: sc.peers,
	})
	if err != nil {
		return nil, fmt.Errorf("daft-sandboxd: broker client: %w", err)
	}
	ic, err := bc.AttachAndDial(ctx, brokerclient.AttachOptions{
		Workspace:        opts.workspace,
		SandboxID:        opts.sandboxID,
		ServeFingerprint: serveFP,
	})
	if err != nil {
		return nil, fmt.Errorf("daft-sandboxd: broker attach: %w", err)
	}
	tracer := trace.TracerFromContext(ctx)
	if tracer == nil {
		tracer = trace.New(component.Sandboxd)
	}
	return &workspaceClient{
		rpc: workspacev1connect.NewWorkspaceServiceClient(
			ic.HTTPClient, ic.BaseURL,
			connect.WithInterceptors(tracer.ClientInterceptor()),
		),
		store: sc.store,
		close: func() { _ = ic.Close() },
	}, nil
}

// pullWorkspaceBroker registers as a sandbox with the
// orchestrator at opts.orchestratorURL, waits for the user-host's
// `daft workspace attach` to broker us a connection, runs inner
// mTLS as TLS client, and pulls the workspace manifest+chunks
// into a fresh tmpdir. Mirrors pullWorkspaceFrom's interface so
// the caller (resolveWorkspaceDir) treats both modes symmetrically.
func pullWorkspaceBroker(ctx context.Context, opts pullWorkspaceBrokerOpts) (dir string, cleanup func(), err error) {
	bc, err := openBrokeredWorkspaceClient(ctx, opts)
	if err != nil {
		return "", nil, err
	}
	dest, cleanupTmp, err := fetchAndApplyManifest(ctx, bc.rpc, bc.store, opts.workspace)
	if err != nil {
		bc.close()
		return "", nil, err
	}
	return dest, func() {
		bc.close()
		cleanupTmp()
	}, nil
}

// serveDaftFuse fetches the manifest via rpc, mounts a daft-fuse FS
// over rpc + store in a fresh tmpdir, and returns the mountpoint
// plus a cleanup that unmounts → drains in-flight RPCs via
// srv.Wait → calls closeTransport → removes the tmpdir.
// closeTransport must release whatever held rpc alive (HTTP
// transport, broker InnerClient, etc.). On any error before Serve
// succeeds, the deferred error-path closes the transport and
// removes any half-created tmpdir.
func serveDaftFuse(
	ctx context.Context,
	rpc workspacev1connect.WorkspaceServiceClient,
	store *cas.Store,
	workspaceName string,
	closeTransport func(),
) (mountpoint string, cleanup func(), err error) {
	defer func() {
		if err != nil {
			_ = os.RemoveAll(mountpoint)
			closeTransport()
		}
	}()
	resp, err := rpc.GetManifest(ctx,
		connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: workspaceName}))
	if err != nil {
		return "", nil, fmt.Errorf("daft-sandboxd: GetManifest: %w", err)
	}
	mnt, err := daftfuse.New(resp.Msg.GetManifest(), store,
		daftfuse.NewConnectFetcher(rpc), daftfuse.Options{})
	if err != nil {
		return "", nil, fmt.Errorf("daft-sandboxd: daftfuse: %w", err)
	}
	mountpoint, err = os.MkdirTemp("", "daft-sandboxd-lazy-")
	if err != nil {
		return "", nil, fmt.Errorf("daft-sandboxd: lazy mountpoint: %w", err)
	}
	srv, err := mnt.Serve(ctx, mountpoint)
	if err != nil {
		return "", nil, fmt.Errorf("daft-sandboxd: daftfuse mount %s: %w", mountpoint, err)
	}
	return mountpoint, func() {
		oops.Log("daft-sandboxd: unmount lazy workspace", srv.Unmount())
		// Wait for in-flight FUSE reads to drain before tearing
		// down the transport — a pending GetChunk holding the
		// HTTP/2 stream would otherwise race the transport close.
		srv.Wait()
		closeTransport()
		oops.Log("daft-sandboxd: remove lazy mountpoint", os.RemoveAll(mountpoint))
	}, nil
}

// attachLazyWorkspaceFrom is the lazy-mode counterpart of
// pullWorkspaceFrom: same direct-dial mTLS transport, but instead of
// pre-fetching every chunk via ApplyManifest, it mounts daft-fuse at
// a tmpdir and lets the VM's reads trigger per-chunk GetChunk calls
// on demand. Mirrors pullWorkspaceFrom's signature so
// resolveWorkspaceDir treats both modes symmetrically.
func attachLazyWorkspaceFrom(ctx context.Context, opts pullWorkspaceOpts) (dir string, cleanup func(), err error) {
	wc, err := openDirectWorkspaceClient(ctx, opts.addr, opts.configDir)
	if err != nil {
		return "", nil, err
	}
	return serveDaftFuse(ctx, wc.rpc, wc.store, opts.name, wc.close)
}

// attachLazyWorkspaceBroker is the lazy-mode counterpart of
// pullWorkspaceBroker: same orchestrator-brokered transport (inner
// mTLS over Relay byte-splice), but mounts daft-fuse for on-demand
// chunk fetch instead of pre-fetching.
func attachLazyWorkspaceBroker(ctx context.Context, opts pullWorkspaceBrokerOpts) (dir string, cleanup func(), err error) {
	bc, err := openBrokeredWorkspaceClient(ctx, opts)
	if err != nil {
		return "", nil, err
	}
	return serveDaftFuse(ctx, bc.rpc, bc.store, opts.workspace, bc.close)
}

func runRun(ctx context.Context, w io.Writer, opts runOptions, deps runDeps) error {
	if trace.TracerFromContext(ctx) == nil {
		ctx = trace.ContextWithTracer(ctx, trace.New(component.Sandboxd))
	}
	ctx, span := trace.Start(ctx, "daft-sandboxd.run",
		trace.WithAttributes(
			trace.KV{Key: "ref", Value: opts.ref},
			trace.KV{Key: "args", Value: strconv.Itoa(len(opts.cmdArgs))},
			trace.KV{Key: "workspace", Value: opts.workspace},
			trace.KV{Key: "workspace_from", Value: opts.workspaceFrom},
		))
	defer span.End()

	if err := opts.validate(); err != nil {
		return trace.RecordError(ctx, err)
	}

	workspaceDir, cleanup, err := resolveWorkspaceDir(ctx, opts, deps)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	defer cleanup()

	cfg, unlock, err := assembleBootConfig(ctx, opts, workspaceDir, deps)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	defer unlock()

	return trace.RecordError(ctx, deps.boot(ctx, w, cfg))
}

// validate checks the flag combinations rejected by runRun before
// any side-effects fire.
func (o runOptions) validate() error {
	switch {
	case o.workspaceFrom != "" && o.workspaceBroker != "":
		return errors.New("daft-sandboxd: --workspace-from and --workspace-broker are mutually exclusive")
	case o.workspace != "" && (o.workspaceFrom != "" || o.workspaceBroker != ""):
		return errors.New("daft-sandboxd: --workspace is mutually exclusive with --workspace-from / --workspace-broker")
	case o.workspaceFrom != "" && o.workspaceName == "":
		return errors.New("daft-sandboxd: --workspace-from requires --workspace-name")
	case o.workspaceBroker != "" && o.workspaceName == "":
		return errors.New("daft-sandboxd: --workspace-broker requires --workspace-name")
	case o.workspaceBroker != "" && o.workspaceServeFP == "":
		return errors.New("daft-sandboxd: --workspace-broker requires --workspace-serve-fingerprint")
	case o.workspaceBroker != "" && o.workspaceSandboxID == "":
		return errors.New("daft-sandboxd: --workspace-broker requires --workspace-sandbox-id")
	case o.workspaceName != "" && o.workspaceFrom == "" && o.workspaceBroker == "":
		return errors.New("daft-sandboxd: --workspace-name requires --workspace-from or --workspace-broker")
	case o.workspaceAttach && o.workspaceFrom == "" && o.workspaceBroker == "":
		return errors.New("daft-sandboxd: --workspace-attach requires --workspace-from or --workspace-broker")
	case o.workspaceAttach && o.keepWorkspace:
		return errors.New("daft-sandboxd: --workspace-attach is mutually exclusive with --keep-workspace (nothing to keep — chunks live in CAS)")
	}
	return nil
}

// resolveWorkspaceDir returns the on-disk directory to expose at
// /workspace, plus a cleanup that runs on function exit. Four
// modes:
//   - --workspace: caller-supplied dir, no copy.
//   - --workspace-from: direct-dial pull from a daft workspace
//     serve at host:port. Fresh tmpdir, every chunk pre-fetched.
//   - --workspace-broker: register as a sandbox with the
//     orchestrator and wait for the user-host's `daft workspace
//     attach` to broker us a connection. Fresh tmpdir,
//     pre-fetched.
//   - --workspace-attach (combined with --workspace-from or
//     --workspace-broker): same transport, but the workspace is
//     mounted lazily via daft-fuse — chunks fetch on first read
//     instead of pre-loading at boot.
//
// When opts.keepWorkspace is set the tmpdir is preserved past the
// run — the cleanup becomes a no-op that prints the path to stdout
// so the operator knows where to point a follow-up
// `daft-sandboxd workspace serve` for pull-back. --workspace-attach
// rejects --keep-workspace (validated up-front): the chunks live
// in the CAS, there's no eager-materialised tmpdir to preserve.
func resolveWorkspaceDir(ctx context.Context, opts runOptions, deps runDeps) (dir string, cleanup func(), err error) {
	switch {
	case opts.workspaceAttach && opts.workspaceBroker != "":
		dir, cleanup, err = deps.attachWorkspaceBroker(ctx, pullWorkspaceBrokerOpts{
			orchestratorURL: opts.workspaceBroker,
			workspace:       opts.workspaceName,
			sandboxID:       opts.workspaceSandboxID,
			serveFP:         opts.workspaceServeFP,
			configDir:       opts.configDir,
		})
	case opts.workspaceAttach:
		dir, cleanup, err = deps.attachWorkspaceFrom(ctx, pullWorkspaceOpts{
			addr: opts.workspaceFrom, name: opts.workspaceName, configDir: opts.configDir,
		})
	case opts.workspaceBroker != "":
		dir, cleanup, err = deps.pullWorkspaceBroker(ctx, pullWorkspaceBrokerOpts{
			orchestratorURL: opts.workspaceBroker,
			workspace:       opts.workspaceName,
			sandboxID:       opts.workspaceSandboxID,
			serveFP:         opts.workspaceServeFP,
			configDir:       opts.configDir,
		})
	case opts.workspaceFrom != "":
		dir, cleanup, err = deps.pullWorkspaceFrom(ctx, pullWorkspaceOpts{
			addr: opts.workspaceFrom, name: opts.workspaceName, configDir: opts.configDir,
		})
	default:
		return opts.workspace, func() {}, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("daft-sandboxd: pull workspace: %w", err)
	}
	if opts.keepWorkspace {
		realCleanup := cleanup
		cleanup = func() {
			fmt.Fprintf(os.Stderr,
				"daft-sandboxd: workspace kept at %s\n"+
					"daft-sandboxd: serve back with `daft-sandboxd workspace serve --path=%s --workspace=<name> --addr=...`\n",
				dir, dir)
			_ = realCleanup // intentionally not invoked: --keep-workspace's whole point
		}
	}
	return dir, cleanup, nil
}

// assembleBootConfig pulls + bundles + locks the image and builds
// the sandbox.Config the caller passes to deps.boot. unlock returns
// the bundle lock so the caller can defer it; assembleBootConfig
// own only the lock acquired in this call, no half-states.
func assembleBootConfig(ctx context.Context, opts runOptions, workspaceDir string, deps runDeps) (sandbox.Config, func(), error) {
	store, err := deps.openStore()
	if err != nil {
		return sandbox.Config{}, nil, err
	}
	img, err := store.Pull(ctx, opts.ref)
	if err != nil {
		return sandbox.Config{}, nil, fmt.Errorf("daft-sandboxd: pull %s: %w", opts.ref, err)
	}
	bundleOpts := imagestore.BundleOptions{}
	if len(opts.cmdArgs) > 0 {
		bundleOpts.Args = opts.cmdArgs
	}
	if workspaceDir != "" {
		bundleOpts.ExtraMounts = []imagestore.BundleMount{{
			Destination: workspaceMount,
			Type:        "bind",
			Source:      workspaceMount,
			Options:     []string{"rbind", "rw"},
		}}
	}
	bundle, err := store.Bundle(ctx, img.Digest, bundleOpts)
	if err != nil {
		return sandbox.Config{}, nil, fmt.Errorf("daft-sandboxd: bundle %s: %w", img.Digest, err)
	}
	unlock, err := store.Lock(img.Digest)
	if err != nil {
		return sandbox.Config{}, nil, fmt.Errorf("daft-sandboxd: lock bundle: %w", err)
	}
	cacheDir, err := deps.cacheDir()
	if err != nil {
		unlock()
		return sandbox.Config{}, nil, err
	}
	paths, err := deps.extract(cacheDir)
	if err != nil {
		unlock()
		return sandbox.Config{}, nil, fmt.Errorf("daft-sandboxd: extract bundled artifacts: %w", err)
	}

	cfg := sandbox.Config{
		CloudHypervisor: paths.CloudHypervisor,
		Kernel:          paths.Kernel,
		Initramfs:       paths.Initramfs,
		Virtiofsd:       paths.Virtiofsd,
		Shares: []*sandbox.ShareConfig{{
			HostDir:    filepath.Dir(bundle.Path),
			Tag:        bundlesTag,
			Mountpoint: bundlesMount,
		}},
		Bundle: filepath.Join(bundlesMount, filepath.Base(bundle.Path)),
	}
	if workspaceDir != "" {
		abs, err := filepath.Abs(workspaceDir)
		if err != nil {
			unlock()
			return sandbox.Config{}, nil, fmt.Errorf("daft-sandboxd: resolve workspace %q: %w", workspaceDir, err)
		}
		cfg.Shares = append(cfg.Shares, &sandbox.ShareConfig{
			HostDir:    abs,
			Tag:        workspaceTag,
			Mountpoint: workspaceMount,
		})
	}
	return cfg, unlock, nil
}
