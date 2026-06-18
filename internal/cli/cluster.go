package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/localai-org/darkaifactory/internal/localcluster"
)

const clusterCmdName = "cluster"

// clusterUp is a seam over localcluster.Up so the `up` command's
// post-bring-up emit path is unit-testable without spawning real daemons.
//
//nolint:gochecknoglobals // test seam, swapped in unit tests.
var clusterUp = localcluster.Up

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
			"client identities in a default state dir, starts both daemons, and\n" +
			"records a handle there. A subsequent bare `daft workspace …` then\n" +
			"auto-discovers the running cluster — no env vars, no --orchestrator\n" +
			"or --config-dir flag. `down` stops it. The same box that runs the\n" +
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
		shellEnv     bool
		readyTimeout time.Duration
	)
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Bring up the co-located control plane",
		Long: "Mint + cross-approve identities in the state dir, start the\n" +
			"orchestrator and a sandbox-host as detached children, wait until\n" +
			"both are ready, and record a handle the daft client auto-discovers.\n" +
			"After this, a bare `daft workspace …` connects with no env var and\n" +
			"no flag. --state defaults to a per-user XDG location, so the common\n" +
			"case is just `daft cluster up`. Re-running tears down any prior\n" +
			"cluster in the same state dir first.\n\n" +
			"For scripting, --shell prints eval-able DAFT_CLUSTER_* lines on\n" +
			"stdout instead of the human summary, so a caller can\n" +
			"`eval \"$(daft cluster up --shell)\"`.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			stateDir, err := stateDirOrDefault(stateDir)
			if err != nil {
				return err
			}
			c, err := clusterUp(cmd.Context(), localcluster.Config{
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
			emitClusterHandle(cmd.OutOrStdout(), c, shellEnv)
			return nil
		},
	}
	cmd.Flags().StringVar(&stateDir, "state", "",
		"state directory for identities, logs, and the handle "+
			"(default: $XDG_STATE_HOME/daft/cluster)")
	cmd.Flags().StringVar(&addr, "addr", localcluster.DefaultAddr,
		"orchestrator TCP listen address (host:port; port must be concrete)")
	cmd.Flags().StringVar(&orchBin, "orchestrator-bin", "",
		"path to daft-orchestrator (default: sibling of this binary, else PATH)")
	cmd.Flags().StringVar(&sbxBin, "sandboxd-bin", "",
		"path to daft-sandboxd (default: sibling of this binary, else PATH)")
	cmd.Flags().BoolVar(&egressProxy, "egress-proxy", false,
		"leave the sandbox-host egress proxy on (default off for local dev)")
	cmd.Flags().BoolVar(&shellEnv, "shell", false,
		"print eval-able DAFT_CLUSTER_* lines instead of the human summary")
	cmd.Flags().DurationVar(&readyTimeout, "ready-timeout", 0,
		"how long to wait for both daemons to be ready (default 60s)")
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
			stateDir, err := stateDirOrDefault(stateDir)
			if err != nil {
				return err
			}
			if err := localcluster.Down(stateDir, purge); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "cluster down: %s\n", stateDir)
			return nil
		},
	}
	cmd.Flags().StringVar(&stateDir, "state", "",
		"state directory of the cluster to stop "+
			"(default: $XDG_STATE_HOME/daft/cluster)")
	cmd.Flags().BoolVar(&purge, "purge", false,
		"also remove the state dir (identities, logs)")
	return cmd
}

// stateDirOrDefault returns flag when set, else the well-known per-user
// cluster state dir. Centralises the `--state` default shared by up + down
// so a bare `daft cluster up`/`down` needs no flag.
func stateDirOrDefault(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	return localcluster.DefaultStateDir()
}

// emitClusterHandle writes the cluster handle after a successful `up`:
// the human summary by default, or the eval-able DAFT_CLUSTER_* lines when
// --shell is set (for scripts that need the fingerprints).
func emitClusterHandle(w io.Writer, c *localcluster.Cluster, shellEnv bool) {
	if shellEnv {
		printClusterEnv(w, c)
		return
	}
	printClusterSummary(w, c)
}

// printClusterSummary writes the human-facing handle a developer reads
// after `daft cluster up`: where the cluster is and that the client will
// find it automatically. The machine-readable form is behind --shell
// (printClusterEnv).
func printClusterSummary(w io.Writer, c *localcluster.Cluster) {
	_, _ = fmt.Fprintf(w,
		"daft cluster up\n"+
			"  orchestrator: %s\n"+
			"  sandbox-host: %s\n"+
			"  state:        %s\n\n"+
			"The daft client uses this cluster automatically — no env vars, no flags:\n"+
			"  daft workspace create --name <name>\n\n"+
			"Stop it with: daft cluster down\n",
		c.URL, c.SandboxFP, c.StateDir)
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
