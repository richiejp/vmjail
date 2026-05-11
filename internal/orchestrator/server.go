// Package orchestrator hosts the central daft control-plane: it
// authenticates clients and sandbox-hosts via mTLS (with TOFU peer
// approvals from internal/identity) and serves Connect-RPC endpoints
// over HTTP/2. Currently exposes only Health.Ping; the registry
// handler, port forwarder, and terminal stream land here next.
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
	"github.com/richiejp/darkaifactory/internal/genpb/trace/v1/tracev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/mtls"
	"github.com/richiejp/darkaifactory/internal/trace"
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
}

// Server is the orchestrator's mTLS-gated control-plane listener.
// It's a thin wrapper around mtls.Server that registers the
// orchestrator's Connect-RPC handlers (currently Health.Ping and
// TraceService.ReportSpans) plus a /debug/traces dump endpoint.
type Server struct {
	inner  *mtls.Server
	tracer *trace.Tracer
	reaper *trace.IngressReaper

	reaperMu     sync.Mutex
	reaperCancel context.CancelFunc
	reaperDone   chan struct{}
}

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
	healthPath, healthHandler := healthv1connect.NewHealthServiceHandler(
		NewHealthHandler(), interceptor,
	)
	mux.Handle(healthPath, healthHandler)

	traceHandler, err := NewTraceHandler(tracer)
	if err != nil {
		return nil, err
	}
	tracePath, traceConnectHandler := tracev1connect.NewTraceServiceHandler(
		traceHandler, interceptor,
	)
	mux.Handle(tracePath, traceConnectHandler)

	mux.Handle("/debug/traces", debugTracesHandler(tracer))

	inner, err := mtls.New(mtls.Config{
		Identity:          cfg.Identity,
		Peers:             cfg.Peers,
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	})
	if err != nil {
		return nil, err
	}
	reaper := cfg.Reaper
	if reaper == nil {
		reaper = trace.NewIngressReaper(tracer, component.Orchestrator)
	}
	return &Server{inner: inner, tracer: tracer, reaper: reaper}, nil
}

// Tracer returns the orchestrator's Tracer. Callers can use it to
// emit additional spans (e.g. from background workers) or to read
// the buffer directly for tests.
func (s *Server) Tracer() *trace.Tracer { return s.tracer }

// Listen and Addr are passthroughs to the inner mtls.Server.
func (s *Server) Listen() error { return s.inner.Listen() }
func (s *Server) Addr() string  { return s.inner.Addr() }

// Serve starts the ingress reaper goroutine and then blocks on the
// inner HTTP server. The reaper runs alongside Serve and is stopped
// by Shutdown, so the typical pattern (Serve in one goroutine,
// Shutdown from the signal handler) keeps the reaper bounded to
// the server's lifetime without callers having to wire context.
func (s *Server) Serve() error {
	s.startReaper()
	return s.inner.Serve()
}

// Shutdown stops the reaper first, then asks the inner server to
// drain. Stopping the reaper before HTTP shutdown means we don't
// race a final scan against a server that's about to lose its
// span sources.
func (s *Server) Shutdown(ctx context.Context) error {
	s.stopReaper()
	return s.inner.Shutdown(ctx)
}

func (s *Server) startReaper() {
	s.reaperMu.Lock()
	defer s.reaperMu.Unlock()
	if s.reaperCancel != nil {
		return
	}
	// The reaper's lifetime is the Serve->Shutdown window, which is
	// intentionally independent of any request context — so a fresh
	// Background-rooted ctx is what we want, not the caller's.
	ctx, cancel := context.WithCancel(context.Background()) //nolint:contextcheck // reaper lifetime is Serve→Shutdown, not request-scoped

	s.reaperCancel = cancel
	s.reaperDone = make(chan struct{})
	go func() {
		defer close(s.reaperDone)
		s.reaper.Run(ctx)
	}()
}

func (s *Server) stopReaper() {
	s.reaperMu.Lock()
	cancel := s.reaperCancel
	done := s.reaperDone
	s.reaperCancel = nil
	s.reaperDone = nil
	s.reaperMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}
