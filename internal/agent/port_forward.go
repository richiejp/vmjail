package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"

	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/trace"
)

// portForwardReportPrefix is the console prefix the agent uses for
// every port-forward status line. Same shape as the share / trace
// prefixes so a host-side test can scan for it without coupling
// to free-form text.
const portForwardReportPrefix = "port-forward"

// portForwardDeps factors the two side-effect surfaces the
// forwarder needs so tests can swap them for in-process pipes.
// Production wires the real AF_VSOCK listener + net.Dial.
type portForwardDeps struct {
	// listenVsock listens on the given AF_VSOCK port from inside
	// the VM. Returned as a net.Listener so the pump treats vsock
	// and TCP uniformly.
	listenVsock func(port uint32) (net.Listener, error)
	// dialContainer dials the in-VM container at 127.0.0.1:port
	// (or whatever loopback address the test injects).
	dialContainer func(ctx context.Context, port uint32) (net.Conn, error)
}

// defaultPortForwardDeps wires the production listener + dialer.
// vsockListener is the AF_VSOCK guest-side listener; dial is a
// plain TCP dial to the loopback address.
func defaultPortForwardDeps() portForwardDeps {
	return portForwardDeps{
		listenVsock:   vsockListenGuest,
		dialContainer: dialLoopbackTCP,
	}
}

// dialLoopbackTCP dials 127.0.0.1:port. Split out so tests can
// swap in net.Pipe / in-memory sockets without standing up a
// real TCP server.
func dialLoopbackTCP(ctx context.Context, port uint32) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", "127.0.0.1:"+strconv.FormatUint(uint64(port), 10))
}

// startPortForwardersFromCmdline parses the cmdline, reports
// errors to the agent console, and kicks off runPortForwarders on
// a detached goroutine. The agent's main loop blocks on ctx; this
// helper must not block.
func startPortForwardersFromCmdline(ctx context.Context, w io.Writer, d runDeps, cmdline string) {
	specs, err := ParsePortForwardCmdlines(cmdline)
	if err != nil {
		writeReport(w, portForwardReportPrefix, "cmdline: "+err.Error())
		return
	}
	if len(specs) == 0 {
		return
	}
	go runPortForwarders(ctx, w, specs, d.portForward)
}

// runPortForwarders launches one pump per PortForwardSpec, blocks
// until ctx is cancelled, then drains the pumps. Per-pump errors
// are logged to w via oops.Log and do not abort the others — a
// single port that refuses to bind shouldn't take down the rest.
//
// Pumps live for the agent's lifetime; their accept loops exit when
// ctx fires (the listener is closed, causing Accept to return).
func runPortForwarders(ctx context.Context, w io.Writer, specs []PortForwardSpec, d portForwardDeps) {
	if len(specs) == 0 {
		return
	}
	_, span := trace.Start(ctx, "agent.port_forwarders.start",
		trace.WithAttributes(trace.KV{Key: "count", Value: strconv.Itoa(len(specs))}))
	defer span.End()
	var wg sync.WaitGroup
	for _, spec := range specs {
		ln, err := d.listenVsock(spec.VsockPort)
		if err != nil {
			writeReport(w, portForwardReportPrefix,
				fmt.Sprintf("listen vsock %d → 127.0.0.1:%d: %v",
					spec.VsockPort, spec.ContainerPort, err))
			continue
		}
		writeReport(w, portForwardReportPrefix,
			fmt.Sprintf("ready vsock %d → 127.0.0.1:%d", spec.VsockPort, spec.ContainerPort))
		// Close the listener on ctx.Done so Accept returns and
		// the goroutine exits cleanly.
		go func(ln net.Listener) {
			<-ctx.Done()
			oops.Skip("agent: close port-forward listener", ln.Close(), net.ErrClosed)
		}(ln)
		wg.Add(1)
		go func(spec PortForwardSpec, ln net.Listener) {
			defer wg.Done()
			acceptLoop(ctx, w, ln, spec, d.dialContainer)
		}(spec, ln)
	}
	<-ctx.Done()
	wg.Wait()
}

// acceptLoop accepts connections on ln and pipes each one to the
// container at 127.0.0.1:spec.ContainerPort. Each accepted
// connection runs in its own goroutine so a slow client can't
// starve the others.
//
// Returns when ln returns ErrClosed (the ctx-driven Close
// above triggered it) or any non-temporary error.
func acceptLoop(
	ctx context.Context,
	w io.Writer,
	ln net.Listener,
	spec PortForwardSpec,
	dial func(context.Context, uint32) (net.Conn, error),
) {
	//daft:lint:disable=forloopbound reason="bounded by ln.Accept returning ErrClosed when ctx.Done closes the listener"
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return
			}
			writeReport(w, portForwardReportPrefix,
				fmt.Sprintf("accept vsock %d: %v", spec.VsockPort, err))
			return
		}
		go pumpConnection(ctx, w, conn, spec, dial)
	}
}

// pumpConnection dials the container, then bidirectionally copies
// bytes between the vsock-side conn and the container-side conn.
// Either direction's EOF closes both halves. Errors are logged
// (best-effort) but don't propagate — the forwarder is fire-and-
// forget per connection.
func pumpConnection(
	ctx context.Context,
	w io.Writer,
	upstream net.Conn,
	spec PortForwardSpec,
	dial func(context.Context, uint32) (net.Conn, error),
) {
	defer func() { oops.Skip("agent: close upstream", upstream.Close(), net.ErrClosed) }()
	downstream, err := dial(ctx, spec.ContainerPort)
	if err != nil {
		writeReport(w, portForwardReportPrefix,
			fmt.Sprintf("dial 127.0.0.1:%d: %v", spec.ContainerPort, err))
		return
	}
	defer func() { oops.Skip("agent: close downstream", downstream.Close(), net.ErrClosed) }()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(downstream, upstream)
		// Half-close the container side so a server reading until
		// EOF actually sees it.
		if cw, ok := downstream.(interface{ CloseWrite() error }); ok {
			oops.Skip("agent: close-write", cw.CloseWrite(), net.ErrClosed)
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(upstream, downstream)
		if cw, ok := upstream.(interface{ CloseWrite() error }); ok {
			oops.Skip("agent: close-write", cw.CloseWrite(), net.ErrClosed)
		}
	}()
	wg.Wait()
}
