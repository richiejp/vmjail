package orchestratorcli

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/localai-org/darkaifactory/internal/identity"
)

const (
	authCmdName = "auth"
	listSubName = "list"
	// authConfigDirFlag is the --config-dir flag name on the auth
	// subtree. Matches the literal serveCommand uses; named here because
	// the subcommands share it. (The _test const configDirFlag holds the
	// "--" arg form, hence the distinct name.)
	authConfigDirFlag = "config-dir"
)

// newAuthCmd builds the `daft-orchestrator auth` subtree: show / list /
// approve / remove over the orchestrator's TOFU peer store. The
// orchestrator needs this to bootstrap trust out-of-band — print its own
// fingerprint and approve the sandbox-hosts and clients that will dial
// it — before `serve` starts (serve loads the peer set once and does not
// retry). Mirrors `daft-sandboxd auth` and `daft auth`.
func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   authCmdName,
		Short: "Manage approved peers (TOFU mTLS trust)",
		Long: "daft-orchestrator authenticates peers (the user-host's daft client\n" +
			"and sandbox-hosts) via mutual TLS with TOFU peer pinning. The local\n" +
			"identity (Ed25519 keypair + self-signed leaf cert) plus the\n" +
			"approved-peer set live under $XDG_CONFIG_HOME/daft/orchestrator/ by\n" +
			"default.",
	}
	cmd.PersistentFlags().String(authConfigDirFlag, "",
		"override the orchestrator config dir (default: $XDG_CONFIG_HOME/daft/orchestrator)")
	cmd.AddCommand(newAuthShowCmd())
	cmd.AddCommand(newAuthListCmd())
	cmd.AddCommand(newAuthApproveCmd())
	cmd.AddCommand(newAuthRemoveCmd())
	return cmd
}

func authConfigDir(cmd *cobra.Command) (string, error) {
	override, err := cmd.Flags().GetString(authConfigDirFlag)
	if err != nil {
		return "", err
	}
	return resolveConfigDir(override)
}

func authPeerStore(dir string) (*identity.PeerStore, error) {
	return identity.OpenPeerStore(filepath.Join(dir, peersFile))
}

func newAuthShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "show",
		Short:         "Print this orchestrator's fingerprint and config dir",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := authConfigDir(cmd)
			if err != nil {
				return err
			}
			id, err := loadOrGenerate(dir)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"fingerprint: %s\nconfig-dir:  %s\n", id.Fingerprint(), dir)
			return err
		},
	}
}

func newAuthListCmd() *cobra.Command {
	return &cobra.Command{
		Use:           listSubName,
		Short:         "List approved peers",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := authConfigDir(cmd)
			if err != nil {
				return err
			}
			peers, err := authPeerStore(dir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			list := peers.List()
			if len(list) == 0 {
				_, err := fmt.Fprintln(out, "(no peers approved)")
				return err
			}
			for _, p := range list {
				label := p.Label
				if label == "" {
					label = "<unlabeled>"
				}
				role := string(p.Role)
				if role == "" {
					role = "<no-role>"
				}
				if _, err := fmt.Fprintf(out, "%s  %s  %s  %s\n",
					p.Fingerprint, role, label,
					p.ApprovedAt.UTC().Format(time.RFC3339)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newAuthApproveCmd() *cobra.Command {
	var (
		label string
		role  string
	)
	cmd := &cobra.Command{
		Use:           "approve <fingerprint>",
		Short:         "Approve a peer by fingerprint (TOFU)",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fp, err := identity.ParseFingerprint(args[0])
			if err != nil {
				return err
			}
			if role != "" && !identity.Role(role).Valid() {
				return fmt.Errorf("daft-orchestrator: unknown role %q (want client / orchestrator / sandbox-host)", role)
			}
			dir, err := authConfigDir(cmd)
			if err != nil {
				return err
			}
			peers, err := authPeerStore(dir)
			if err != nil {
				return err
			}
			if err := peers.Add(identity.Peer{
				Fingerprint: fp,
				Label:       strings.TrimSpace(label),
				Role:        identity.Role(role),
			}); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "approved %s\n", fp)
			return err
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "human-readable label for this peer")
	cmd.Flags().StringVar(&role, "role", "",
		"peer role: client | orchestrator | sandbox-host")
	return cmd
}

func newAuthRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "remove <fingerprint>",
		Short:         "Remove an approved peer",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fp, err := identity.ParseFingerprint(args[0])
			if err != nil {
				return err
			}
			dir, err := authConfigDir(cmd)
			if err != nil {
				return err
			}
			peers, err := authPeerStore(dir)
			if err != nil {
				return err
			}
			if err := peers.Remove(fp); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "removed %s\n", fp)
			return err
		},
	}
}
