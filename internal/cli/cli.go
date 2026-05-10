// Package cli wires up the daft command tree.
//
// The root command and all subcommands are constructed by NewRootCmd, which
// is callable from tests as well as from cmd/daft/main.go. Keeping command
// construction in a library package (rather than main) is what makes the
// CLI surface testable.
//
// The CLI is built on github.com/spf13/cobra, which gives us:
//   - Sub-commands and nested help out of the box.
//   - An auto-generated `completion` sub-command supporting bash, zsh, fish,
//     and PowerShell — registered automatically as long as the root command
//     has at least one user-defined sub-command.
//   - PersistentFlags that can be backed by environment variables (see the
//     --verbose / DAFT_VERBOSE wiring below).
package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/richiejp/darkaifactory/internal/sandbox"
	"github.com/richiejp/darkaifactory/internal/version"
)

// envPrefix is the prefix used for environment-variable bindings on the
// daft command. Keep it short and stable — renaming it is a breaking change
// for anyone scripting against the CLI.
const envPrefix = "DAFT_"

// versionCmdName is the name of the `daft version` sub-command. Promoted
// to a constant so tests and benchmarks reference the same string the
// command tree registers.
const versionCmdName = "version"

// NewRootCmd constructs the daft command tree. It is exported (lower-case
// would be sufficient for cmd/daft, but tests in this package construct the
// tree directly to exercise it without a subprocess).
//
// The returned command has not yet been Execute()d — callers may inspect
// or mutate it (for example, replacing its Out/Err writers in tests).
func NewRootCmd() *cobra.Command {
	var verbose bool

	root := &cobra.Command{
		Use:   "daft",
		Short: "darkaifactory CLI",
		Long: "daft is the command-line entry point for the darkaifactory project.\n\n" +
			"Run `daft completion --help` to set up shell completions, and\n" +
			"`daft version` to print the embedded build version.",
		// Don't print usage on every error — the error itself is usually enough,
		// and a wall of usage text obscures the actual problem.
		SilenceUsage: true,
	}

	// --verbose / DAFT_VERBOSE. Demonstrates env-var binding for flags. The
	// pattern: register a flag, then in PersistentPreRunE, fall back to the
	// env var when the flag wasn't explicitly set. This avoids pulling in
	// viper just for env wiring while still being conventional.
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false,
		"enable verbose output (env: "+envPrefix+"VERBOSE)")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("verbose") {
			return nil
		}
		raw, ok := os.LookupEnv(envPrefix + "VERBOSE")
		if !ok || raw == "" {
			return nil
		}
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("invalid %sVERBOSE=%q: %w", envPrefix, raw, err)
		}
		verbose = v
		return nil
	}

	root.AddCommand(newVersionCmd())
	root.AddCommand(newSandboxCmd())
	root.AddCommand(newAuthCmd())
	root.AddCommand(newPingCmd())
	root.AddCommand(newWorkspaceCmd())
	// Note: cobra auto-generates a `completion` sub-command for us; we don't
	// need to register it explicitly. Keeping it auto-generated means it
	// stays in sync with cobra's supported shells.

	return root
}

// newSandboxCmd returns the `daft sandbox` parent command. Its sub-commands
// drive sandbox VMs on the configured sandbox-host. Each sub-command
// delegates to internal/sandbox so the CLI layer stays a thin wrapper.
func newSandboxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Manage sandbox VMs",
		Long: "Sandbox sub-commands boot, inspect, and tear down sandbox VMs on the\n" +
			"configured sandbox-host. The microVM stack and protocol details are\n" +
			"described in PLAN.md.",
	}
	cmd.AddCommand(newSandboxHelloCmd())
	return cmd
}

// newSandboxHelloCmd returns `daft sandbox hello`. It boots a minimal
// sandbox VM that runs the daft-agent stub, captures the agent's banner
// from the serial console, and prints it. Used to verify the full
// kernel/init/agent boot path end to end.
func newSandboxHelloCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "hello",
		Short: "Boot a minimal sandbox VM and print its hello banner",
		Long: "Boot a minimal sandbox VM that runs the daft-agent stub, capture the\n" +
			"agent's banner from the serial console, and print it on stdout. Returns\n" +
			"non-zero if the VM fails to boot or the banner does not appear.\n\n" +
			"This is the smoke test for the microVM stack and is expected to work\n" +
			"on any sandbox-host with /dev/kvm available.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := sandbox.ConfigFromEnv()
			if err != nil {
				return err
			}
			return sandbox.Hello(cmd.Context(), cmd.OutOrStdout(), cfg)
		},
	}
}

// newVersionCmd returns the `daft version` sub-command. Output goes to the
// command's configured Out writer (so tests can capture it).
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   versionCmdName,
		Short: "Print the daft version",
		Long: "Print the embedded build version of the daft binary.\n\n" +
			"The version is injected at build time via -ldflags from version.txt.\n" +
			"A value of \"dev\" indicates a local build with no version override.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Invariant: Version is non-empty. The package-level default is
			// "dev" and any -ldflags override would also be non-empty. If
			// someone replaces it with "" we'd silently print a blank line —
			// assert instead.
			if version.Version == "" {
				return fmt.Errorf("version.Version is empty (broken build)")
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version.Version)
			return err
		},
	}
}

// Execute builds the root command, points its output streams at the given
// writers, and runs it with the given arguments. Returns the command's
// exit error (or nil on success). Callers in main typically pass os.Args[1:],
// os.Stdout, os.Stderr.
func Execute(args []string, stdout, stderr io.Writer) error {
	root := NewRootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	return root.Execute()
}
