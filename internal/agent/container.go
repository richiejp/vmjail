package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
)

// ContainerRuntime is the absolute path to the OCI runtime the agent
// invokes (`crun` in v0). Lives in /bin/crun in the initramfs.
const ContainerRuntime = "/bin/crun"

// ContainerID is the crun-side container identifier. crun keeps state
// under /run/<id>; we only run one container per VM in v0 so a
// constant is fine.
const ContainerID = "daft-container"

// ContainerDeps abstracts the subprocess surface RunContainer uses so
// tests can swap in a fake without spawning crun.
type ContainerDeps struct {
	// Exec runs the container runtime synchronously, ferrying its
	// stdout to the supplied writer line by line and returning when
	// the container exits. Stderr is captured into the returned error
	// on non-zero exit.
	Exec func(ctx context.Context, w io.Writer, runtime, bundle, id string) error
}

// RunContainer execs the configured OCI runtime against bundle, ferrying
// its stdout to w with a ContainerReportPrefix per line so the host can
// distinguish container output from agent / kernel logs on the shared
// console.
//
// Errors are reported the same way (`container: <error>`) — a container
// failure does not bring the agent down, the same as a share failure.
func RunContainer(ctx context.Context, w io.Writer, deps ContainerDeps, bundle string) {
	if err := deps.Exec(ctx, w, ContainerRuntime, bundle, ContainerID); err != nil {
		_, _ = fmt.Fprintf(w, "%s%s\n", ContainerReportPrefix, "exec: "+err.Error())
	}
}

// prefixCopy reads r line by line and writes each line to w with prefix
// prepended and a trailing newline. Returns when r reports EOF or any
// other read error — the read error is intentionally swallowed because
// the producer (a child process) is responsible for surfacing it via
// its exit status.
func prefixCopy(w io.Writer, r io.Reader, prefix string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	for scanner.Scan() {
		_, _ = fmt.Fprintf(w, "%s%s\n", prefix, scanner.Text())
	}
}

// execContainerLinux is the production implementation of
// ContainerDeps.Exec. It runs `<runtime> run --bundle <bundle> <id>`
// and forwards the runtime's stdout AND stderr to w, prefixing each
// line with ContainerReportPrefix so the host scanner can tell
// container output from kernel/agent log lines on the shared
// console. Stderr is folded in because crun's diagnostics on
// startup failure (missing config field, broken namespace setup,
// etc.) only appear there; without it a failing container is
// reported only as a non-zero exit code with no useful context.
func execContainerLinux(ctx context.Context, w io.Writer, runtime, bundle, id string) error {
	// --no-pivot: skip pivot_root and chroot into the new rootfs
	// instead. We need this because the bundle is delivered via a
	// virtiofs share — pivot_root requires the new root to be its
	// own mountpoint, and a sub-directory of the share isn't.
	// Trade-off: chroot is escapable from inside a privileged
	// process, but the VM is the security boundary anyway, so this
	// is fine for our threat model.
	cmd := exec.CommandContext(ctx, runtime, "run", "--no-pivot", "--bundle", bundle, id)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	prefixCopy(w, out, ContainerReportPrefix)

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	return nil
}
