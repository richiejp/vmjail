//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
)

type pipeJobExec struct {
	newCmd  func(ctx context.Context, req JobExecRequest) *exec.Cmd
	running func(context.Context) bool
}

func newDefaultPipeJobExec() *pipeJobExec {
	return &pipeJobExec{newCmd: defaultJobCmd, running: containerRunning}
}

func defaultJobCmd(ctx context.Context, req JobExecRequest) *exec.Cmd {
	argv := []string{crunExecSubcommand}
	for _, env := range req.Env {
		argv = append(argv, "--env", env)
	}
	if req.User != "" {
		argv = append(argv, "--user", req.User)
	}
	if req.Cwd != "" {
		argv = append(argv, "--cwd", req.Cwd)
	}
	argv = append(argv, ContainerID)
	argv = append(argv, req.Command...)
	// #nosec G204 — command and inherited container execution context are
	// authenticated orchestrator inputs, identical to the attach path.
	return exec.CommandContext(ctx, ContainerRuntime, argv...)
}

func (e *pipeJobExec) Start(ctx context.Context, req JobExecRequest) (JobProcess, error) {
	if e.running != nil && !e.running(ctx) {
		return nil, ErrContainerNotRunning
	}
	procCtx, cancel := context.WithCancel(ctx)
	cmd := e.newCmd(procCtx, req)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("agent: job stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("agent: job stderr pipe: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("agent: start job exec: %w", err)
	}
	return &pipeJobProcess{cmd: cmd, stdout: stdout, stderr: stderr, cancel: cancel}, nil
}

type pipeJobProcess struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser
	cancel context.CancelFunc

	cancelOnce sync.Once
}

func (p *pipeJobProcess) Stdout() io.Reader { return p.stdout }
func (p *pipeJobProcess) Stderr() io.Reader { return p.stderr }

func (p *pipeJobProcess) Cancel() error {
	var err error
	p.cancelOnce.Do(func() {
		p.cancel()
		if p.cmd.Process != nil {
			err = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			if errors.Is(err, syscall.ESRCH) {
				err = nil
			}
		}
	})
	return err
}

func (p *pipeJobProcess) Wait() (int32, error) {
	err := p.cmd.Wait()
	if err == nil {
		return int32(p.cmd.ProcessState.ExitCode()), nil //nolint:gosec // process exit codes fit int32.
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return int32(exitErr.ExitCode()), nil //nolint:gosec // process exit codes fit int32.
	}
	return 1, err
}
