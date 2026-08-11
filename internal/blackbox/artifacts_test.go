package blackbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateArtifacts(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scenarios"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scenarios", "help-smoke.json"), []byte(validScenario), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence := `{"version":1,"scenario":"help-smoke","passed":false,"steps":[{"name":"help","exit_code":0,"duration_millis":1,"stdout":"","stderr":"","passed":false,"failure":"missing output"}]}`
	for _, name := range []string{"one.json", "two.json"} {
		if err := os.WriteFile(filepath.Join(root, "runs", name), []byte(evidence), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	issue := Issue{ID: "bb-1", Scenario: "help-smoke", Evidence: []string{"runs/one.json", "runs/two.json"}}
	if err := ValidateArtifacts(root, []Issue{issue}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		mutate func(Issue) Issue
	}{
		{"missing scenario", func(i Issue) Issue { i.Scenario = "missing"; return i }},
		{"duplicate evidence", func(i Issue) Issue { i.Evidence[1] = i.Evidence[0]; return i }},
		{"missing evidence", func(i Issue) Issue { i.Evidence[1] = "runs/missing.json"; return i }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			copyIssue := issue
			copyIssue.Evidence = append([]string(nil), issue.Evidence...)
			if err := ValidateArtifacts(root, []Issue{tc.mutate(copyIssue)}); err == nil {
				t.Fatal("invalid artifacts accepted")
			}
		})
	}
}

func TestValidateArtifactsRejectsMismatchedAndMalformedEvidence(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scenarios"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scenarios", "help-smoke.json"), []byte(validScenario), 0o600); err != nil {
		t.Fatal(err)
	}
	issue := Issue{ID: "bb-1", Scenario: "help-smoke", Evidence: []string{"runs/one.json", "runs/two.json"}}
	for name, content := range map[string]string{
		"malformed":      `{`,
		"successful-run": `{"version":1,"scenario":"help-smoke","passed":true,"steps":[]}`,
		"mismatch":       `{"version":1,"scenario":"other","passed":false,"steps":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			for _, path := range issue.Evidence {
				if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := ValidateArtifacts(root, []Issue{issue}); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	if err := ValidateArtifacts(filepath.Join(root, "missing"), nil); err == nil {
		t.Fatal("missing root accepted")
	}
	if hasFailedStep([]StepResult{{Passed: true}, {Passed: false}}) {
		t.Fatal("failure without diagnostic accepted")
	}
	if !strings.Contains(validScenario, "help-smoke") {
		t.Fatal("invalid test fixture")
	}
}
