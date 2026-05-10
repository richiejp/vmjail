package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/richiejp/darkaifactory/internal/identity"
)

// authCmdName is the name of the parent `daft auth` command.
const authCmdName = "auth"

// configDirFlag is the persistent flag every auth subcommand reads
// to find the client identity + peer store. Default is XDG-aware.
const configDirFlag = "config-dir"

// listSubName is the cobra command name for `daft auth list`.
const listSubName = "list"

// newAuthCmd returns the `daft auth` subtree. The TOFU peer store
// and the local identity both live under --config-dir; the four
// child commands operate on those two artefacts.
func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   authCmdName,
		Short: "Manage approved peers (TOFU mTLS trust)",
		Long: "daft authenticates orchestrators and sandbox-hosts via mutual TLS\n" +
			"with TOFU peer pinning. The local identity (Ed25519 keypair +\n" +
			"self-signed leaf cert) plus the approved-peer set live under\n" +
			"$XDG_CONFIG_HOME/daft/client/ by default.\n\n" +
			"Approve a peer with `daft auth approve <fingerprint>` after\n" +
			"obtaining its fingerprint out-of-band; revoke with `daft auth\n" +
			"remove <fingerprint>`.",
	}
	cmd.PersistentFlags().String(configDirFlag, "",
		"override the daft client config dir (default: $XDG_CONFIG_HOME/daft/client)")

	cmd.AddCommand(newAuthShowCmd())
	cmd.AddCommand(newAuthListCmd())
	cmd.AddCommand(newAuthApproveCmd())
	cmd.AddCommand(newAuthRemoveCmd())
	return cmd
}

// configDirFromFlag is the small bit of glue that turns
// --config-dir (parent flag) into a resolved on-disk path. It's
// shared by every `daft auth` subcommand.
func configDirFromFlag(cmd *cobra.Command) (string, error) {
	override, err := cmd.Flags().GetString(configDirFlag)
	if err != nil {
		return "", err
	}
	return resolveClientDir(override)
}

// newAuthShowCmd: `daft auth show` — prints this client's own
// fingerprint and config-dir path. Used to copy/paste the
// fingerprint to the orchestrator's approval list.
func newAuthShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "show",
		Short:         "Print this client's fingerprint and config dir",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := configDirFromFlag(cmd)
			if err != nil {
				return err
			}
			id, err := loadOrGenerateClient(dir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(out, "fingerprint: %s\n", id.Fingerprint()); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(out, "config-dir:  %s\n", dir); err != nil {
				return err
			}
			return nil
		},
	}
}

// newAuthListCmd: `daft auth list` — prints the approved peer set,
// oldest first.
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
			peers, err := openClientPeerStore(dir)
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

// newAuthApproveCmd: `daft auth approve <fingerprint> [--label X]
// [--role R]` — adds (or replaces) a peer in the store. The
// fingerprint must be in the canonical sha256:AA:BB:... form, but
// ParseFingerprint also accepts bare hex.
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
				return fmt.Errorf("daft: unknown role %q (want client / orchestrator / sandbox-host)", role)
			}
			dir, err := configDirFromFlag(cmd)
			if err != nil {
				return err
			}
			peers, err := openClientPeerStore(dir)
			if err != nil {
				return err
			}
			peer := identity.Peer{
				Fingerprint: fp,
				Label:       strings.TrimSpace(label),
				Role:        identity.Role(role),
			}
			if err := peers.Add(peer); err != nil {
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

// newAuthRemoveCmd: `daft auth remove <fingerprint>` — deletes a
// peer entry. Idempotent: removing an unknown fingerprint succeeds
// quietly.
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
			peers, err := openClientPeerStore(dir)
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
