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
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/localai-org/darkaifactory/internal/component"
	"github.com/localai-org/darkaifactory/internal/oops"
	"github.com/localai-org/darkaifactory/internal/trace"
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

// TraceReportPrefix tags console lines about the trace channel —
// cmdline parse errors, exporter Flush failures. Same separator
// convention as ShareReportPrefix / ContainerReportPrefix so the
// host scanner's regex doesn't have to special-case this stream.
const TraceReportPrefix = "trace: "

// Workspace round-trip in-VM helper paths and marker. The
// daft-hello-container binary uses these to read input the user-host
// pushed in and write output the host can later pull back — that's
// what the workspace-round-trip e2e test asserts on. Lives here
// rather than in the cmd/ binary so both producer and consumer
// import the same source of truth.
const (
	WorkspaceInPath  = "/workspace/in.txt"
	WorkspaceOutPath = "/workspace/out.txt"
	WorkspaceSuffix  = " (modified-in-sandbox)"
)

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
		dialVsock:   DialHostVsock,
		portForward: defaultPortForwardDeps(),
		attach:      defaultAttachDeps(),
		tap:         defaultTapDeps(),
	})
}

// runDeps bundles the side-effect surfaces of Run so unit tests can
// substitute in-memory fakes. Production wires the real Linux
// implementations via Run.
type runDeps struct {
	readCmdline func() (string, error)
	share       ShareDeps
	container   ContainerDeps
	// dialVsock dials the host on the given AF_VSOCK port. Returned
	// as a net.Conn so the trace package's StreamExporter can write
	// framed proto without caring about transport.
	dialVsock func(ctx context.Context, port uint32) (net.Conn, error)
	// portForward is the test-substitutable surface for the C1
	// in-VM port-forwarder: vsock.Listen + container dial.
	portForward portForwardDeps
	// attach is the test-substitutable surface for the D2b
	// in-VM attach listener: vsock.Listen + ExecAttach.
	attach attachDeps
	// tap is the test-substitutable surface for the D6c.2 slice-2b
	// tap bridge: /dev/net/tun open + netlink configure. Zero value
	// disables the tap path (slice-2a conn-only behaviour).
	tap tapDeps
}

func runWith(ctx context.Context, w io.Writer, d runDeps) error {
	cmdline, _ := d.readCmdline()
	tracer, ctx := startTracer(ctx, w, d, cmdline)

	ctx, bootSpan := trace.Start(ctx, "agent.boot",
		trace.WithAttributes(trace.KV{Key: "cmdline.len", Value: fmt.Sprintf("%d", len(cmdline))}))
	bundle, _, _ := ParseBundleCmdline(cmdline)
	applySharesFromCmdline(w, d, cmdline, bundle != "")
	// Port-forwarders and the attach listener must come up
	// BEFORE the container so a long-running workload like
	// `bash` doesn't block the agent's main goroutine forever
	// before any listener is ready. Each helper spawns its own
	// goroutines and returns immediately; the listeners live for
	// the agent's lifetime.
	startPortForwardersFromCmdline(ctx, w, d, cmdline)
	startAttachListenerFromCmdline(ctx, w, d, cmdline)
	startNetBridgeFromCmdline(ctx, w, d, cmdline)
	runContainerFromCmdline(ctx, w, d, cmdline)
	bootSpan.End()

	// Flush spans *before* the banner: the host's banner-scan is
	// what tells the VMM supervisor "tear down safe", so flushing
	// afterwards would race the agent's last vsock write against
	// VM shutdown.
	shutdownTracer(ctx, tracer, w)

	if _, err := fmt.Fprintln(w, Banner); err != nil {
		return fmt.Errorf("agent: write banner: %w", err)
	}
	<-ctx.Done()
	return ctx.Err()
}

// tracerShutdownTimeout caps how long Shutdown's final Flush can
// block. The agent ships spans best-effort over a fresh vsock conn
// per export; if the host listener disappeared, we don't want to
// hang the banner-print path on it.
const tracerShutdownTimeout = 2 * time.Second

// startTracer returns a Tracer (with a vsock StreamExporter wired up
// when daft.vsock.trace_port is set on the cmdline) and the ctx
// augmented with that tracer. Cmdline parse errors are reported
// on the console; span shipping is best-effort.
func startTracer(ctx context.Context, w io.Writer, d runDeps, cmdline string) (*trace.Tracer, context.Context) {
	tr := trace.New(component.Agent)
	port, ok, err := ParseVsockTracePortCmdline(cmdline)
	switch {
	case err != nil:
		writeReport(w, TraceReportPrefix, "cmdline: "+err.Error())
	case ok:
		dial := d.dialVsock
		tr.SetExporter(trace.NewStreamExporter(func(ctx context.Context) (net.Conn, error) {
			return dial(ctx, port)
		}))
	}
	return tr, trace.ContextWithTracer(ctx, tr)
}

// shutdownTracer Flushes the tracer with a bounded timeout. The
// parent's cancellation is detached (via WithoutCancel) so a
// SIGTERM-driven Main can't cut Flush short before the host has
// received the spans.
func shutdownTracer(parent context.Context, tr *trace.Tracer, w io.Writer) {
	if tr == nil {
		return
	}
	ctx, cancel := context.WithTimeout(
		context.WithoutCancel(parent), tracerShutdownTimeout,
	)
	defer cancel()
	if err := tr.Shutdown(ctx); err != nil {
		writeReport(w, TraceReportPrefix, err.Error())
	}
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
	overlays, err := ParseWorkspaceOverlayCmdlines(cmdline)
	if err != nil {
		writeReport(w, OverlayReportPrefix, "cmdline: "+err.Error())
		overlays = nil
	}
	overlaySet := make(map[string]bool, len(overlays))
	for _, mp := range overlays {
		overlaySet[mp] = true
	}

	specs, err := ParseShareCmdlines(cmdline)
	if err != nil {
		writeReport(w, ShareReportPrefix, "cmdline: "+err.Error())
		return
	}
	for i, spec := range specs {
		target := spec
		if overlaySet[spec.Mountpoint] {
			target.Mountpoint = OverlayLowerPath(spec.Mountpoint)
		}
		if i == 0 && !haveBundle {
			ApplyShare(target, d.share, w)
			continue
		}
		if err := MountShare(target, d.share); err != nil {
			writeReport(w, ShareReportPrefix, err.Error())
		}
	}
	for i, merged := range overlays {
		if err := MountOverlay(i, merged, d.share); err != nil {
			writeReport(w, OverlayReportPrefix, err.Error())
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
