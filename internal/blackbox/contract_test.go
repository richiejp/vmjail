package blackbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

const validScenario = `{"version":1,"name":"help-smoke","steps":[{"name":"help","argv":["daft","--help"],"want_exit":0,"stdout_contains":["darkaifactory"]}]}`

func TestDecodeScenarioAndRun(t *testing.T) {
	scenario, err := DecodeScenario(strings.NewReader(validScenario))
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(t.Context(), scenario, func(_ context.Context, argv []string) (string, string, int, error) {
		if strings.Join(argv, " ") != "daft --help" {
			t.Fatalf("argv = %q", argv)
		}
		return "darkaifactory CLI", "", 0, nil
	})
	if err != nil || !result.Passed || len(result.Steps) != 1 || !result.Steps[0].Passed {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestRunReportsAssertionsAndDeadline(t *testing.T) {
	scenario, _ := DecodeScenario(strings.NewReader(validScenario))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	result, err := Run(ctx, scenario, func(ctx context.Context, _ []string) (string, string, int, error) {
		<-ctx.Done()
		return "wrong", "problem", 2, ctx.Err()
	})
	if err != nil || result.Passed || result.Steps[0].Failure == "" {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestRunRejectsInvalidAndTruncatesEvidence(t *testing.T) {
	if _, err := Run(t.Context(), Scenario{}, nil); err == nil {
		t.Fatal("invalid scenario accepted")
	}
	scenario, _ := DecodeScenario(strings.NewReader(validScenario))
	result, err := Run(t.Context(), scenario, func(context.Context, []string) (string, string, int, error) {
		return strings.Repeat("x", MaxCapturedBytes+10), "", 0, errors.New("launch failed")
	})
	if err != nil || len(result.Steps[0].Stdout) != MaxCapturedBytes || result.Passed {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestDecodeScenarioRejectsContractViolations(t *testing.T) {
	cases := []string{
		`{}`,
		strings.Replace(validScenario, `"version":1`, `"version":2`, 1),
		strings.Replace(validScenario, `"help-smoke"`, `"Bad Name"`, 1),
		strings.Replace(validScenario, `"steps":[{`, `"unknown":1,"steps":[{`, 1),
		strings.Replace(validScenario, `["daft","--help"]`, `["sh"]`, 1),
		strings.Replace(validScenario, `"want_exit":0`, `"want_exit":256`, 1),
		strings.Replace(validScenario, `"want_exit":0`, `"timeout_seconds":121,"want_exit":0`, 1),
		strings.Replace(validScenario, `"name":"help"`, `"name":""`, 1),
		strings.Replace(validScenario, `["daft","--help"]`, `["daft",""]`, 1),
		strings.Replace(validScenario, `"darkaifactory"`, `""`, 1),
		validScenario + `{}`,
	}
	for i, input := range cases {
		if _, err := DecodeScenario(strings.NewReader(input)); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if _, err := DecodeScenario(nil); err == nil {
		t.Fatal("nil scenario accepted")
	}
	if _, err := DecodeScenario(strings.NewReader(strings.Repeat(" ", MaxDocumentBytes+1))); err == nil {
		t.Fatal("oversized scenario accepted")
	}
}

func TestDecodeIssues(t *testing.T) {
	line := `{"id":"bb-1","category":"ux","title":"unclear output","status":"open","scenario":"help-smoke","expected":"actionable output","observed":"ambiguous output","reproduced":2,"evidence":["runs/one.json","runs/two.json"],"first_seen_version":"0.1.0"}`
	issues, err := DecodeIssues(strings.NewReader("\n" + line + "\n"))
	if err != nil || len(issues) != 1 {
		t.Fatalf("issues = %#v, %v", issues, err)
	}
	if _, err := DecodeIssues(strings.NewReader(strings.Replace(line, `"status":"open"`, `"status":"resolved"`, 1))); err != nil {
		t.Fatalf("resolved issue rejected: %v", err)
	}
	for _, mutation := range []string{
		strings.Replace(line, `"ux"`, `"security"`, 1),
		strings.Replace(line, `"reproduced":2`, `"reproduced":1`, 1),
		strings.Replace(line, `"status":"open"`, `"status":"invalid"`, 1),
		strings.Replace(line, `"runs/one.json"`, `"../one.json"`, 1),
		strings.Replace(line, `"runs/one.json"`, `"/one.json"`, 1),
		strings.Replace(line, `"expected":"actionable output"`, `"expected":""`, 1),
		strings.Replace(line, `"bb-1"`, `"Bad ID"`, 1),
		strings.Replace(line, `"runs/one.json","runs/two.json"`, `"runs/one.json"`, 1),
	} {
		if _, err := DecodeIssues(strings.NewReader(mutation)); err == nil {
			t.Fatalf("invalid issue accepted: %s", mutation)
		}
	}
	if _, err := DecodeIssues(strings.NewReader(line + "\n" + line)); err == nil {
		t.Fatal("duplicate issue accepted")
	}
	if _, err := DecodeIssues(nil); err == nil {
		t.Fatal("nil ledger accepted")
	}
}

func TestRunUsesRealCommandExecutor(t *testing.T) {
	scenario := Scenario{Version: Version, Name: "missing-daft", Steps: []Step{{Name: "missing", Argv: []string{Executable, "--help"}, WantExit: 0}}}
	t.Setenv("PATH", t.TempDir())
	result, err := Run(t.Context(), scenario, nil)
	if err != nil || result.Passed || result.Steps[0].ExitCode != -1 {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestRunAcceptsExpectedNonzeroExitAndStderr(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/daft"
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho expected-error >&2\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	scenario := Scenario{Version: Version, Name: "expected-error", Steps: []Step{{Name: "error", Argv: []string{"daft"}, WantExit: 7, StderrContains: []string{"expected-error"}}}}
	result, err := Run(t.Context(), scenario, nil)
	if err != nil || !result.Passed {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestRunDefaultTimeoutIsBounded(t *testing.T) {
	scenario, _ := DecodeScenario(strings.NewReader(validScenario))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	result, err := Run(ctx, scenario, func(ctx context.Context, _ []string) (string, string, int, error) {
		<-ctx.Done()
		return "", "", -1, ctx.Err()
	})
	if err != nil || result.Passed {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestDecodeReadersReportFailures(t *testing.T) {
	if _, err := DecodeScenario(failingReader{}); err == nil {
		t.Fatal("scenario read failure ignored")
	}
	if _, err := DecodeIssues(bytes.NewReader(bytes.Repeat([]byte{'x'}, MaxDocumentBytes+1))); err == nil {
		t.Fatal("oversized ledger accepted")
	}
	if _, err := DecodeIssues(failingReader{}); err == nil {
		t.Fatal("ledger read failure ignored")
	}
}
