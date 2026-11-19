package sandbox

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/richiejp/vmjail/internal/agent"
	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
	"github.com/richiejp/vmjail/internal/oops"
	"github.com/richiejp/vmjail/internal/trace"
)

// standaloneGuestCID is the AF_VSOCK context ID handed to a standalone
// single-VM boot. 3 is the lowest non-reserved CID (0-2 are reserved by
// the vsock spec), and a standalone invocation boots exactly one VM, so
// no per-VM counter is needed — unlike the orchestrated executor, which
// mints a fresh CID per Sandbox.
const standaloneGuestCID uint32 = 3

// NewStandaloneVsock mints a hybrid-vsock device for the standalone CLI
// boot paths (vmjail sandbox, vmjail-sandboxd) so they deliver per-VM config
// and stream status events over the control channel like the orchestrated
// executor, instead of the world-readable kernel cmdline. The socket
// lives in a fresh temp dir that cleanup removes once the VM is gone;
// callers defer it. Always-on means every boot path gets a device — see
// the orchestrated counterpart, Executor.assignVsock.
func NewStandaloneVsock() (cfg *VsockConfig, cleanup func(), err error) {
	dir, err := os.MkdirTemp("", "vmjail-vsock-")
	if err != nil {
		return nil, func() {}, fmt.Errorf("sandbox: vsock socket dir: %w", err)
	}
	return &VsockConfig{
		HostSocket: filepath.Join(dir, "control.sock"),
		GuestCID:   standaloneGuestCID,
	}, func() { _ = os.RemoveAll(dir) }, nil
}

// startVsockTraceReceiver opens a UDS listener at
// <HostSocket>_<agent.DefaultTraceVsockPort> (the Cloud Hypervisor
// hybrid-vsock convention: every guest AF_VSOCK port maps to one host
// UDS) and hands incoming framed batches to the Tracer reachable from ctx
// (via trace.TracerFromContext). Returns a stop func that closes
// the listener and joins the accept goroutine.
//
// No-op (returns a do-nothing stop) when cfg has no Vsock or the trace
// feature is off (cfg.Vsock.Trace == false).
//
// If trace is on but no Tracer is in ctx, the listener still
// runs (so the in-VM agent's Flush doesn't error on dial), but the
// spans are silently dropped. The expected caller wires a Tracer
// via trace.ContextWithTracer before invoking Hello.
func startVsockTraceReceiver(ctx context.Context, cfg Config) (stop func(), err error) {
	if cfg.Vsock == nil || !cfg.Vsock.Trace {
		return func() {}, nil
	}
	sockPath := fmt.Sprintf("%s_%d", cfg.Vsock.HostSocket, agent.DefaultTraceVsockPort)
	// Pre-clean a stale socket from a crashed previous run; Listen
	// would otherwise EADDRINUSE.
	oops.Skip("remove stale vsock trace socket", os.Remove(sockPath), fs.ErrNotExist)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return func() {}, fmt.Errorf("sandbox: listen %s: %w", sockPath, err)
	}

	tracer := trace.TracerFromContext(ctx)
	if tracer == nil {
		tracer = trace.NewWithCapacity("dev-null", 1)
	}
	recv := trace.NewStreamReceiver(listener, tracer)

	// The listener's lifetime is bounded by Hello's defer stop(),
	// not the caller's request ctx — detach cancellation from the
	// parent (a request-cancelled or SIGTERM ctx would close the
	// listener before the agent has flushed its spans) while
	// keeping any values the parent carried.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = recv.Run(runCtx)
	}()

	stop = func() {
		cancel()
		<-done
		oops.Skip("remove vsock trace socket on shutdown",
			os.Remove(sockPath), fs.ErrNotExist)
	}
	return stop, nil
}

// configServeTimeout bounds one config exchange (read handshake +
// write response) on an accepted connection so a client that connects
// but never sends its handshake can't pin a server goroutine.
const configServeTimeout = 5 * time.Second

// startVsockConfigServer opens a UDS listener at
// <HostSocket>_<DefaultConfigVsockPort> (the same Cloud Hypervisor
// hybrid-vsock convention the trace receiver uses) and serves the VM's
// control channel: it replies to each accepted connection with this
// VM's GuestConfig (built from cfg), then reads the status events the
// agent streams back. The returned bootComplete channel is closed the
// first time an agent reports AgentEvent_BOOT_COMPLETE — the one-shot
// Hello harness waits on it instead of scanning the console banner.
//
// No-op (do-nothing stop, nil bootComplete) when cfg has no Vsock /
// HostSocket: such a VM has no vsock device at all, so the agent can't
// dial and degrades to a banner-only boot (the banner-scan sentinel).
func startVsockConfigServer(ctx context.Context, cfg Config) (stop func(), bootComplete <-chan struct{}, err error) {
	if cfg.Vsock == nil || cfg.Vsock.HostSocket == "" {
		return func() {}, nil, nil
	}
	sockPath := fmt.Sprintf("%s_%d", cfg.Vsock.HostSocket, agent.DefaultConfigVsockPort)
	// Pre-clean a stale socket from a crashed previous run; Listen
	// would otherwise EADDRINUSE.
	oops.Skip("remove stale vsock config socket", os.Remove(sockPath), fs.ErrNotExist)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return func() {}, nil, fmt.Errorf("sandbox: listen %s: %w", sockPath, err)
	}

	guestCfg := buildGuestConfig(cfg)
	bootCh := make(chan struct{})
	var bootOnce sync.Once
	signalBoot := func() { bootOnce.Do(func() { close(bootCh) }) }

	// Detach from the request ctx (same rationale as the trace
	// receiver): the listener's lifetime is bounded by the caller's
	// defer stop(), not a request-cancelled or SIGTERM ctx.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveControl(runCtx, listener, guestCfg, signalBoot)
	}()

	stop = func() {
		cancel()
		_ = listener.Close()
		<-done
		oops.Skip("remove vsock config socket on shutdown",
			os.Remove(sockPath), fs.ErrNotExist)
	}
	return stop, bootCh, nil
}

// buildGuestConfig projects the boot Config into the GuestConfig wire
// message the agent applies: the container bundle, each virtio-fs share
// (with its overlay flag), the per-workspace port-forward table, the
// Tier-2 observe-gadget paths, and the trace/attach/net feature toggles.
// This is the sole channel for per-VM settings — the kernel cmdline
// carries no vmjail.* tokens at all.
func buildGuestConfig(cfg Config) *agentv1.GuestConfig {
	gc := &agentv1.GuestConfig{
		Bundle:              cfg.Bundle,
		CredentialProvider:  cfg.CredentialProvider,
		ContainerPersistent: cfg.ContainerPersistent,
	}
	for _, s := range cfg.Shares {
		gc.Shares = append(gc.Shares, &agentv1.GuestConfig_Share{
			Tag:        s.Tag,
			Mountpoint: s.Mountpoint,
			Overlay:    s.Overlay,
			ReadOnly:   s.ReadOnly,
		})
	}
	for _, pf := range cfg.PortForwards {
		gc.PortForwards = append(gc.PortForwards, &agentv1.GuestConfig_PortForward{
			VsockPort:     pf.VsockPort,
			ContainerPort: pf.ContainerPort,
		})
	}
	for _, argv := range cfg.PostStartCommands {
		if len(argv) == 0 {
			continue
		}
		gc.PostStartCommands = append(gc.PostStartCommands,
			&agentv1.GuestConfig_PostStartCommand{Argv: argv})
	}
	gc.Observe = buildObserveConfig(cfg)
	// Protocol-feature toggles: the agent brings each up on its fixed
	// constant port, so we send a bit, not the port number. Derived from
	// the per-VM VsockConfig the executor set (a non-zero port == feature
	// on); the port fields themselves stay host-side for the listeners.
	if v := cfg.Vsock; v != nil {
		gc.AttachEnabled = v.Attach
		gc.NetEnabled = v.Net
		gc.TraceEnabled = v.Trace
	}
	return gc
}

// buildObserveConfig projects the boot Config's Tier-2 gadget output
// paths into the wire message, or nil when none are set (so a VM with no
// observe share carries no Observe block at all).
func buildObserveConfig(cfg Config) *agentv1.GuestConfig_Observe {
	if cfg.OpensnoopLogPath == "" && cfg.ExecsnoopLogPath == "" &&
		cfg.SleepsnoopLogPath == "" && cfg.IosnoopLogPath == "" &&
		cfg.SyscallListPath == "" && cfg.ObserveSummaryPath == "" {
		return nil
	}
	return &agentv1.GuestConfig_Observe{
		Opensnoop:  cfg.OpensnoopLogPath,
		Execsnoop:  cfg.ExecsnoopLogPath,
		Sleepsnoop: cfg.SleepsnoopLogPath,
		Iosnoop:    cfg.IosnoopLogPath,
		Syscalls:   cfg.SyscallListPath,
		Summary:    cfg.ObserveSummaryPath,
	}
}

// serveControl accepts connections until the listener is closed, driving
// the control protocol (config reply + event read) on each. signalBoot
// is invoked when any connection reports BOOT_COMPLETE.
func serveControl(ctx context.Context, listener net.Listener, cfg *agentv1.GuestConfig, signalBoot func()) {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	//vmjail:lint:disable=forloopbound reason="exits when listener.Close drives Accept to return net.ErrClosed"
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go handleControlConn(ctx, conn, cfg, signalBoot)
	}
}

// handleControlConn reads the agent's handshake, writes the GuestConfig
// reply, then reads the status events the agent streams until it closes
// the connection (EOF) or the server stops. signalBoot fires on
// BOOT_COMPLETE. The handshake is bounded by configServeTimeout; the
// deadline is then cleared because events arrive throughout the boot.
func handleControlConn(ctx context.Context, conn net.Conn, cfg *agentv1.GuestConfig, signalBoot func()) {
	defer func() { _ = conn.Close() }()
	// Close the conn when the server stops so a parked event Read
	// unblocks even though the agent never closed its end.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-watchDone:
		}
	}()

	_ = conn.SetDeadline(time.Now().Add(configServeTimeout))
	if _, err := agent.ReadGuestConfigRequest(conn); err != nil {
		return
	}
	if err := agent.WriteGuestConfig(conn, cfg); err != nil {
		return
	}

	_ = conn.SetDeadline(time.Time{})
	//vmjail:lint:disable=forloopbound reason="exits on conn close (agent EOF or server stop) driving ReadAgentEvent to error"
	for {
		ev, err := agent.ReadAgentEvent(conn)
		if err != nil {
			return
		}
		if ev.GetKind() == agentv1.AgentEvent_BOOT_COMPLETE {
			signalBoot()
		}
	}
}
