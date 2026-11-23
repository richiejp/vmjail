package sandboxdcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/richiejp/vmjail/internal/bundled"
	"github.com/richiejp/vmjail/internal/component"
	"github.com/richiejp/vmjail/internal/imagestore"
	"github.com/richiejp/vmjail/internal/sandbox"
	"github.com/richiejp/vmjail/internal/trace"
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
		Long:  "Pull an image into the local store and run it in a VM. --workspace shares a host directory at /workspace; --workspace-overlay keeps writes in a temporary guest overlay.",
		Args:  cobra.MinimumNArgs(1), SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.ref, opts.cmdArgs = args[0], args[1:]
			return runRun(cmd.Context(), cmd.OutOrStdout(), opts, defaultRunDepsFn())
		},
	}
	cmd.Flags().StringVarP(&opts.workspace, "workspace", "w", "", "host directory to mount at /workspace")
	cmd.Flags().BoolVar(&opts.workspaceOverlay, "workspace-overlay", false, "keep workspace writes in a temporary guest overlay")
	return cmd
}

// runOptions consolidates the run command's flags. Hoisted so adding
// a flag is one struct field plus one Flags() call.
type runOptions struct {
	ref       string
	cmdArgs   []string
	workspace string

	workspaceOverlay bool
}

// runDeps bundles the moving parts of `run` so unit tests can swap
// in fakes for the network-talking and VM-spawning bits.
type runDeps struct {
	openStore func() (storeAPI, error)
	cacheDir  func() (string, error)
	extract   func(string) (bundled.Paths, error)
	boot      func(context.Context, io.Writer, sandbox.Config) error

	// observerLog resolves the per-sandbox observer-log path so the
	// boot gets a default ObserverLogPath. nil disables defaulting.
	observerLog func() (string, error)
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
//
//nolint:gochecknoglobals // test-hook indirection for the cobra wiring; swapped only in tests.
var defaultRunDepsFn = defaultRunDeps

func defaultRunDeps() runDeps {
	return runDeps{
		openStore: func() (storeAPI, error) { return openStore() },
		cacheDir:  userCacheDir,
		extract:   bundled.Extract,
		boot:      sandbox.Hello,

		observerLog: defaultObserverLog,
	}
}

func runRun(ctx context.Context, w io.Writer, opts runOptions, deps runDeps) error {
	if trace.TracerFromContext(ctx) == nil {
		ctx = trace.ContextWithTracer(ctx, trace.New(component.Sandboxd))
	}
	ctx, span := trace.Start(ctx, "vmjail-sandboxd.run",
		trace.WithAttributes(
			trace.KV{Key: "ref", Value: opts.ref},
			trace.KV{Key: "args", Value: strconv.Itoa(len(opts.cmdArgs))},
			trace.KV{Key: "workspace", Value: opts.workspace},
		))
	defer span.End()

	if err := opts.validate(); err != nil {
		return trace.RecordError(ctx, err)
	}

	workspaceDir := opts.workspace

	cfg, unlock, err := assembleBootConfig(ctx, opts, workspaceDir, deps)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	defer unlock()

	applyObserverLog(w, &cfg, deps.observerLog)
	return trace.RecordError(ctx, deps.boot(ctx, w, cfg))
}

// validate checks the flag combinations rejected by runRun before
// any side-effects fire.
func (o runOptions) validate() error {
	if o.workspaceOverlay && o.workspace == "" {
		return errors.New("vmjail-sandboxd: --workspace-overlay requires a workspace share (use --workspace)")
	}
	return nil
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
		return sandbox.Config{}, nil, fmt.Errorf("vmjail-sandboxd: pull %s: %w", opts.ref, err)
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
		return sandbox.Config{}, nil, fmt.Errorf("vmjail-sandboxd: bundle %s: %w", img.Digest, err)
	}
	unlock, err := store.Lock(img.Digest)
	if err != nil {
		return sandbox.Config{}, nil, fmt.Errorf("vmjail-sandboxd: lock bundle: %w", err)
	}
	cacheDir, err := deps.cacheDir()
	if err != nil {
		unlock()
		return sandbox.Config{}, nil, err
	}
	paths, err := deps.extract(cacheDir)
	if err != nil {
		unlock()
		return sandbox.Config{}, nil, fmt.Errorf("vmjail-sandboxd: extract bundled artifacts: %w", err)
	}

	bundleRelease := unlock
	unlock = func() { paths.Close(); bundleRelease() }
	vsock, vsockCleanup, err := sandbox.NewStandaloneVsock()
	if err != nil {
		unlock()
		return sandbox.Config{}, nil, err
	}
	// Compose the vsock-dir cleanup onto the bundle unlock so the caller's
	// single deferred func releases both.
	bundleUnlock := unlock
	unlock = func() { vsockCleanup(); bundleUnlock() }

	cfg := sandbox.Config{
		Shares: []*sandbox.ShareConfig{{
			HostDir:    filepath.Dir(bundle.Path),
			Tag:        bundlesTag,
			Mountpoint: bundlesMount,
		}},
		Bundle: filepath.Join(bundlesMount, filepath.Base(bundle.Path)),
		Vsock:  vsock,
	}
	cfg.ApplyArtifactDefaults(paths)

	if workspaceDir != "" {
		abs, err := filepath.Abs(workspaceDir)
		if err != nil {
			unlock()
			return sandbox.Config{}, nil, fmt.Errorf("vmjail-sandboxd: resolve workspace %q: %w", workspaceDir, err)
		}
		cfg.Shares = append(cfg.Shares, &sandbox.ShareConfig{
			HostDir:    abs,
			Tag:        workspaceTag,
			Mountpoint: workspaceMount,
			Overlay:    opts.workspaceOverlay,
		})
	}
	return cfg, unlock, nil
}
