// Package orchestratorcli is the cobra command tree for the
// daft-orchestrator binary. Single subcommand for now (`serve`);
// `auth approve` and registry endpoints will join it.
package orchestratorcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/orchestrator"
	"github.com/richiejp/darkaifactory/internal/version"
)

// configSubdir is appended to the user's config dir to derive the
// orchestrator's identity directory. Same convention the rest of
// daft will follow once we add daft auth on the client side.
const configSubdir = "daft/orchestrator"

// peersFile is the on-disk filename for the TOFU peer store, sitting
// next to cert.pem and key.pem under the orchestrator's config dir.
const peersFile = "peers.json"

// serveCmdName is the literal cobra command name used to register
// and invoke the long-running listener.
const serveCmdName = "serve"

// New builds the root cobra.Command. Stdin/stdout/stderr are passed
// in for testability.
func New(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "daft-orchestrator",
		Short:         "daft orchestrator: central control-plane for sandbox-hosts and clients",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)

	root.AddCommand(versionCommand())
	root.AddCommand(serveCommand())
	return root
}

// Execute is the binary's main entry point. Args is os.Args[1:].
func Execute(args []string, stdout, stderr io.Writer) error {
	cmd := New(stdout, stderr)
	cmd.SetArgs(args)
	return cmd.Execute()
}

func versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the daft-orchestrator version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version.Version)
			return err
		},
	}
}

func serveCommand() *cobra.Command {
	var (
		addr      string
		configDir string
	)
	cmd := &cobra.Command{
		Use:   serveCmdName,
		Short: "Start the orchestrator HTTPS listener",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			dir, err := resolveConfigDir(configDir)
			if err != nil {
				return err
			}
			id, err := loadOrGenerate(dir)
			if err != nil {
				return err
			}
			peers, err := identity.OpenPeerStore(filepath.Join(dir, peersFile))
			if err != nil {
				return err
			}
			srv, err := orchestrator.New(orchestrator.Config{
				Identity: id,
				Peers:    peers,
				Addr:     addr,
			})
			if err != nil {
				return err
			}
			if err := srv.Listen(); err != nil {
				return err
			}
			banner := fmt.Sprintf(
				"daft-orchestrator: listening on %s\n"+
					"daft-orchestrator: fingerprint %s\n"+
					"daft-orchestrator: peers in %s\n",
				srv.Addr(), id.Fingerprint(), peers.Path(),
			)
			if _, err := io.WriteString(cmd.OutOrStdout(), banner); err != nil {
				return err
			}
			return runServer(ctx, srv)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "0.0.0.0:8443",
		"TCP listen address (host:port)")
	cmd.Flags().StringVar(&configDir, "config-dir", "",
		"override the orchestrator config dir (default: $XDG_CONFIG_HOME/daft/orchestrator)")
	return cmd
}

// runServer blocks on Serve and triggers a graceful shutdown when
// ctx is cancelled. http.ErrServerClosed is the expected return on
// shutdown and is squashed.
func runServer(ctx context.Context, srv *orchestrator.Server) error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve()
	}()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// ctx is the cancelled signal context — its Done is already
		// closed, so a timeout-bound child suits Shutdown's "give in
		// flight requests this long to drain" semantic. Detaching
		// from the cancelled parent is intentional (linter:contextcheck).
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}

// resolveConfigDir returns the directory the orchestrator stores its
// identity + peer store in. Order: explicit --config-dir flag, then
// XDG-aware os.UserConfigDir + configSubdir.
func resolveConfigDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("orchestrator: locate config dir: %w", err)
	}
	return filepath.Join(base, configSubdir), nil
}

// loadOrGenerate is a thin wrapper around identity.LoadOrGenerate
// fixing the role to RoleOrchestrator.
func loadOrGenerate(dir string) (*identity.Identity, error) {
	return identity.LoadOrGenerate(dir, identity.RoleOrchestrator)
}
