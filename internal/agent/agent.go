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
	"strings"
	"sync"
	"time"

	"github.com/richiejp/darkaifactory/internal/component"
	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/trace"
)

// Banner is the line Run prints on start-up. The sandbox package matches
// against this exact string on the VM console.
const Banner = "hello world from daft-agent"

const traceAttrVsockPort = "vsock_port"

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
// exporter Flush failures. Same separator
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

// Run prints Banner to w, applies the per-VM settings fetched over the
// vsock control channel (shares mounted and reported via ShareReportPrefix,
// the container run if a bundle is set, forwarded via ContainerReportPrefix),
// then blocks until ctx is cancelled, returning ctx.Err(). It does not
// exit on its own: when the agent is PID 1's only child a return would
// let init exit, which panics the kernel. Lifetime is therefore
// controlled by ctx — Main wires it to SIGTERM/SIGINT.
func Run(ctx context.Context, w io.Writer) error {
	return runWith(ctx, w, runDeps{
		openControl: func(ctx context.Context) (controlChannel, error) {
			cc, err := OpenControlChannel(ctx, DialHostVsock)
			if err != nil {
				return nil, err
			}
			return cc, nil
		},
		share:       defaultShareDeps(),
		container:   defaultContainerDeps(),
		dialVsock:   DialHostVsock,
		portForward: defaultPortForwardDeps(),
		attach:      defaultAttachDeps(),
		job:         defaultJobDeps(),
		secret:      defaultSecretDeps(),
		tap:         defaultTapDeps(),
		opensnoop:   defaultOpensnoopDeps(),
		execsnoop:   defaultExecsnoopDeps(),
		sleepsnoop:  defaultSleepsnoopDeps(),
		iosnoop:     defaultIosnoopDeps(),
		syscalls:    defaultSyscallDumpDeps(),
		summary:     defaultSummaryDeps(),
	})
}

// controlChannel is the agent's open control connection to the host: it
// carries the fetched GuestConfig and lets the agent stream status
// events back. *ControlChannel is the production implementation; tests
// inject a fake. Modelled as an interface so unit tests don't need a
// real vsock conn.
type controlChannel interface {
	Config() *agentv1.GuestConfig
	SendEvent(agentv1.AgentEvent_Kind) error
	Close() error
}

// runDeps bundles the side-effect surfaces of Run so unit tests can
// substitute in-memory fakes. Production wires the real Linux
// implementations via Run.
type runDeps struct {
	// cfg is the GuestConfig fetched over the control channel, set by
	// runWith after the handshake. The start* helpers read every per-VM
	// setting (bundle, shares, port-forwards, observe paths, feature
	// toggles) from it. Nil when the control channel was unreachable —
	// the vsock-unreachable degrade, where the agent is banner-only.
	cfg *agentv1.GuestConfig
	// openControl dials the host's per-VM config/event server, performs
	// the handshake, and returns the open control channel (config +
	// event writer). A non-nil error means the channel was unreachable
	// (no vsock device, AF_VSOCK seccomp-denied, wedged listener) or the
	// handshake failed; the agent then degrades to a banner-only boot
	// (cfg nil, every feature off). Zero value (nil) skips the channel
	// entirely — tests that exercise the no-config path leave it unset.
	openControl func(context.Context) (controlChannel, error)
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
	// job is the non-PTY, output-separated command listener used by
	// detached Node jobs. It shares attach_enabled but has its own port and
	// framing so interactive PTY traffic cannot be confused with jobs.
	job jobDeps
	// secret is the credential-only listener and tmpfs store. It is kept
	// separate from jobs so arbitrary exec traffic never handles secrets.
	secret secretDeps
	// tap is the test-substitutable surface for the D6c.2 slice-2b
	// tap bridge: /dev/net/tun open + netlink configure. Zero value
	// disables the tap path (slice-2a conn-only behaviour).
	tap tapDeps
	// opensnoop is the test-substitutable surface for the Tier-2
	// open-snoop eBPF gadget: open the guest trace file + drain the
	// gadget. Zero value (run == nil) disables it (the BPF load needs
	// CAP_BPF, which make check lacks).
	opensnoop opensnoopDeps
	// execsnoop is the test-substitutable surface for the Tier-2 exec-snoop
	// eBPF gadget, parallel to opensnoop. Zero value disables it.
	execsnoop execsnoopDeps
	// sleepsnoop is the test-substitutable surface for the Tier-2 sleep-snoop
	// eBPF gadget, parallel to opensnoop. Zero value disables it.
	sleepsnoop sleepsnoopDeps
	// iosnoop is the test-substitutable surface for the Tier-2 io-snoop eBPF
	// gadget, parallel to opensnoop. Zero value disables it.
	iosnoop iosnoopDeps
	// syscalls is the test-substitutable surface for the boot-time syscall-list
	// dump (enumerate tracefs → write the available-syscalls file). Not a
	// gadget; runs once at boot. Zero value (listEnter == nil) disables it.
	syscalls syscallDumpDeps
	// summary is the test-substitutable surface for the build-progress
	// summary writer (clock + tick interval + atomic file write). Zero
	// value (write == nil) disables it. Fed by the open-snoop, exec-snoop, and
	// sleep-snoop aggregators, so it only produces output when at least one is
	// enabled.
	summary summaryDeps
}

func runWith(ctx context.Context, w io.Writer, d runDeps) error {
	// Open the vsock control channel: fetch this VM's settings and keep
	// the connection for streaming status events back. On any failure
	// the agent degrades — ctrl and cfg are nil, every setting is
	// unset, and the console banner is all that remains (the sanctioned
	// vsock-unreachable degrade, e.g. a nested check-container with no
	// device of its own).
	ctrl := openControlChannel(ctx, w, d)
	var cfg *agentv1.GuestConfig
	if ctrl != nil {
		cfg = ctrl.Config()
		d.cfg = cfg
		defer func() { _ = ctrl.Close() }()
	}
	tracer, ctx := startTracer(ctx, w, d)

	ctx, bootSpan := trace.Start(ctx, "agent.boot")
	bundle := resolveBundle(w, cfg)
	applyShares(w, d, cfg, bundle != "")
	secretCleanup, err := startSecretListener(ctx, w, d)
	if err != nil {
		return err
	}
	defer secretCleanup()
	// Port-forwarders and the attach listener must come up
	// BEFORE the container so a long-running workload like
	// `bash` doesn't block the agent's main goroutine forever
	// before any listener is ready. Each helper spawns its own
	// goroutines and returns immediately; the listeners live for
	// the agent's lifetime.
	startPortForwarders(ctx, w, d)
	startAttachListener(ctx, w, d)
	startJobListener(ctx, w, d)
	// postStart runs the resolved devcontainer lifecycle command inside the
	// container after it comes up; it waits for "running" on its own
	// goroutine and returns immediately, so it never blocks the boot before
	// runContainer below.
	startPostStart(ctx, w, d)
	if startNetBridge(ctx, w, d) != nil && bundle != "" {
		// The container shares the VM's init netns, so it inherits
		// tap0 + the default route. resolv.conf is the only missing
		// piece for DNS to resolve through the sandboxnet gateway.
		WriteContainerResolvConf(w, bundle)
	}
	// Put /sys/fs/cgroup on v2 before crun runs so it manages the
	// container under the unified hierarchy (see ensureCgroupV2), and
	// lift the guest's pristine-low rlimits to a normal-host level so the
	// container inherits them (see raiseGuestRlimits) — both are
	// guest-environment normalisation that must run before the container.
	if bundle != "" {
		ensureCgroupV2(w)
		raiseGuestRlimits(w)
	}
	// Dump the guest kernel's available syscall list to the observe state
	// folder (one-shot; the share was mounted by applyShares above) so a
	// user can always discover what is traceable inside the microvm.
	startSyscallDump(w, d)
	// Attach the open-snoop + exec-snoop + sleep-snoop + io-snoop gadgets
	// before the container so their tracepoints are live for the workload's
	// first syscalls, then start the build-progress summary as a live view over
	// whichever gadgets came up. Each start* returns a readiness channel that
	// closes once the gadget has attached (or given up).
	osAgg, osReady := startOpensnoop(ctx, w, d)
	exAgg, exReady := startExecsnoop(ctx, w, d)
	slAgg, slReady := startSleepsnoop(ctx, w, d)
	ioAgg, ioReady := startIosnoop(ctx, w, d)
	startSummaryWriter(ctx, w, d, osAgg, exAgg, slAgg, ioAgg)
	// Hold the container until every started gadget has either attached or
	// given up, so a healthy gadget's tracepoints are live before the
	// workload's first syscalls rather than racing them (the dropped-early-
	// events flake). A gadget that failed to attach releases the barrier too —
	// the trace is simply absent for it; observability is never a correctness
	// gate. Bounded so a wedged gadget never blocks the boot. The barrier only
	// exists to order against the container, so a share-only boot (no bundle)
	// skips it rather than risk stalling on a wedged gadget that protects
	// nothing — the gadgets still attach in the background.
	if bundle != "" {
		awaitGadgetsAttached(ctx, w, osReady, exReady, slReady, ioReady)
	}
	containerErr := runContainer(ctx, w, d, bundle)
	bootSpan.End()
	if cfg.GetContainerPersistent() && bundle != "" {
		if err := ctx.Err(); err != nil {
			return err
		}
		if containerErr != nil {
			return fmt.Errorf("agent: persistent container stopped: %w", containerErr)
		}
		return errors.New("agent: persistent container exited unexpectedly")
	}

	// Flush spans before signalling completion so the host has the boot
	// trace in hand when it tears the VM down on the boot-complete event.
	shutdownTracer(ctx, tracer, w)

	// The banner is now a plain console liveness print (and the
	// degrade-fallback sentinel for a host with no control channel) —
	// VM status flows over the control channel as a boot-complete event.
	if _, err := fmt.Fprintln(w, Banner); err != nil {
		return fmt.Errorf("agent: write banner: %w", err)
	}
	signalBootComplete(w, ctrl)
	<-ctx.Done()
	return ctx.Err()
}

// GadgetReportPrefix labels console lines about the observe gadgets as a group
// rather than one named gadget — currently only the attach-barrier timeout.
const GadgetReportPrefix = "observe: "

// gadgetAttachTimeout bounds awaitGadgetsAttached. Gadgets normally attach in
// milliseconds, and a failed attach releases its barrier immediately (its
// drain goroutine returns), so only a pathologically wedged BPF load can reach
// this. When it does, booting the workload without the trace beats hanging the
// VM — observability is never a correctness gate.
//
//nolint:gochecknoglobals // test-hook timeout; mutated only in tests.
var gadgetAttachTimeout = 5 * time.Second

// spawnGadgetDrain runs a gadget's drain loop in a goroutine and returns a
// channel that closes once the gadget has attached — drain calls the onReady it
// is handed the instant its tracepoints are live — or the goroutine exits,
// whichever comes first. The sync.Once makes the two paths idempotent: a failed
// attach never calls onReady, so the deferred signalReady closes the channel on
// return instead, ensuring awaitGadgetsAttached can never block on a gadget
// that gave up. f (the trace file) is closed when the loop returns; a mid-run
// error while ctx is still live is reported under prefix.
func spawnGadgetDrain(ctx context.Context, w io.Writer, prefix string, f io.Closer, drain func(onReady func()) error) <-chan struct{} {
	ready := make(chan struct{})
	go func() {
		defer func() { _ = f.Close() }()
		var once sync.Once
		signalReady := func() { once.Do(func() { close(ready) }) }
		defer signalReady()
		if err := drain(signalReady); err != nil && ctx.Err() == nil {
			writeReport(w, prefix, err.Error())
		}
	}()
	return ready
}

// awaitGadgetsAttached blocks until every non-nil ready channel has closed —
// each started gadget has attached (its tracepoints live) or its drain
// goroutine has given up — or a single shared deadline elapses, whichever
// first. For a healthy gadget this is the happens-before that keeps the
// workload's first syscalls from outrunning attach (the lost-early-events
// flake); a gadget that failed to attach simply has no trace. nil channels
// (gadgets that never started) are skipped; a deadline overrun is reported
// once and the boot proceeds best-effort.
func awaitGadgetsAttached(ctx context.Context, w io.Writer, readies ...<-chan struct{}) {
	timer := time.NewTimer(gadgetAttachTimeout)
	defer timer.Stop()
	for _, ready := range readies {
		if ready == nil {
			continue
		}
		// Prefer an already-signalled gadget. select picks among ready cases at
		// random, so a plain three-way select could pick a fired timer.C over an
		// already-closed ready and misreport an attached gadget as timed out.
		// This fast path takes the close without ever consulting the timer; only
		// a gadget still genuinely un-signalled here falls through to wait.
		select {
		case <-ready:
			continue
		default:
		}
		select {
		case <-ready:
		case <-ctx.Done():
			return
		case <-timer.C:
			writeReport(w, GadgetReportPrefix, "tracepoint attach timed out; workload may race the trace")
			return
		}
	}
}

// openControlChannel opens the control channel via d.openControl (when
// wired) and reports any failure on the console best-effort. A nil
// return — no dep wired, or a handshake failure — means "no channel":
// cfg stays nil, every feature stays off, and the banner becomes the
// host's only completion signal. The report is deliberately not
// fatal (unlike writeReport's oops.Must): a channel failure is already a
// graceful degrade (the VM may simply have no vsock device, the off-VM
// unit-test case), so a broken console must not escalate to an exit.
func openControlChannel(ctx context.Context, w io.Writer, d runDeps) controlChannel {
	if d.openControl == nil {
		return nil
	}
	ctrl, err := d.openControl(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(w, "%s%s\n", ConfigReportPrefix, err.Error())
		return nil
	}
	return ctrl
}

// signalBootComplete sends the boot-complete event over the control
// channel so the host's one-shot harness can return without scanning the
// console. No-op when the channel is absent (degraded boot). A send
// failure is reported best-effort — the agent is past its work and must
// not abort.
func signalBootComplete(w io.Writer, ctrl controlChannel) {
	if ctrl == nil {
		return
	}
	if err := ctrl.SendEvent(agentv1.AgentEvent_BOOT_COMPLETE); err != nil {
		_, _ = fmt.Fprintf(w, "%ssend boot-complete: %s\n", ConfigReportPrefix, err.Error())
	}
}

// resolveBundle picks the OCI bundle path to run: the GuestConfig's
// bundle. Returns "" (no container) when none is set, or when the value
// is malformed (reported on the console). cfg may be nil.
func resolveBundle(w io.Writer, cfg *agentv1.GuestConfig) string {
	b := cfg.GetBundle()
	if b == "" {
		return ""
	}
	if !strings.HasPrefix(b, "/") {
		writeReport(w, ContainerReportPrefix, "config: bundle path "+b+" must be absolute")
		return ""
	}
	return b
}

// runContainer crun-runs the resolved bundle, forwarding the container's
// stdout via ContainerReportPrefix. A "" bundle (no container requested)
// is silently skipped so a share-only boot keeps working.
func runContainer(ctx context.Context, w io.Writer, d runDeps, bundle string) error {
	if bundle == "" {
		return nil
	}
	return RunContainer(ctx, w, d.container, bundle)
}

// tracerShutdownTimeout caps how long Shutdown's final Flush can
// block. The agent ships spans best-effort over a fresh vsock conn
// per export; if the host listener disappeared, we don't want to
// hang the banner-print path on it.
const tracerShutdownTimeout = 2 * time.Second

// startTracer returns a Tracer (with a vsock StreamExporter wired up to
// the fixed DefaultTraceVsockPort when the GuestConfig's trace_enabled bit
// is set) and the ctx augmented with that tracer. Span shipping is
// best-effort.
func startTracer(ctx context.Context, w io.Writer, d runDeps) (*trace.Tracer, context.Context) {
	tr := trace.New(component.Agent)
	if d.cfg.GetTraceEnabled() {
		dial := d.dialVsock
		tr.SetExporter(trace.NewStreamExporter(func(ctx context.Context) (net.Conn, error) {
			return dial(ctx, DefaultTraceVsockPort)
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

// applyShares mounts each virtio-fs share carried in the GuestConfig,
// redirecting any share whose overlay bit is set through a tmpfs overlay.
//
// When haveBundle is false (the v0 marker-demo path), the first share
// is mounted via ApplyShare so its hello.txt marker gets read and
// reported. When haveBundle is true (the run-with-image path), every
// share is mount-only — container output is the proof, and reading a
// non-existent marker would just spam noise.
//
// All failure paths print a single ShareReportPrefix line so the
// host scanner sees one prefix per failure mode.
func applyShares(w io.Writer, d runDeps, cfg *agentv1.GuestConfig, haveBundle bool) {
	// Shares and their overlay flags travel together: the overlays come
	// from each Share's overlay bit.
	specs := configShareSpecs(cfg)
	overlays := configOverlays(cfg)
	overlaySet := make(map[string]bool, len(overlays))
	for _, mp := range overlays {
		overlaySet[mp] = true
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
