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
//
// A var rather than a const so tests can point execContainerLinux at
// a fake binary; production never reassigns it.
//
//nolint:gochecknoglobals // test-hook indirection for the crun binary path; never mutated in production.
var ContainerRuntime = "/bin/crun"

// ContainerID is the crun-side container identifier. crun keeps state
// under /run/<id>; we only run one container per VM in v0 so a
// constant is fine.
const ContainerID = "vmjail-container"

// ContainerDeps abstracts the subprocess surface RunContainer uses so
// tests can swap in a fake without spawning crun. Bundle is the only
// per-call varying input — runtime and id are package constants and
// closed over by the production Exec.
type ContainerDeps struct {
	Exec func(ctx context.Context, w io.Writer, bundle string) error
}

// RunContainer execs the configured OCI runtime against bundle,
// ferrying its stdout AND stderr to w with ContainerReportPrefix per
// line so the host can distinguish container output from agent /
// kernel logs on the shared console. crun's startup-failure
// diagnostics only appear on stderr, so folding it into the same
// stream is what gives "exec: wait: exit status 1" lines a useful
// preceding error.
//
// The caller decides whether container exit completes the VM or is fatal to a
// persistent environment. Errors are both reported and returned so lifecycle
// policy cannot accidentally discard them.
func RunContainer(ctx context.Context, w io.Writer, deps ContainerDeps, bundle string) error {
	if err := deps.Exec(ctx, w, bundle); err != nil {
		writeReport(w, ContainerReportPrefix, "exec: "+err.Error())
		return err
	}
	return nil
}

// prefixCopy reads r line by line and writes each line to w with prefix
// prepended and a trailing newline. Returns when r reports EOF or any
// other read error — the read error is intentionally swallowed because
// the producer (a child process) is responsible for surfacing it via
// its exit status.
//
// 256 KiB max-line cap matches scanForBanner's hostile-input ceiling.
// 4 KiB initial buffer is plenty for typical container output and
// kernel boot lines (<1 KiB); bufio grows it on demand up to the cap.
func prefixCopy(w io.Writer, r io.Reader, prefix string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4*1024), 256*1024)
	for scanner.Scan() {
		writeReport(w, prefix, scanner.Text())
	}
}

// execContainerLinux is the production implementation of
// ContainerDeps.Exec. It runs
// `<ContainerRuntime> run --no-pivot --bundle <bundle> <ContainerID>`
// and forwards stdout+stderr to w via prefixCopy.
//
// --no-pivot: skip pivot_root and chroot into the new rootfs instead.
// Required because the bundle is delivered via a virtiofs share —
// pivot_root needs the new root to be its own mountpoint, and a
// sub-directory of the share isn't. Trade-off: chroot is escapable
// from inside a privileged process, but the VM is the security
// boundary anyway.
func execContainerLinux(ctx context.Context, w io.Writer, bundle string) error {
	// #nosec G204 — execing the container runtime IS the agent's
	// job. ContainerRuntime is a package var the host orchestrator
	// could only swap by code change; bundle comes from the kernel
	// cmdline our own host code wrote. There is no untrusted input
	// here that gosec would help with.
	cmd := exec.CommandContext(ctx, ContainerRuntime,
		"run", "--no-pivot", "--bundle", bundle, ContainerID)
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
