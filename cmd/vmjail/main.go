// Command vmjail runs host executables inside a local microVM.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/richiejp/vmjail/internal/cgroupwrap"
	"github.com/richiejp/vmjail/internal/localrun"
	"github.com/richiejp/vmjail/internal/observability"
	"github.com/richiejp/vmjail/internal/sandbox"
	"github.com/richiejp/vmjail/internal/userns"
)

//nolint:gochecknoglobals // Startup boundaries are replaced by unit tests.
var (
	ensureDelegated = cgroupwrap.EnsureDelegated
	reexecDelegated = cgroupwrap.ReexecDelegated
	prepareSubtree  = observability.PrepareDelegatedSubtree
	namespaceRun    = mountNamespace
	runLocal        = localrun.Run
)

const (
	privateMountsEnv   = "VMJAIL_PRIVATE_MOUNTS"
	vmCgroupParentEnv  = "VMJAIL_VM_CGROUP_PARENT"
	namespaceWaitDelay = 5 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := mainCode(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

//nolint:contextcheck // Cobra passes ExecuteContext to the command callback.
func mainCode(ctx context.Context, args []string, in io.Reader, out, errout io.Writer) int {
	run := func(ctx context.Context, opts localrun.Options, in io.Reader, out, errout io.Writer) error {
		if os.Getenv(privateMountsEnv) == "1" {
			sandbox.SetVMCgroupParent(os.Getenv(vmCgroupParentEnv))
			return runLocal(ctx, opts, in, out, errout)
		}

		if err := ensureDelegated(errout); err != nil {
			return err
		}

		parent, err := prepareSubtree(sandbox.SupervisorLeaf)
		if err != nil {
			if errors.Is(err, syscall.EBUSY) {
				return errors.Join(err, reexecDelegated(errout))
			}

			return err
		}

		return namespaceRun(ctx, args, parent, in, out, errout)
	}

	// The command invokes run only after parsing and validation. Help and
	// version do not need a delegated cgroup or mount namespace.
	cmd := localrun.NewCommand(in, out, errout, run)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(ctx)
	var status *localrun.ExitError
	switch {
	case errors.As(err, &status):
		return status.Code
	case err != nil:
		// A diagnostic write failure does not change the startup exit status.
		_, _ = fmt.Fprintln(errout, "vmjail:", err)
		return 125
	default:
		return 0
	}
}

func namespaceCommand(ctx context.Context, args []string, parent string) *exec.Cmd {
	// #nosec G204 G702 -- /proc/self/exe is the running vmjail binary.
	cmd := exec.CommandContext(ctx, "/proc/self/exe", args...)
	cmd.Env = append(cmd.Environ(), privateMountsEnv+"=1", vmCgroupParentEnv+"="+parent)
	cmd.SysProcAttr = userns.RootMappedMountNamespace()

	// Keep foreground terminal access and allow the child to clean up its VM.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = namespaceWaitDelay
	return cmd
}

func mountNamespace(ctx context.Context, args []string, parent string, in io.Reader, out, errout io.Writer) error {
	cmd := namespaceCommand(ctx, args, parent)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errout
	return waitNamespace(cmd)
}

func waitNamespace(cmd *exec.Cmd) error {
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return &localrun.ExitError{Code: 128 + int(status.Signal())}
		}

		return &localrun.ExitError{Code: exit.ExitCode()}
	}

	if err != nil {
		return fmt.Errorf("start or wait for private mount namespace: %w", err)
	}

	return nil
}
