// Package orchestrator hosts the central daft control-plane: it
// authenticates clients and sandbox-hosts via mTLS (with TOFU peer
// approvals from internal/identity) and serves Connect-RPC endpoints
// over HTTP/2. Currently exposes only Health.Ping; the registry
// handler, port forwarder, and terminal stream land here next.
package orchestrator

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/http2"

	"github.com/richiejp/darkaifactory/internal/genpb/health/v1/healthv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
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

// Server wraps the underlying http.Server + listener so callers can
// Start() / Shutdown() it without touching net/http directly.
type Server struct {
	cfg  Config
	hs   *http.Server
	ln   net.Listener
	once sync.Once
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
	if cfg.ReadHeaderTimeout == 0 {
		cfg.ReadHeaderTimeout = 10 * time.Second
	}
	if cfg.Addr == "" {
		cfg.Addr = "0.0.0.0:0"
	}

	tlsCfg, err := identity.ServerTLSConfig(cfg.Identity, cfg.Peers)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: tls config: %w", err)
	}
	// HTTP/2 needs h2 in NextProtos; HTTP/1.1 stays as a fallback so
	// curl debugging works without --http2-prior-knowledge.
	tlsCfg.NextProtos = []string{"h2", "http/1.1"}

	mux := http.NewServeMux()
	healthPath, healthHandler := healthv1connect.NewHealthServiceHandler(NewHealthHandler())
	mux.Handle(healthPath, healthHandler)

	hs := &http.Server{
		Handler:           mux,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}
	if err := http2.ConfigureServer(hs, &http2.Server{}); err != nil {
		return nil, fmt.Errorf("orchestrator: configure http2: %w", err)
	}
	return &Server{cfg: cfg, hs: hs}, nil
}

// Listen binds the configured Addr but does not yet accept
// connections. Splitting Listen from Serve lets tests grab the port
// before any goroutine is in flight.
func (s *Server) Listen() error {
	ln, err := tls.Listen("tcp", s.cfg.Addr, s.hs.TLSConfig)
	if err != nil {
		return fmt.Errorf("orchestrator: listen %s: %w", s.cfg.Addr, err)
	}
	s.ln = ln
	return nil
}

// Addr returns the actual bound address. Only meaningful after Listen
// has succeeded.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Serve blocks until the server stops. Returns http.ErrServerClosed
// on graceful shutdown.
func (s *Server) Serve() error {
	if s.ln == nil {
		return errors.New("orchestrator: Serve called before Listen")
	}
	return s.hs.Serve(s.ln)
}

// Shutdown gracefully stops the server. Idempotent: subsequent calls
// are no-ops.
func (s *Server) Shutdown(ctx context.Context) error {
	var err error
	s.once.Do(func() {
		err = s.hs.Shutdown(ctx)
	})
	return err
}
