package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/richiejp/vmjail/internal/version"
)

// NewVersionCommand reports the build version for a CLI.
func NewVersionCommand(program string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the " + program + " version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if version.Version == "" {
				return errors.New("version.Version is empty (broken build)")
			}

			_, err := fmt.Fprintln(cmd.OutOrStdout(), version.Version)
			return err
		},
	}
}
