package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/richiejp/darkaifactory/internal/blackbox"
)

const (
	blackboxCmdName      = "blackbox"
	blackboxScenarioName = "scenario"
	blackboxIssuesName   = "issues"
	blackboxHistoryName  = "history"
	blackboxFileFlag     = "--file"
)

func newBlackboxCmd() *cobra.Command {
	cmd := &cobra.Command{Use: blackboxCmdName, Short: "Run and validate source-blind product tests"}
	cmd.AddCommand(newBlackboxScenarioCmd(), newBlackboxIssueCmd(), newBlackboxHistoryCmd())
	return cmd
}

func newBlackboxHistoryCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   blackboxHistoryName,
		Short: "Validate tracker-independent source-blind issue history",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			file, err := os.Open(path) // #nosec G304 -- explicit tester-owned history path.
			if err != nil {
				return fmt.Errorf("open issue history: %w", err)
			}
			history, decodeErr := blackbox.DecodeHistory(file)
			closeErr := file.Close()
			if decodeErr != nil {
				return decodeErr
			}
			if closeErr != nil {
				return fmt.Errorf("close issue history: %w", closeErr)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "valid history=%d\n", len(history))
			return err
		},
	}
	cmd.Flags().StringVar(&path, "file", "", "JSONL issue history")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

func newBlackboxScenarioCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   blackboxScenarioName,
		Short: "Run a bounded public-CLI scenario and print JSON evidence",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			file, err := os.Open(path) // #nosec G304 -- explicit tester-owned scenario path.
			if err != nil {
				return fmt.Errorf("open scenario: %w", err)
			}
			scenario, decodeErr := blackbox.DecodeScenario(file)
			closeErr := file.Close()
			if decodeErr != nil {
				return decodeErr
			}
			if closeErr != nil {
				return fmt.Errorf("close scenario: %w", closeErr)
			}
			result, err := blackbox.Run(cmd.Context(), scenario, nil)
			if err != nil {
				return err
			}
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
				return fmt.Errorf("write scenario evidence: %w", err)
			}
			if !result.Passed {
				return fmt.Errorf("blackbox scenario assertions failed")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "file", "", "scenario JSON file")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

func newBlackboxIssueCmd() *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   blackboxIssuesName,
		Short: "Validate an append-only source-blind issue ledger",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			file, err := os.Open(path) // #nosec G304 -- explicit tester-owned issue path.
			if err != nil {
				return fmt.Errorf("open issue ledger: %w", err)
			}
			issues, decodeErr := blackbox.DecodeIssues(file)
			closeErr := file.Close()
			if decodeErr != nil {
				return decodeErr
			}
			if closeErr != nil {
				return fmt.Errorf("close issue ledger: %w", closeErr)
			}
			if err := blackbox.ValidateArtifacts(filepath.Dir(path), issues); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "valid issues=%d\n", len(issues))
			return err
		},
	}
	cmd.Flags().StringVar(&path, "file", "", "JSONL issue ledger")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}
