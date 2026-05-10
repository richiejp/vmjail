package sandboxdcli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/richiejp/darkaifactory/internal/bundled"
	"github.com/richiejp/darkaifactory/internal/component"
	"github.com/richiejp/darkaifactory/internal/imagestore"
	"github.com/richiejp/darkaifactory/internal/sandbox"
	"github.com/richiejp/darkaifactory/internal/trace"
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
	var workspace string
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
			"the container persist on the host.",
		Args:          cobra.MinimumNArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRun(cmd.Context(), cmd.OutOrStdout(),
				args[0], args[1:], workspace, defaultRunDeps())
		},
	}
	cmd.Flags().StringVarP(&workspace, "workspace", "w", "",
		"host directory to mount read-write at /workspace inside the container")
	return cmd
}

// runDeps bundles the moving parts of `run` so unit tests can swap
// in fakes for the network-talking and VM-spawning bits.
type runDeps struct {
	openStore func() (storeAPI, error)
	cacheDir  func() (string, error)
	extract   func(string) (bundled.Paths, error)
	boot      func(context.Context, io.Writer, sandbox.Config) error
}

// storeAPI is the surface of imagestore.Store the run command uses.
// Defining it as an interface keeps run-side tests from spinning up
// a real registry just to assert wiring choices.
type storeAPI interface {
	Pull(ctx context.Context, ref string) (*imagestore.Image, error)
	Bundle(ctx context.Context, digest string, opts imagestore.BundleOptions) (*imagestore.Bundle, error)
	Lock(digest string) (func(), error)
}

func defaultRunDeps() runDeps {
	return runDeps{
		openStore: func() (storeAPI, error) { return openStore() },
		cacheDir:  userCacheDir,
		extract:   bundled.Extract,
		boot:      sandbox.Hello,
	}
}

func runRun(ctx context.Context, w io.Writer, ref string, args []string, workspace string, deps runDeps) error {
	if trace.TracerFromContext(ctx) == nil {
		ctx = trace.ContextWithTracer(ctx, trace.New(component.Sandboxd))
	}
	ctx, span := trace.Start(ctx, "daft-sandboxd.run",
		trace.WithAttributes(
			trace.KV{Key: "ref", Value: ref},
			trace.KV{Key: "args", Value: strconv.Itoa(len(args))},
			trace.KV{Key: "workspace", Value: workspace},
		))
	defer span.End()

	store, err := deps.openStore()
	if err != nil {
		return trace.RecordError(ctx, err)
	}

	img, err := store.Pull(ctx, ref)
	if err != nil {
		return trace.RecordError(ctx, fmt.Errorf("daft-sandboxd: pull %s: %w", ref, err))
	}

	bundleOpts := imagestore.BundleOptions{}
	if len(args) > 0 {
		bundleOpts.Args = args
	}
	if workspace != "" {
		bundleOpts.ExtraMounts = []imagestore.BundleMount{{
			Destination: workspaceMount,
			Type:        "bind",
			Source:      workspaceMount,
			Options:     []string{"rbind", "rw"},
		}}
	}

	bundle, err := store.Bundle(ctx, img.Digest, bundleOpts)
	if err != nil {
		return trace.RecordError(ctx, fmt.Errorf("daft-sandboxd: bundle %s: %w", img.Digest, err))
	}

	unlock, err := store.Lock(img.Digest)
	if err != nil {
		return trace.RecordError(ctx, fmt.Errorf("daft-sandboxd: lock bundle: %w", err))
	}
	defer unlock()

	cacheDir, err := deps.cacheDir()
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	paths, err := deps.extract(cacheDir)
	if err != nil {
		return trace.RecordError(ctx, fmt.Errorf("daft-sandboxd: extract bundled artifacts: %w", err))
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
	if workspace != "" {
		abs, err := filepath.Abs(workspace)
		if err != nil {
			return trace.RecordError(ctx,
				fmt.Errorf("daft-sandboxd: resolve --workspace %q: %w", workspace, err))
		}
		cfg.Shares = append(cfg.Shares, &sandbox.ShareConfig{
			HostDir:    abs,
			Tag:        workspaceTag,
			Mountpoint: workspaceMount,
		})
	}

	return trace.RecordError(ctx, deps.boot(ctx, w, cfg))
}
