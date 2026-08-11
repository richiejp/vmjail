package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlackboxCommandsRunAndValidateArtifacts(t *testing.T) {
	binDir := t.TempDir()
	daftPath := filepath.Join(binDir, "daft")
	if err := os.WriteFile(daftPath, []byte("#!/bin/sh\necho public-output\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	scenarioPath := filepath.Join(t.TempDir(), "scenario.json")
	scenario := `{"version":1,"name":"public-smoke","steps":[{"name":"help","argv":["daft","--help"],"want_exit":0,"stdout_contains":["public-output"]}]}`
	if err := os.WriteFile(scenarioPath, []byte(scenario), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runCmd(t, blackboxCmdName, blackboxScenarioName, blackboxFileFlag, scenarioPath)
	if err != nil || !strings.Contains(stdout, `"passed":true`) {
		t.Fatalf("scenario output = %q, %v", stdout, err)
	}

	artifactRoot := t.TempDir()
	ledgerPath := filepath.Join(artifactRoot, "issues.jsonl")
	issue := `{"id":"bb-1","category":"correctness","title":"wrong result","status":"open","scenario":"public-smoke","expected":"expected result","observed":"wrong result","reproduced":2,"evidence":["runs/one.json","runs/two.json"],"first_seen_version":"0.1.0"}`
	if err := os.MkdirAll(filepath.Join(artifactRoot, "scenarios"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(artifactRoot, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactRoot, "scenarios", "public-smoke.json"), []byte(scenario), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence := `{"version":1,"scenario":"public-smoke","passed":false,"steps":[{"name":"help","exit_code":0,"duration_millis":1,"stdout":"actual","stderr":"","passed":false,"failure":"stdout missing expected"}]}`
	for _, name := range []string{"one.json", "two.json"} {
		if err := os.WriteFile(filepath.Join(artifactRoot, "runs", name), []byte(evidence), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(ledgerPath, []byte(issue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = runCmd(t, blackboxCmdName, blackboxIssuesName, blackboxFileFlag, ledgerPath)
	if err != nil || stdout != "valid issues=1\n" {
		t.Fatalf("issue output = %q, %v", stdout, err)
	}
}

func TestBlackboxScenarioReturnsEvidenceOnAssertionFailure(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "daft"), []byte("#!/bin/sh\necho actual\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	path := filepath.Join(t.TempDir(), "scenario.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"name":"failure","steps":[{"name":"check","argv":["daft"],"want_exit":0,"stdout_contains":["expected"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := runCmd(t, blackboxCmdName, blackboxScenarioName, blackboxFileFlag, path)
	if err == nil || !strings.Contains(stdout, `"passed":false`) || !strings.Contains(stdout, "stdout missing") {
		t.Fatalf("scenario output = %q, %v", stdout, err)
	}
}

func TestBlackboxCommandsReportInvalidFiles(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	for _, args := range [][]string{{blackboxCmdName, blackboxScenarioName, blackboxFileFlag, missing}, {blackboxCmdName, blackboxIssuesName, blackboxFileFlag, missing}} {
		if _, _, err := runCmd(t, args...); err == nil {
			t.Fatalf("%v accepted missing file", args)
		}
	}
	invalid := filepath.Join(root, "invalid.json")
	if err := os.WriteFile(invalid, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{blackboxCmdName, blackboxScenarioName, blackboxFileFlag, invalid}, {blackboxCmdName, blackboxIssuesName, blackboxFileFlag, invalid}} {
		if _, _, err := runCmd(t, args...); err == nil {
			t.Fatalf("%v accepted malformed JSON", args)
		}
	}
	validLedger := filepath.Join(root, "issues.jsonl")
	issue := `{"id":"bb-1","category":"ux","title":"missing detail","status":"open","scenario":"missing","expected":"detail","observed":"absent","reproduced":2,"evidence":["runs/one.json","runs/two.json"],"first_seen_version":"dev"}`
	if err := os.WriteFile(validLedger, []byte(issue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCmd(t, blackboxCmdName, blackboxIssuesName, blackboxFileFlag, validLedger); err == nil {
		t.Fatal("issue ledger with missing artifacts accepted")
	}
}
