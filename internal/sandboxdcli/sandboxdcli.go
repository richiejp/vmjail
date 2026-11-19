// Package sandboxdcli wires up the vmjail-sandboxd command tree.
//
// vmjail-sandboxd is the single-executable sandbox-host: cloud-hypervisor,
// the guest kernel, and the initramfs are all embedded in the binary
// (see internal/bundled). At run time it extracts them to the user's
// cache directory and uses them as the default Config for sandbox.Hello.
//
// Builds outside the project's Nix flake see stub artifacts, so the
// `hello` command refuses to run and points the user at the env-var
// fallback (which `vmjail sandbox hello` exposes for dev work).
package sandboxdcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/richiejp/vmjail/internal/banner"
	"github.com/richiejp/vmjail/internal/bundled"
	"github.com/richiejp/vmjail/internal/sandbox"
	"github.com/richiejp/vmjail/internal/version"
)

// cacheSubdir is the directory we create under os.UserCacheDir for our
// extracted artifacts. Stable so the same cache survives across runs.
const cacheSubdir = "vmjail-sandboxd"

// NewRootCmd builds the vmjail-sandboxd command tree. Tests construct
// the tree directly to exercise it without spawning a subprocess.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "vmjail-sandboxd",
		Short: "vmjail sandbox-host (single-executable)",
		Long: banner.String +
			"\nvmjail-sandboxd is the sandbox-host stack as a single static binary.\n\n" +
			"It embeds the VMM (cloud-hypervisor), the guest kernel, and the\n" +
			"sandbox initramfs, extracts them to the user cache directory on\n" +
			"first run, and boots sandbox VMs from them.",
		SilenceUsage: true,
	}
	root.AddCommand(newVersionCmd())
	root.AddCommand(newHelloCmd())
	root.AddCommand(newPullCmd())
	root.AddCommand(newImagesCmd())
	root.AddCommand(newDebugCmd())
	return root
}

// newDebugCmd contains the retained local image-run command.
func newDebugCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "debug",
		Short: "Local sandbox debugging commands",
		Long:  "Run local image workloads for testing and reproduction.",
	}
	cmd.AddCommand(newRunCmd())
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the vmjail-sandboxd version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if version.Version == "" {
				return fmt.Errorf("version.Version is empty (broken build)")
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version.Version)
			return err
		},
	}
}

func newHelloCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "hello",
		Short: "Boot a sandbox VM and print its hello banner",
		Long: "Extract the bundled artifacts to the user cache, boot a sandbox\n" +
			"VM, and print the agent's banner from the serial console.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runHello(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

// helloDeps bundles the moving parts of `vmjail-sandboxd hello` so unit
// tests can swap each one for a fake. Production wires the real
// implementations via defaultHelloDeps.
type helloDeps struct {
	cacheDir func() (string, error)
	extract  func(string) (bundled.Paths, error)
	boot     func(context.Context, io.Writer, sandbox.Config) error
	// observerLog resolves the per-sandbox host-side observer-log
	// path (mkdir'ing its dir) so the boot gets a default
	// ObserverLogPath. nil disables defaulting — used by tests that
	// don't want to touch the state dir.
	observerLog func() (string, error)
}

func defaultHelloDeps() helloDeps {
	return helloDeps{
		cacheDir:    userCacheDir,
		extract:     bundled.Extract,
		boot:        sandbox.Hello,
		observerLog: defaultObserverLog,
	}
}

func runHello(ctx context.Context, w io.Writer) error {
	return runHelloWith(ctx, w, defaultHelloDeps())
}

func runHelloWith(ctx context.Context, w io.Writer, deps helloDeps) error {
	cacheDir, err := deps.cacheDir()
	if err != nil {
		return err
	}
	paths, err := deps.extract(cacheDir)
	if errors.Is(err, bundled.ErrNotBundled) {
		return fmt.Errorf(
			"vmjail-sandboxd: this binary has no bundled artifacts (built outside the Nix flake?)\n"+
				"For dev work, use `vmjail sandbox hello` with %s / %s / %s set",
			sandbox.EnvCloudHypervisor, sandbox.EnvKernel, sandbox.EnvInitramfs,
		)
	}
	if err != nil {
		return err
	}
	defer paths.Close()
	cfg := sandbox.Config{
		ArtifactFiles:   paths.Files,
		CloudHypervisor: paths.CloudHypervisor,
		Kernel:          paths.Kernel,
		Initramfs:       paths.Initramfs,
		Virtiofsd:       paths.Virtiofsd,
	}
	// Always-on vsock: deliver config + status events over the control
	// channel rather than the kernel cmdline.
	vsock, cleanup, err := sandbox.NewStandaloneVsock()
	if err != nil {
		return err
	}
	defer cleanup()
	cfg.Vsock = vsock
	applyObserverLog(w, &cfg, deps.observerLog)
	return deps.boot(ctx, w, cfg)
}

// applyObserverLog defaults cfg.ObserverLogPath from resolve and
// announces the chosen path on w. Observability is a debugging
// surface, never a launch precondition: a nil resolver or a
// state-dir error leaves the observer disabled and the boot
// proceeds. An already-set path is left untouched so an explicit
// caller override wins.
func applyObserverLog(w io.Writer, cfg *sandbox.Config, resolve func() (string, error)) {
	if resolve == nil || cfg.ObserverLogPath != "" {
		return
	}
	path, err := resolve()
	if err != nil {
		_, _ = fmt.Fprintf(w, "vmjail-sandboxd: observer log disabled: %v\n", err)
		return
	}
	cfg.ObserverLogPath = path
	_, _ = fmt.Fprintf(w, "vmjail-sandboxd: observer log %s\n", path)
}

// userCacheDir supplies the legacy loader hint. The embedded memfd loader
// ignores it; retained loader interfaces still accept a cache directory.
func userCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("vmjail-sandboxd: locate cache dir: %w", err)
	}
	return filepath.Join(base, cacheSubdir), nil
}

// Execute builds the root command, wires its writers, and runs it.
func Execute(args []string, stdout, stderr io.Writer) error {
	root := NewRootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return root.Execute()
}
