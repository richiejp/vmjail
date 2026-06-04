package sandbox

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"os"
	"sync"
	"time"

	"github.com/localai-org/darkaifactory/internal/agent"
	agentv1 "github.com/localai-org/darkaifactory/internal/genpb/agent/v1"
	"github.com/localai-org/darkaifactory/internal/oops"
	"github.com/localai-org/darkaifactory/internal/trace"
)

// startVsockTraceReceiver opens a UDS listener at
// <HostSocket>_<TracePort> (the Cloud Hypervisor hybrid-vsock
// convention: every guest AF_VSOCK port maps to one host UDS) and
// hands incoming framed batches to the Tracer reachable from ctx
// (via trace.TracerFromContext). Returns a stop func that closes
// the listener and joins the accept goroutine.
//
// No-op (returns a do-nothing stop) when cfg has no Vsock or no
// TracePort.
//
// If TracePort is set but no Tracer is in ctx, the listener still
// runs (so the in-VM agent's Flush doesn't error on dial), but the
// spans are silently dropped. The expected caller wires a Tracer
// via trace.ContextWithTracer before invoking Hello.
func startVsockTraceReceiver(ctx context.Context, cfg Config) (stop func(), err error) {
	if cfg.Vsock == nil || cfg.Vsock.TracePort == 0 {
		return func() {}, nil
	}
	sockPath := fmt.Sprintf("%s_%d", cfg.Vsock.HostSocket, cfg.Vsock.TracePort)
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
// dial and falls back to the cmdline + the banner-scan sentinel.
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
// (with its overlay flag), the per-workspace port-forward table, and the
// Tier-2 observe-gadget paths. Mirrors the per-workspace half of
// buildKernelCmdline; trace/attach/net ports still ride the cmdline until
// slice 3b collapses them to fixed constants.
func buildGuestConfig(cfg Config) *agentv1.GuestConfig {
	gc := &agentv1.GuestConfig{Bundle: cfg.Bundle}
	for _, s := range cfg.Shares {
		gc.Shares = append(gc.Shares, &agentv1.GuestConfig_Share{
			Tag:        s.Tag,
			Mountpoint: s.Mountpoint,
			Overlay:    s.Overlay,
		})
	}
	for _, pf := range cfg.PortForwards {
		gc.PortForwards = append(gc.PortForwards, &agentv1.GuestConfig_PortForward{
			VsockPort:     pf.VsockPort,
			ContainerPort: pf.ContainerPort,
		})
	}
	gc.Observe = buildObserveConfig(cfg)
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
	//daft:lint:disable=forloopbound reason="exits when listener.Close drives Accept to return net.ErrClosed"
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
	//daft:lint:disable=forloopbound reason="exits on conn close (agent EOF or server stop) driving ReadAgentEvent to error"
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
