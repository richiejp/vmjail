// Package orchestrator hosts the central daft control-plane: it
// authenticates clients and sandbox-hosts via mTLS (with TOFU peer
// approvals from internal/identity) and serves Connect-RPC endpoints
// over HTTP/2 — currently Health.Ping, TraceService.ReportSpans,
// the workspace BrokerService (matcher only; relay lands later),
// and a /debug/traces dump endpoint.
//
// The actual mTLS HTTP/2 listener lives in internal/mtls — this
// package is just the orchestrator-specific service surface.
package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/component"
	"github.com/richiejp/darkaifactory/internal/genpb/health/v1/healthv1connect"
	"github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1/sandboxctrlv1connect"
	"github.com/richiejp/darkaifactory/internal/genpb/trace/v1/tracev1connect"
	"github.com/richiejp/darkaifactory/internal/genpb/workspacebroker/v1/workspacebrokerv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/mtls"
	"github.com/richiejp/darkaifactory/internal/sandboxctrl"
	"github.com/richiejp/darkaifactory/internal/trace"
	"github.com/richiejp/darkaifactory/internal/workspacebroker"
)

// Config holds everything the server needs to start up. Identity is
// the orchestrator's own keypair + cert; Peers is the TOFU set used
// by VerifyPeerCertificate to authorise inbound connections.
type Config struct {
	Identity *identity.Identity
	Peers    *identity.PeerStore

	// Addr is the TCP listen address ("host:port"). If empty, "0.0.0.0:0"
	// is used and Server.Addr() reports the actual bound address.
	Addr string

	// ReadHeaderTimeout caps how long the server waits for a client
	// to send request headers. Belt-and-braces against slow-loris.
	ReadHeaderTimeout time.Duration

	// Tracer collects both inbound spans (via TraceService) and the
	// orchestrator's own server-side spans (via the Connect server
	// interceptor). New constructs one if Tracer is nil.
	Tracer *trace.Tracer

	// Reaper sweeps the central tracer's buffer for trace_ids that
	// have stopped receiving spans without finishing (typical
	// failure mode: a producer crashes before its Flush lands) and
	// injects a synthetic marker so the gap is visible. New
	// constructs a default reaper if this is nil.
	Reaper *trace.IngressReaper

	// Matcher pairs `daft workspace serve` peers with sandbox-host
	// peers. New constructs a default matcher if this is nil.
	// Tests pass a pre-populated matcher to inspect state.
	Matcher *workspacebroker.Matcher

	// SandboxRegistry tracks live sandbox-host registrations
	// (`daft-sandboxd serve` callers) and their heartbeats. New
	// constructs a default registry if this is nil. Tests pass a
	// pre-built registry to observe state directly.
	SandboxRegistry *sandboxctrl.Registry
}

// Server is the orchestrator's mTLS-gated control-plane listener.
// It's a thin wrapper around mtls.Server that registers the
// orchestrator's Connect-RPC handlers (Health.Ping,
// TraceService.ReportSpans, BrokerService, SandboxControlService)
// plus a /debug/traces dump endpoint.
type Server struct {
	inner       *mtls.Server
	tracer      *trace.Tracer
	reaper      *trace.IngressReaper
	matcher     *workspacebroker.Matcher
	sandboxReg  *sandboxctrl.Registry
	sandboxCtrl *SandboxControlHandler

	reaperStart  sync.Once
	reaperStop   sync.Once
	reaperCancel context.CancelFunc
	reaperDone   chan struct{}
}

// Matcher returns the orchestrator's workspace-broker matcher.
// Exposed for tests that want to assert matcher state directly.
func (s *Server) Matcher() *workspacebroker.Matcher { return s.matcher }

// SandboxRegistry returns the orchestrator's sandbox-control
// registry. Exposed for tests that want to assert registration
// state directly, and (Slice A2) for ListSandboxes.
func (s *Server) SandboxRegistry() *sandboxctrl.Registry { return s.sandboxReg }

// SandboxControl returns the orchestrator's sandbox-control
// handler. Exposed so operator tooling (and tests) can call
// Dispatch on a live sandbox without a separate RPC client.
func (s *Server) SandboxControl() *SandboxControlHandler { return s.sandboxCtrl }

// New builds a Server with the provided config. Returns an error if
// Config.Identity or Config.Peers is missing or if the TLS template
// can't be built.
func New(cfg Config) (*Server, error) {
	if cfg.Identity == nil {
		return nil, errors.New("orchestrator: nil identity")
	}
	if cfg.Peers == nil {
		return nil, errors.New("orchestrator: nil peer store")
	}

	tracer := cfg.Tracer
	if tracer == nil {
		tracer = trace.New(component.Orchestrator)
	}

	mux := http.NewServeMux()
	interceptor := connect.WithInterceptors(tracer.ServerInterceptor())
	matcher := cfg.Matcher
	if matcher == nil {
		matcher = workspacebroker.New()
	}
	sandboxReg := cfg.SandboxRegistry
	if sandboxReg == nil {
		sandboxReg = sandboxctrl.NewRegistry(nil)
	}
	sandboxCtrl, scErr := NewSandboxControlHandler(sandboxReg)
	if scErr != nil {
		return nil, scErr
	}
	// The broker handler needs the orchestrator's bound address
	// to return as the relay endpoint, but the listener doesn't
	// bind until Listen(). Close over a late-bound *mtls.Server
	// that gets filled in once mtls.New returns below.
	var inner *mtls.Server
	getRelayEndpoint := func() string {
		if inner == nil {
			return ""
		}
		return inner.Addr()
	}
	if err := registerServices(mux, interceptor, tracer, matcher, sandboxCtrl, getRelayEndpoint); err != nil {
		return nil, err
	}
	handler := withPeerMiddleware(mux)

	var err error
	inner, err = mtls.New(mtls.Config{
		Identity:          cfg.Identity,
		Peers:             cfg.Peers,
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	})
	if err != nil {
		return nil, err
	}
	reaper := cfg.Reaper
	if reaper == nil {
		reaper = trace.NewIngressReaper(tracer, component.Orchestrator)
	}
	return &Server{
		inner:       inner,
		tracer:      tracer,
		reaper:      reaper,
		matcher:     matcher,
		sandboxReg:  sandboxReg,
		sandboxCtrl: sandboxCtrl,
	}, nil
}

// registerServices installs every Connect-RPC handler plus the
// /debug/traces endpoint on mux. Extracted from New so that Slice
// A's incremental services keep New itself under funlen's cap.
func registerServices(
	mux *http.ServeMux,
	interceptor connect.HandlerOption,
	tracer *trace.Tracer,
	matcher *workspacebroker.Matcher,
	sandboxCtrl *SandboxControlHandler,
	relayEndpointFn func() string,
) error {
	healthPath, healthHandler := healthv1connect.NewHealthServiceHandler(
		NewHealthHandler(), interceptor,
	)
	mux.Handle(healthPath, healthHandler)

	traceHandler, err := NewTraceHandler(tracer)
	if err != nil {
		return err
	}
	tracePath, traceConnectHandler := tracev1connect.NewTraceServiceHandler(
		traceHandler, interceptor,
	)
	mux.Handle(tracePath, traceConnectHandler)
	mux.Handle("/debug/traces", debugTracesHandler(tracer))

	brokerHandler, err := NewBrokerHandler(matcher, relayEndpointFn)
	if err != nil {
		return err
	}
	brokerPath, brokerConnectHandler := workspacebrokerv1connect.NewBrokerServiceHandler(
		brokerHandler, interceptor,
	)
	mux.Handle(brokerPath, brokerConnectHandler)

	sandboxCtrlPath, sandboxCtrlConnectHandler := sandboxctrlv1connect.NewSandboxControlServiceHandler(
		sandboxCtrl, interceptor,
	)
	mux.Handle(sandboxCtrlPath, sandboxCtrlConnectHandler)
	return nil
}

// Tracer returns the orchestrator's Tracer. Callers can use it to
// emit additional spans (e.g. from background workers) or to read
// the buffer directly for tests.
func (s *Server) Tracer() *trace.Tracer { return s.tracer }

func (s *Server) Listen() error { return s.inner.Listen() }
func (s *Server) Addr() string  { return s.inner.Addr() }

// Serve starts the ingress reaper and blocks on the inner HTTP
// server. Reaper lifetime is the Serve→Shutdown window.
func (s *Server) Serve() error {
	s.startReaper()
	return s.inner.Serve()
}

// Shutdown stops the reaper before draining the HTTP server so a
// final scan can't race a teardown that's about to lose its span
// sources.
func (s *Server) Shutdown(ctx context.Context) error {
	s.stopReaper()
	return s.inner.Shutdown(ctx)
}

func (s *Server) startReaper() {
	s.reaperStart.Do(func() {
		// Reaper lifetime is Serve→Shutdown, not request-scoped.
		ctx, cancel := context.WithCancel(context.Background()) //nolint:contextcheck // see above
		s.reaperCancel = cancel
		s.reaperDone = make(chan struct{})
		go func() {
			defer close(s.reaperDone)
			s.reaper.Run(ctx)
		}()
	})
}

func (s *Server) stopReaper() {
	s.reaperStop.Do(func() {
		if s.reaperCancel == nil {
			return
		}
		s.reaperCancel()
		<-s.reaperDone
	})
}
