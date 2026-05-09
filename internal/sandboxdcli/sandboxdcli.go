// Package sandboxdcli wires up the daft-sandboxd command tree.
//
// daft-sandboxd is the single-executable sandbox-host: cloud-hypervisor,
// the guest kernel, and the initramfs are all embedded in the binary
// (see internal/bundled). At run time it extracts them to the user's
// cache directory and uses them as the default Config for sandbox.Hello.
//
// Builds outside the project's Nix flake see stub artifacts, so the
// `hello` command refuses to run and points the user at the env-var
// fallback (which `daft sandbox hello` exposes for dev work).
package sandboxdcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/richiejp/darkaifactory/internal/bundled"
	"github.com/richiejp/darkaifactory/internal/sandbox"
	"github.com/richiejp/darkaifactory/internal/version"
)

// cacheSubdir is the directory we create under os.UserCacheDir for our
// extracted artifacts. Stable so the same cache survives across runs.
const cacheSubdir = "daft-sandboxd"

// NewRootCmd builds the daft-sandboxd command tree. Tests construct
// the tree directly to exercise it without spawning a subprocess.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "daft-sandboxd",
		Short: "darkaifactory sandbox-host (single-executable)",
		Long: "daft-sandboxd is the sandbox-host stack as a single static binary.\n\n" +
			"It embeds the VMM (cloud-hypervisor), the guest kernel, and the\n" +
			"sandbox initramfs, extracts them to the user cache directory on\n" +
			"first run, and boots sandbox VMs from them.",
		SilenceUsage: true,
	}
	root.AddCommand(newVersionCmd())
	root.AddCommand(newHelloCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the daft-sandboxd version",
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

// helloDeps bundles the moving parts of `daft-sandboxd hello` so unit
// tests can swap each one for a fake. Production wires the real
// implementations via defaultHelloDeps.
type helloDeps struct {
	cacheDir func() (string, error)
	extract  func(string) (bundled.Paths, error)
	boot     func(context.Context, io.Writer, sandbox.Config) error
}

func defaultHelloDeps() helloDeps {
	return helloDeps{
		cacheDir: userCacheDir,
		extract:  bundled.Extract,
		boot:     sandbox.Hello,
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
			"daft-sandboxd: this binary has no bundled artifacts (built outside the Nix flake?)\n"+
				"For dev work, use `daft sandbox hello` with %s / %s / %s set",
			sandbox.EnvCloudHypervisor, sandbox.EnvKernel, sandbox.EnvInitramfs,
		)
	}
	if err != nil {
		return err
	}
	return deps.boot(ctx, w, sandbox.Config{
		CloudHypervisor: paths.CloudHypervisor,
		Kernel:          paths.Kernel,
		Initramfs:       paths.Initramfs,
	})
}

// userCacheDir returns the directory we extract artifacts into. Uses
// os.UserCacheDir which honours $XDG_CACHE_HOME on Linux.
func userCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("daft-sandboxd: locate cache dir: %w", err)
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
