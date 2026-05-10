// Package agent is the daft sandbox-agent. It runs as PID 1's child inside
// the sandbox VM, brings up vsock RPC, and supervises the workload
// container. In v0 it just prints a hello banner and blocks until its
// context is cancelled — enough to verify that the VM boots, the kernel
// hands control to user space, and our binary's stdout reaches the host
// serial console.
//
// As of v0.1 the agent also honours `daft.share=<tag>:<mountpoint>` on
// the kernel cmdline: it mounts the named virtio-fs share, reads the
// marker file, and reports the contents on the same console stream.
// Sharing is best-effort — failure does not bring the agent down.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/richiejp/darkaifactory/internal/oops"
)

// Banner is the line Run prints on start-up. The sandbox package matches
// against this exact string on the VM console.
const Banner = "hello world from daft-agent"

// ContainerBanner is the exact line cmd/daft-hello-container prints
// when run inside the sandbox VM. The host's banner-scanner watches for
// it (prefixed with ContainerReportPrefix) to confirm crun successfully
// launched the workload container.
const ContainerBanner = "hello world from container"

// ContainerReportPrefix tags lines the agent forwards from the
// container's stdout so the host can distinguish container output
// from kernel/agent log lines on the shared console. Mirrors
// ShareReportPrefix.
const ContainerReportPrefix = "container: "

// BundleCmdlineKey names the kernel cmdline parameter that asks the
// agent to crun-run an OCI bundle from the given absolute path inside
// the guest. Format: `daft.bundle=<absolute-path>`. The path is
// usually somewhere on the share mount (so the host can ship the
// bundle into the VM via virtiofs).
const BundleCmdlineKey = "daft.bundle"

// Run prints Banner to w, applies any `daft.share=...` cmdline directive
// (mounting the share and reporting via ShareReportPrefix), runs the
// `daft.bundle=...` container if set (forwarded via ContainerReportPrefix),
// then blocks until ctx is cancelled, returning ctx.Err(). It does not
// exit on its own: when the agent is PID 1's only child a return would
// let init exit, which panics the kernel. Lifetime is therefore
// controlled by ctx — Main wires it to SIGTERM/SIGINT.
func Run(ctx context.Context, w io.Writer) error {
	return runWith(ctx, w, runDeps{
		readCmdline: readKernelCmdline,
		share:       defaultShareDeps(),
		container:   defaultContainerDeps(),
	})
}

// runDeps bundles the side-effect surfaces of Run so unit tests can
// substitute in-memory fakes. Production wires the real Linux
// implementations via Run.
type runDeps struct {
	readCmdline func() (string, error)
	share       ShareDeps
	container   ContainerDeps
}

func runWith(ctx context.Context, w io.Writer, d runDeps) error {
	// Order matters: share + container lines go out *before* the
	// banner so the host can observe their results in the lines
	// preceding the banner. Banner appearance is the "agent is up,
	// all setup applied" signal — share + container are best-effort,
	// so a failure in either still lets the banner print.
	cmdline, _ := d.readCmdline()
	bundle, _, _ := ParseBundleCmdline(cmdline)
	applySharesFromCmdline(w, d, cmdline, bundle != "")
	runContainerFromCmdline(ctx, w, d, cmdline)
	if _, err := fmt.Fprintln(w, Banner); err != nil {
		return fmt.Errorf("agent: write banner: %w", err)
	}
	<-ctx.Done()
	return ctx.Err()
}

// applySharesFromCmdline parses every daft.share= token and mounts
// each share. When haveBundle is false (the v0 marker-demo path),
// the first share is mounted via ApplyShare so its hello.txt marker
// gets read and reported. When haveBundle is true (the run-with-
// image path), every share is mount-only — container output is the
// proof, and reading a non-existent marker would just spam noise.
//
// All failure paths print a single ShareReportPrefix line so the
// host scanner sees one prefix per failure mode.
func applySharesFromCmdline(w io.Writer, d runDeps, cmdline string, haveBundle bool) {
	if cmdline == "" {
		return
	}
	specs, err := ParseShareCmdlines(cmdline)
	if err != nil {
		writeReport(w, ShareReportPrefix, "cmdline: "+err.Error())
		return
	}
	for i, spec := range specs {
		if i == 0 && !haveBundle {
			ApplyShare(spec, d.share, w)
			continue
		}
		if err := MountShare(spec, d.share); err != nil {
			writeReport(w, ShareReportPrefix, err.Error())
		}
	}
}

// runContainerFromCmdline parses any daft.bundle token and execs crun
// against it. Failures are reported via ContainerReportPrefix; absence
// of the token is silently skipped so plain `daft sandbox hello` keeps
// working unchanged.
func runContainerFromCmdline(ctx context.Context, w io.Writer, d runDeps, cmdline string) {
	if cmdline == "" {
		return
	}
	bundle, ok, err := ParseBundleCmdline(cmdline)
	if err != nil {
		writeReport(w, ContainerReportPrefix, "cmdline: "+err.Error())
		return
	}
	if !ok {
		return
	}
	RunContainer(ctx, w, d.container, bundle)
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
	_, werr := fmt.Fprintln(stderr, "daft-agent:", err)
	oops.Must("agent: write Main error", werr)
	return 1
}
