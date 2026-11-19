package blackbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const MaxCapturedBytes = 64 << 10

type StepResult struct {
	Name           string `json:"name"`
	ExitCode       int    `json:"exit_code"`
	DurationMillis int64  `json:"duration_millis"`
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	Passed         bool   `json:"passed"`
	Failure        string `json:"failure,omitempty"`
}

type Result struct {
	Version  int          `json:"version"`
	Scenario string       `json:"scenario"`
	Passed   bool         `json:"passed"`
	Steps    []StepResult `json:"steps"`
}

type Executor func(context.Context, []string) (stdout, stderr string, exitCode int, err error)

func Run(ctx context.Context, scenario Scenario, execute Executor) (Result, error) {
	if err := scenario.Validate(); err != nil {
		return Result{}, err
	}
	if execute == nil {
		execute = executeCommand
	}
	result := Result{Version: Version, Scenario: scenario.Name, Passed: true, Steps: make([]StepResult, 0, len(scenario.Steps))}
	for _, step := range scenario.Steps {
		timeout := time.Duration(step.TimeoutSeconds) * time.Second
		if timeout == 0 {
			timeout = DefaultTimeoutSecs * time.Second
		}
		stepCtx, cancel := context.WithTimeout(ctx, timeout)
		started := time.Now()
		stdout, stderr, exitCode, runErr := execute(stepCtx, step.Argv)
		contextErr := stepCtx.Err()
		cancel()
		stepResult := StepResult{Name: step.Name, ExitCode: exitCode, DurationMillis: time.Since(started).Milliseconds(), Stdout: truncate(stdout), Stderr: truncate(stderr), Passed: true}
		failures := make([]string, 0)
		if runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded) {
			failures = append(failures, runErr.Error())
		}
		if contextErr != nil {
			failures = append(failures, "deadline exceeded")
		}
		if exitCode != step.WantExit {
			failures = append(failures, fmt.Sprintf("exit code %d, want %d", exitCode, step.WantExit))
		}
		for _, want := range step.StdoutContains {
			if !strings.Contains(stdout, want) {
				failures = append(failures, fmt.Sprintf("stdout missing %q", want))
			}
		}
		for _, want := range step.StderrContains {
			if !strings.Contains(stderr, want) {
				failures = append(failures, fmt.Sprintf("stderr missing %q", want))
			}
		}
		if len(failures) > 0 {
			stepResult.Passed, result.Passed = false, false
			stepResult.Failure = strings.Join(failures, "; ")
		}
		result.Steps = append(result.Steps, stepResult)
	}
	return result, nil
}

func executeCommand(ctx context.Context, argv []string) (stdoutText, stderrText string, exitCode int, runErr error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- Scenario validation fixes argv[0] to the installed vmjail client; remaining arguments are the purpose of the source-blind public CLI test.
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	exitCode = 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
			err = nil
		} else {
			exitCode = -1
		}
	}
	return stdout.String(), stderr.String(), exitCode, err
}

func truncate(value string) string {
	if len(value) <= MaxCapturedBytes {
		return value
	}
	return value[:MaxCapturedBytes]
}
