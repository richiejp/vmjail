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
	"time"

	"github.com/richiejp/darkaifactory/internal/genpb/health/v1/healthv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/mtls"
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
}

// Server is the orchestrator's mTLS-gated control-plane listener.
// It's a thin wrapper around mtls.Server that registers the
// orchestrator's Connect-RPC handlers (currently just Health.Ping).
type Server struct {
	inner *mtls.Server
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

	mux := http.NewServeMux()
	healthPath, healthHandler := healthv1connect.NewHealthServiceHandler(NewHealthHandler())
	mux.Handle(healthPath, healthHandler)

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
	return &Server{inner: inner}, nil
}

// Listen / Addr / Serve / Shutdown are passthroughs to the inner
// mtls.Server. Kept on the wrapper so callers don't need to know
// the listener's a separate package.
func (s *Server) Listen() error                      { return s.inner.Listen() }
func (s *Server) Addr() string                       { return s.inner.Addr() }
func (s *Server) Serve() error                       { return s.inner.Serve() }
func (s *Server) Shutdown(ctx context.Context) error { return s.inner.Shutdown(ctx) }
