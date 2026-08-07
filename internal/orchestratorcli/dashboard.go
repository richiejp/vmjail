package orchestratorcli

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
)

const (
	maxDashboardPasswordBytes = 72
	dashboardCmdName          = "dashboard"
	passwordHashCmdName       = "password-hash"
)

func dashboardCommand() *cobra.Command {
	dashboard := &cobra.Command{Use: dashboardCmdName, Short: "Manage dashboard authentication"}
	dashboard.AddCommand(&cobra.Command{
		Use:   passwordHashCmdName,
		Short: "Read a password from stdin and print its bcrypt hash",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			password, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), maxDashboardPasswordBytes+1))
			if err != nil {
				return fmt.Errorf("read dashboard password: %w", err)
			}
			password = bytes.TrimSuffix(bytes.TrimSuffix(password, []byte("\n")), []byte("\r"))
			if len(password) == 0 {
				return errors.New("dashboard password must not be empty")
			}
			if len(password) > maxDashboardPasswordBytes {
				return errors.New("dashboard password exceeds bcrypt's 72-byte limit")
			}
			hash, err := bcrypt.GenerateFromPassword(password, bcrypt.DefaultCost)
			if err != nil {
				return fmt.Errorf("hash dashboard password: %w", err)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(hash))
			return err
		},
	})
	return dashboard
}
