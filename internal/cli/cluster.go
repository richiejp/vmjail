package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/localai-org/darkaifactory/internal/localcluster"
)

const clusterCmdName = "cluster"

// newClusterCmd returns the `daft cluster` parent command: a single-box
// helper that brings up a co-located orchestrator + sandbox-host with
// the mTLS identities minted and cross-approved automatically, and tears
// the pair down again. It exists so scripts (and humans) don't hand-roll
// the auth dance + daemon lifecycle.
func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   clusterCmdName,
		Short: "Run a co-located orchestrator + sandbox-host on this box",
		Long: "Cluster sub-commands stand up a single-box daft control plane.\n" +
			"`up` mints + cross-approves the orchestrator, sandbox-host, and\n" +
			"client identities in one state dir, starts both daemons, and prints\n" +
			"shell-eval-able DAFT_CLUSTER_* lines (URL, client config dir,\n" +
			"fingerprints). `down` stops them. The same box that runs the\n" +
			"orchestrator usually runs sandboxes too, so this is the standard\n" +
			"way to bring the control plane up locally.",
	}
	cmd.AddCommand(newClusterUpCmd())
	cmd.AddCommand(newClusterDownCmd())
	return cmd
}

// newClusterUpCmd returns `daft cluster up`.
func newClusterUpCmd() *cobra.Command {
	var (
		stateDir     string
		addr         string
		orchBin      string
		sbxBin       string
		egressProxy  bool
		readyTimeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Bring up the co-located control plane",
		Long: "Mint + cross-approve identities in --state, start the orchestrator\n" +
			"and a sandbox-host as detached children, wait until both are ready,\n" +
			"and print eval-able DAFT_CLUSTER_* lines on stdout (progress on\n" +
			"stderr) so a script can `eval \"$(daft cluster up --state DIR)\"`.\n" +
			"Re-running tears down any prior cluster in the same state dir first.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := localcluster.Up(cmd.Context(), localcluster.Config{
				StateDir:        stateDir,
				Addr:            addr,
				OrchestratorBin: orchBin,
				SandboxdBin:     sbxBin,
				EgressProxy:     egressProxy,
				ReadyTimeout:    readyTimeout,
				Progress:        cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}
			printClusterEnv(cmd.OutOrStdout(), c)
			return nil
		},
	}
	cmd.Flags().StringVar(&stateDir, "state", "",
		"state directory for identities, logs, and the pidfile (required)")
	cmd.Flags().StringVar(&addr, "addr", localcluster.DefaultAddr,
		"orchestrator TCP listen address (host:port; port must be concrete)")
	cmd.Flags().StringVar(&orchBin, "orchestrator-bin", "",
		"path to daft-orchestrator (default: sibling of this binary, else PATH)")
	cmd.Flags().StringVar(&sbxBin, "sandboxd-bin", "",
		"path to daft-sandboxd (default: sibling of this binary, else PATH)")
	cmd.Flags().BoolVar(&egressProxy, "egress-proxy", false,
		"leave the sandbox-host egress proxy on (default off for local dev)")
	cmd.Flags().DurationVar(&readyTimeout, "ready-timeout", 0,
		"how long to wait for both daemons to be ready (default 60s)")
	_ = cmd.MarkFlagRequired("state")
	return cmd
}

// newClusterDownCmd returns `daft cluster down`.
func newClusterDownCmd() *cobra.Command {
	var (
		stateDir string
		purge    bool
	)
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop the co-located control plane",
		Long: "Read the pidfile under --state and SIGTERM (then SIGKILL) both\n" +
			"daemons' process groups. Identities + logs are kept unless --purge.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := localcluster.Down(stateDir, purge); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "cluster down: %s\n", stateDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&stateDir, "state", "",
		"state directory of the cluster to stop (required)")
	cmd.Flags().BoolVar(&purge, "purge", false,
		"also remove the state dir (identities, logs)")
	_ = cmd.MarkFlagRequired("state")
	return cmd
}

// printClusterEnv writes the shell-eval-able handle lines a caller
// sources to reach the cluster.
func printClusterEnv(w io.Writer, c *localcluster.Cluster) {
	_, _ = fmt.Fprintf(w,
		"DAFT_CLUSTER_URL=%s\n"+
			"DAFT_CLUSTER_CLIENT_DIR=%s\n"+
			"DAFT_CLUSTER_SANDBOX_FP=%s\n"+
			"DAFT_CLUSTER_ORCH_FP=%s\n"+
			"DAFT_CLUSTER_STATE=%s\n",
		c.URL, c.ClientConfigDir, c.SandboxFP, c.OrchestratorFP, c.StateDir)
}
