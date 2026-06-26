package sandboxdcli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/richiejp/darkaifactory/internal/identity"
)

const (
	authCmdName       = "auth"
	configDirFlag     = "config-dir"
	listSubName       = "list"
	configDirFlagDesc = "override the daft-sandboxd config dir (default: $XDG_CONFIG_HOME/daft/sandboxd)"
)

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   authCmdName,
		Short: "Manage approved peers (TOFU mTLS trust)",
		Long: "daft-sandboxd authenticates peers (the user-host's daft client and\n" +
			"orchestrators) via mutual TLS with TOFU peer pinning. The local\n" +
			"identity (Ed25519 keypair + self-signed leaf cert) plus the\n" +
			"approved-peer set live under $XDG_CONFIG_HOME/daft/sandboxd/ by\n" +
			"default.",
	}
	cmd.PersistentFlags().String(configDirFlag, "", configDirFlagDesc)
	cmd.AddCommand(newAuthShowCmd())
	cmd.AddCommand(newAuthListCmd())
	cmd.AddCommand(newAuthApproveCmd())
	cmd.AddCommand(newAuthRemoveCmd())
	return cmd
}

func configDirFromFlag(cmd *cobra.Command) (string, error) {
	override, err := cmd.Flags().GetString(configDirFlag)
	if err != nil {
		return "", err
	}
	return resolveSandboxdDir(override)
}

func newAuthShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "show",
		Short:         "Print this sandbox-host's fingerprint and config dir",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := configDirFromFlag(cmd)
			if err != nil {
				return err
			}
			id, err := loadOrGenerateSandboxd(dir)
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
			dir, err := configDirFromFlag(cmd)
			if err != nil {
				return err
			}
			peers, err := openSandboxdPeerStore(dir)
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
				return fmt.Errorf("daft-sandboxd: unknown role %q (want client / orchestrator / sandbox-host)", role)
			}
			dir, err := configDirFromFlag(cmd)
			if err != nil {
				return err
			}
			peers, err := openSandboxdPeerStore(dir)
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
			dir, err := configDirFromFlag(cmd)
			if err != nil {
				return err
			}
			peers, err := openSandboxdPeerStore(dir)
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
