// Package agent is the daft sandbox-agent. It runs as PID 1's child inside
// the sandbox VM, brings up vsock RPC, and supervises the workload
// container. In v0 it just prints a hello banner and blocks until its
// context is cancelled — enough to verify that the VM boots, the kernel
// hands control to user space, and our binary's stdout reaches the host
// serial console.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
)

// Banner is the line Run prints on start-up. The sandbox package matches
// against this exact string on the VM console.
const Banner = "hello world from daft-agent"

// Run prints Banner to w and then blocks until ctx is cancelled, returning
// ctx.Err(). It does not exit on its own: when the agent is PID 1's only
// child a return would let init exit, which panics the kernel. Lifetime is
// therefore controlled by ctx — Main wires it to SIGTERM/SIGINT.
func Run(ctx context.Context, w io.Writer) error {
	if _, err := fmt.Fprintln(w, Banner); err != nil {
		return fmt.Errorf("agent: write banner: %w", err)
	}
	<-ctx.Done()
	return ctx.Err()
}

// Main is the agent's process entry point, factored out of cmd/daft-agent
// so the lifecycle plumbing (signal context, error reporting, exit code)
// can be tested directly without spawning a subprocess. It returns the
// process exit code: 0 on graceful shutdown, 1 if Run reported a real
// error (a context-cancellation error from the signal handler is treated
// as graceful and yields 0).
func Main(parent context.Context, stdout, stderr io.Writer, signals ...os.Signal) int {
	ctx, stop := signal.NotifyContext(parent, signals...)
	defer stop()

	err := Run(ctx, stdout)
	if err == nil || errors.Is(err, context.Canceled) {
		return 0
	}
	_, _ = fmt.Fprintln(stderr, "daft-agent:", err)
	return 1
}
