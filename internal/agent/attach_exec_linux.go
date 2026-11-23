//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/richiejp/vmjail/internal/pty"
)

// defaultAttachDeps wires the production AF_VSOCK listener and the
// crun-exec-with-PTY implementation of ExecAttach.
func defaultAttachDeps() attachDeps {
	return attachDeps{
		listenVsock: vsockListenGuest,
		exec:        newDefaultPtyAttachExec(),
	}
}

func defaultJobDeps() jobDeps {
	return jobDeps{listenVsock: vsockListenGuest, exec: newDefaultPipeJobExec()}
}

// ptyAttachExec is the production ExecAttach. It allocates a
// PTY pair, builds the exec command (default: `crun exec -t
// <ContainerID> <argv...>`), and wires the command's stdio to the
// PTY slave. Tests substitute newCmd to swap crun for a benign
// binary like /bin/cat so the PTY plumbing can be exercised
// without a running container.
type ptyAttachExec struct {
	newCmd  func(ctx context.Context, req AttachExecRequest) *exec.Cmd
	running func(context.Context) bool
}

func newDefaultPtyAttachExec() *ptyAttachExec {
	return &ptyAttachExec{newCmd: defaultAttachCmd, running: containerRunning}
}

// crunExecSubcommand and defaultAttachShell are split out so the
// argv-builder tests can assert against the same strings without
// the linter flagging the literals as goconst targets.
const (
	crunExecSubcommand = "exec"
	defaultAttachShell = "/bin/sh"
)

// defaultAttachCmd builds the production exec command. Empty
// req.Command falls back to /bin/sh -l so an attach with no
// arguments lands in a login shell — the v0 ergonomic for "give
// me a prompt inside the container."
//
// `-t` is intentionally NOT passed: with -t, crun opens
// /dev/ptmx in the container's mount namespace to allocate its
// own PTY, but the v0 hello-bundle (and many minimal bundles)
// mount /dev as an empty tmpfs with no ptmx node. The agent
// already owns a host-side PTY (see Start) and hands the slave
// fd in as the child's stdio, so the in-container process sees
// a real TTY without crun needing one of its own.
func defaultAttachCmd(ctx context.Context, req AttachExecRequest) *exec.Cmd {
	argv := []string{crunExecSubcommand}
	// Forward the container's env (read from config.json) so the
	// exec'd process sees the same environment as the entrypoint —
	// including the Phase-J egress proxy vars. crun exec starts from
	// a bare env otherwise.
	for _, e := range req.Env {
		argv = append(argv, "--env", e)
	}
	// Run the exec'd process as the container's declared user (read from
	// config.json), not crun exec's default of root — so a non-root image
	// is exec'd as its own uid:gid. Empty (root container) keeps the bare
	// default. Like --env, it's a flag and must precede the container id.
	if req.User != "" {
		argv = append(argv, "--user", req.User)
	}
	if req.Cwd != "" {
		argv = append(argv, "--cwd", req.Cwd)
	}
	argv = append(argv, ContainerID)
	if len(req.Command) == 0 {
		shell := req.Shell
		if shell == "" {
			shell = defaultAttachShell
		}
		argv = append(argv, shell, "-l")
	} else {
		argv = append(argv, req.Command...)
	}
	// #nosec G204 — argv is wholly composed of constants
	// (ContainerRuntime, ContainerID, the shell fallback) plus
	// req.Command, which the orchestrator authenticated at the
	// AttachSandbox RPC boundary. No untrusted host input flows
	// into this exec.
	return exec.CommandContext(ctx, ContainerRuntime, argv...)
}

// Start opens a fresh PTY, sets initial window size when the
// request is TTY-shaped, spawns the command with the slave as its
// controlling tty, and returns a ptyAttachProcess wrapping the
// master fd.
func (e *ptyAttachExec) Start(ctx context.Context, req AttachExecRequest) (AttachProcess, error) {
	if e.running != nil && !e.running(ctx) {
		return nil, ErrContainerNotRunning
	}
	cols, rows := uint32(0), uint32(0)
	if req.TTY {
		cols, rows = req.Cols, req.Rows
	}
	return startPTY(e.newCmd(ctx, req), cols, rows)
}

func startPTY(cmd *exec.Cmd, cols, rows uint32) (*ptyAttachProcess, error) {
	master, slave, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("agent: open pty: %w", err)
	}
	if cols > 0 && rows > 0 {
		// Failure here is non-fatal — the PTY still works at its
		// default 80x24, just sized wrong; a follow-up Resize
		// from the client will recover.
		_ = unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, attachWinsize(cols, rows))
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	// Setsid + Setctty: child becomes the session leader and the
	// slave fd is its controlling tty. Without these, the child
	// would inherit the agent's (init's) tty and signal-handling
	// (Ctrl-C, window-resize) wouldn't propagate through the PTY.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
	}
	if startErr := cmd.Start(); startErr != nil {
		_ = master.Close()
		_ = slave.Close()
		return nil, fmt.Errorf("agent: start exec: %w", startErr)
	}
	// Close the parent's copy of the slave fd: the child has its
	// own dup, and keeping the parent's open would block SIGHUP
	// delivery when the master closes.
	_ = slave.Close()
	return &ptyAttachProcess{cmd: cmd, master: master}, nil
}

// attachWinsize packs cols/rows into a Winsize, truncating to
// uint16 (the ioctl's wire width). Oversized values from the wire
// were already a no-op on a real terminal — truncation matches
// what xterm and friends do.
func attachWinsize(cols, rows uint32) *unix.Winsize {
	const maxDim = uint32(^uint16(0))
	c := min(cols, maxDim)
	r := min(rows, maxDim)
	return &unix.Winsize{Col: uint16(c), Row: uint16(r)} //nolint:gosec // G115: bounded by maxDim above
}

// ptyAttachProcess implements AttachProcess on top of a PTY master
// fd + a running exec.Cmd. Read/Write are passthroughs; Resize is
// TIOCSWINSZ on the master; Close closes the master (the child
// receives SIGHUP on its slave-side input and exits naturally,
// at which point cmd.Wait returns).
type ptyAttachProcess struct {
	cmd    *exec.Cmd
	master *os.File

	closeOnce sync.Once
	closeErr  error
}

func (p *ptyAttachProcess) Read(b []byte) (int, error)  { return p.master.Read(b) }
func (p *ptyAttachProcess) Write(b []byte) (int, error) { return p.master.Write(b) }

func (p *ptyAttachProcess) Resize(cols, rows uint32) error {
	if err := unix.IoctlSetWinsize(int(p.master.Fd()), unix.TIOCSWINSZ, attachWinsize(cols, rows)); err != nil {
		return fmt.Errorf("agent: TIOCSWINSZ: %w", err)
	}
	return nil
}

func (p *ptyAttachProcess) Close() error {
	p.closeOnce.Do(func() {
		// Closing the master sends SIGHUP to the foreground process
		// group, but workloads that ignore SIGHUP (and at least one
		// tested cat binary does) will pin Wait() indefinitely.
		// SIGKILL to the process group — negative PID, possible
		// because of Setsid above — is the actual termination
		// mechanism. ESRCH for an already-exited group is benign.
		p.closeErr = p.master.Close()
		if p.cmd.Process != nil {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		}
	})
	return p.closeErr
}

// Wait blocks until the exec'd command exits and returns its exit
// code. A non-zero status from cmd.Wait is folded into the
// returned code; a wholly unexpected wait error (no ExitError)
// surfaces as a non-nil error so the listener can report it on
// the Exit frame.
func (p *ptyAttachProcess) Wait() (int32, error) {
	return commandExitCode(p.cmd.Wait(), 1)
}

func commandExitCode(err error, failureCode int32) (int32, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return int32(exitErr.ExitCode()), nil //nolint:gosec // OS process exit status fits int32.
	}
	return failureCode, err
}
