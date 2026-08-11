// Package blackbox defines source-blind product-test scenarios and issue
// records. The formats deliberately describe only public daft CLI behaviour;
// implementation-aware regression tests are produced in a separate workflow.
package blackbox

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

const (
	Executable         = "daft"
	Version            = 1
	MaxDocumentBytes   = 1 << 20
	MaxSteps           = 64
	MaxArguments       = 64
	MaxArgumentBytes   = 16 << 10
	MaxAssertions      = 32
	MaxIssues          = 1024
	MaxEvidence        = 16
	MaxTextBytes       = 16 << 10
	MaxStepTimeoutSecs = 120
	DefaultTimeoutSecs = 35
)

var identifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

type Scenario struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	Steps   []Step `json:"steps"`
}

type Step struct {
	Name           string   `json:"name"`
	Argv           []string `json:"argv"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	WantExit       int      `json:"want_exit"`
	StdoutContains []string `json:"stdout_contains,omitempty"`
	StderrContains []string `json:"stderr_contains,omitempty"`
}

type Category string

const (
	CategoryCorrectness Category = "correctness"
	CategoryUX          Category = "ux"
	CategoryPerformance Category = "performance"
)

type Issue struct {
	ID               string   `json:"id"`
	Category         Category `json:"category"`
	Title            string   `json:"title"`
	Status           string   `json:"status"`
	Scenario         string   `json:"scenario"`
	Expected         string   `json:"expected"`
	Observed         string   `json:"observed"`
	Reproduced       int      `json:"reproduced"`
	Evidence         []string `json:"evidence"`
	FirstSeenVersion string   `json:"first_seen_version"`
}

func DecodeScenario(r io.Reader) (Scenario, error) {
	var scenario Scenario
	if err := decodeStrict(r, &scenario); err != nil {
		return Scenario{}, fmt.Errorf("blackbox: decode scenario: %w", err)
	}
	if err := scenario.Validate(); err != nil {
		return Scenario{}, err
	}
	return scenario, nil
}

func (s Scenario) Validate() error {
	if s.Version != Version {
		return fmt.Errorf("blackbox: scenario version must be %d", Version)
	}
	if !identifierPattern.MatchString(s.Name) {
		return errors.New("blackbox: scenario name must be a lowercase identifier")
	}
	if len(s.Steps) == 0 || len(s.Steps) > MaxSteps {
		return fmt.Errorf("blackbox: scenario must contain 1..%d steps", MaxSteps)
	}
	for i, step := range s.Steps {
		if err := step.validate(); err != nil {
			return fmt.Errorf("blackbox: step %d: %w", i, err)
		}
	}
	return nil
}

func (s Step) validate() error {
	if !identifierPattern.MatchString(s.Name) {
		return errors.New("name must be a lowercase identifier")
	}
	if len(s.Argv) == 0 || len(s.Argv) > MaxArguments || s.Argv[0] != Executable {
		return fmt.Errorf("argv must contain 1..%d arguments and start with daft", MaxArguments)
	}
	for _, arg := range s.Argv {
		if arg == "" || len(arg) > MaxArgumentBytes {
			return fmt.Errorf("arguments must be non-empty and at most %d bytes", MaxArgumentBytes)
		}
	}
	if s.TimeoutSeconds < 0 || s.TimeoutSeconds > MaxStepTimeoutSecs {
		return fmt.Errorf("timeout_seconds must be 0..%d", MaxStepTimeoutSecs)
	}
	if s.WantExit < 0 || s.WantExit > 255 {
		return errors.New("want_exit must be 0..255")
	}
	for _, assertions := range [][]string{s.StdoutContains, s.StderrContains} {
		if len(assertions) > MaxAssertions {
			return fmt.Errorf("assertion list exceeds %d entries", MaxAssertions)
		}
		for _, assertion := range assertions {
			if assertion == "" || len(assertion) > MaxTextBytes {
				return fmt.Errorf("assertions must be non-empty and at most %d bytes", MaxTextBytes)
			}
		}
	}
	return nil
}

func DecodeIssues(r io.Reader) ([]Issue, error) {
	if r == nil {
		return nil, errors.New("blackbox: issue reader is required")
	}
	limited := io.LimitReader(r, MaxDocumentBytes+1)
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), MaxDocumentBytes+1)
	issues := make([]Issue, 0)
	seen := map[string]struct{}{}
	for scanner.Scan() {
		if len(issues) == MaxIssues {
			return nil, fmt.Errorf("blackbox: issue ledger exceeds %d records", MaxIssues)
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var issue Issue
		if err := decodeStrict(bytes.NewReader(line), &issue); err != nil {
			return nil, fmt.Errorf("blackbox: issue line %d: %w", len(issues)+1, err)
		}
		if err := issue.Validate(); err != nil {
			return nil, fmt.Errorf("blackbox: issue line %d: %w", len(issues)+1, err)
		}
		if _, duplicate := seen[issue.ID]; duplicate {
			return nil, fmt.Errorf("blackbox: duplicate issue %q", issue.ID)
		}
		seen[issue.ID] = struct{}{}
		issues = append(issues, issue)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("blackbox: read issue ledger: %w", err)
	}
	return issues, nil
}

func (i Issue) Validate() error {
	if !identifierPattern.MatchString(i.ID) || !identifierPattern.MatchString(i.Scenario) {
		return errors.New("issue id and scenario must be lowercase identifiers")
	}
	if !slices.Contains([]Category{CategoryCorrectness, CategoryUX, CategoryPerformance}, i.Category) {
		return errors.New("category must be correctness, ux, or performance")
	}
	if i.Status != "open" {
		return errors.New("new source-blind issues must have status open")
	}
	for label, value := range map[string]string{"title": i.Title, "expected": i.Expected, "observed": i.Observed, "first_seen_version": i.FirstSeenVersion} {
		if strings.TrimSpace(value) == "" || len(value) > MaxTextBytes {
			return fmt.Errorf("%s must be non-empty and at most %d bytes", label, MaxTextBytes)
		}
	}
	if i.Reproduced < 2 {
		return errors.New("issue must be reproduced at least twice")
	}
	if len(i.Evidence) < 2 || len(i.Evidence) > MaxEvidence {
		return fmt.Errorf("issue must reference 2..%d evidence files", MaxEvidence)
	}
	for _, path := range i.Evidence {
		if path == "" || len(path) > MaxTextBytes || strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
			return errors.New("evidence paths must be relative and confined")
		}
	}
	return nil
}

func decodeStrict(r io.Reader, target any) error {
	if r == nil {
		return errors.New("reader is required")
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxDocumentBytes+1))
	if err != nil {
		return err
	}
	if len(data) > MaxDocumentBytes {
		return fmt.Errorf("document exceeds %d bytes", MaxDocumentBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}
