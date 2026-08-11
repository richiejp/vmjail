package blackbox

import (
	"fmt"
	"os"
	"path/filepath"
)

// ValidateArtifacts proves that every ledger record names an executable
// scenario and at least two distinct failed runs of that exact scenario. Root
// confines symlink and traversal resolution to the source-blind artifact tree.
func ValidateArtifacts(rootPath string, issues []Issue) error {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("blackbox: open artifact root: %w", err)
	}
	defer func() { _ = root.Close() }()
	for issueIndex := range issues {
		issue := &issues[issueIndex]
		scenarioPath := filepath.Join("scenarios", issue.Scenario+".json")
		scenarioFile, err := root.Open(scenarioPath)
		if err != nil {
			return fmt.Errorf("blackbox: issue %s: open scenario: %w", issue.ID, err)
		}
		scenario, decodeErr := DecodeScenario(scenarioFile)
		closeErr := scenarioFile.Close()
		if decodeErr != nil {
			return fmt.Errorf("blackbox: issue %s: %w", issue.ID, decodeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("blackbox: issue %s: close scenario: %w", issue.ID, closeErr)
		}
		if scenario.Name != issue.Scenario {
			return fmt.Errorf("blackbox: issue %s: scenario document is named %q", issue.ID, scenario.Name)
		}
		seen := make(map[string]struct{}, len(issue.Evidence))
		for _, evidencePath := range issue.Evidence {
			if _, duplicate := seen[evidencePath]; duplicate {
				return fmt.Errorf("blackbox: issue %s: duplicate evidence %q", issue.ID, evidencePath)
			}
			seen[evidencePath] = struct{}{}
			evidenceFile, err := root.Open(evidencePath)
			if err != nil {
				return fmt.Errorf("blackbox: issue %s: open evidence: %w", issue.ID, err)
			}
			var result Result
			decodeErr := decodeStrict(evidenceFile, &result)
			closeErr := evidenceFile.Close()
			if decodeErr != nil {
				return fmt.Errorf("blackbox: issue %s: decode evidence: %w", issue.ID, decodeErr)
			}
			if closeErr != nil {
				return fmt.Errorf("blackbox: issue %s: close evidence: %w", issue.ID, closeErr)
			}
			if result.Version != Version || result.Scenario != issue.Scenario || result.Passed || !hasFailedStep(result.Steps) {
				return fmt.Errorf("blackbox: issue %s: evidence %q is not a failed run of scenario %q", issue.ID, evidencePath, issue.Scenario)
			}
		}
	}
	return nil
}

func hasFailedStep(steps []StepResult) bool {
	for _, step := range steps {
		if !step.Passed && step.Failure != "" {
			return true
		}
	}
	return false
}
