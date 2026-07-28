//go:build linux

package agent

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestPipeJobExecSeparatesOutputAndExit(t *testing.T) {
	e := &pipeJobExec{newCmd: func(ctx context.Context, _ JobExecRequest) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", "printf out; printf err >&2; exit 6")
	}}
	p, err := e.Start(t.Context(), JobExecRequest{Command: []string{"ignored"}})
	if err != nil {
		t.Fatal(err)
	}
	outDone := make(chan string, 1)
	errDone := make(chan string, 1)
	go func() { b, _ := io.ReadAll(p.Stdout()); outDone <- string(b) }()
	go func() { b, _ := io.ReadAll(p.Stderr()); errDone <- string(b) }()
	code, err := p.Wait()
	if err != nil || code != 6 || <-outDone != "out" || <-errDone != "err" {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if err := p.Cancel(); err != nil {
		t.Fatalf("Cancel after exit: %v", err)
	}
}

func TestPipeJobExecCancelKillsProcessGroup(t *testing.T) {
	e := &pipeJobExec{newCmd: func(ctx context.Context, _ JobExecRequest) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30")
	}}
	p, err := e.Start(t.Context(), JobExecRequest{Command: []string{"ignored"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Cancel(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _, _ = p.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after Cancel")
	}
}

func TestPipeJobExecStartFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		newCmd func(context.Context, JobExecRequest) *exec.Cmd
	}{
		{name: "stdout pipe", newCmd: func(ctx context.Context, _ JobExecRequest) *exec.Cmd {
			cmd := exec.CommandContext(ctx, "/bin/true")
			cmd.Stdout = &bytes.Buffer{}
			return cmd
		}},
		{name: "stderr pipe", newCmd: func(ctx context.Context, _ JobExecRequest) *exec.Cmd {
			cmd := exec.CommandContext(ctx, "/bin/true")
			cmd.Stderr = &bytes.Buffer{}
			return cmd
		}},
		{name: "start", newCmd: func(ctx context.Context, _ JobExecRequest) *exec.Cmd {
			return exec.CommandContext(ctx, "/definitely/missing")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := (&pipeJobExec{newCmd: tc.newCmd}).Start(t.Context(), JobExecRequest{}); err == nil {
				t.Fatal("Start succeeded")
			}
		})
	}
}

func TestPipeJobProcessUnexpectedWaitError(t *testing.T) {
	p := &pipeJobProcess{cmd: exec.Command("/bin/true"), cancel: func() {}}
	if code, err := p.Wait(); err == nil || code != 1 {
		t.Fatalf("Wait = %d, %v", code, err)
	}
}

func TestDefaultJobCmdCarriesExecutionContext(t *testing.T) {
	if newDefaultPipeJobExec() == nil {
		t.Fatal("default job executor is nil")
	}
	cmd := defaultJobCmd(t.Context(), JobExecRequest{
		Command: []string{"agent", testRunCommand}, Env: []string{testEnvAssignment}, User: testExecUser, Cwd: overlayMergedTest,
	})
	joined := strings.Join(cmd.Args, " ")
	for _, want := range []string{"crun exec", "--env " + testEnvAssignment, "--user " + testExecUser, "--cwd " + overlayMergedTest, ContainerID, "agent run"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
}
