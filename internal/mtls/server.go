// Package mtls is the small HTTP/2 + mTLS server skeleton shared by
// daft's listener-side roles: the orchestrator's control-plane and
// the daft client's workspace chunk server. It owns nothing beyond
// the listener wiring — services hand it an http.Handler (typically
// a mux populated with Connect-RPC handlers) and identity bits to
// authenticate peers; everything else is a service-level concern.
package mtls

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

	"github.com/richiejp/darkaifactory/internal/identity"
)

// Config bundles everything Server needs at construction.
type Config struct {
	// Identity is this peer's keypair + cert.
	Identity *identity.Identity
	// Peers is the TOFU set used by VerifyPeerCertificate to
	// authorise inbound connections.
	Peers *identity.PeerStore
	// Addr is the TCP listen address ("host:port"). If empty,
	// "0.0.0.0:0" is used (Server.Addr() reports the actual bind).
	Addr string
	// Handler is the http.Handler that receives every authorised
	// request. Typically an *http.ServeMux populated with
	// Connect-RPC handler paths.
	Handler http.Handler
	// ReadHeaderTimeout caps how long the server waits for a
	// client to send request headers. Belt-and-braces against
	// slow-loris.
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

// New constructs a Server. Returns an error if any required field is
// missing or if the TLS template can't be built.
func New(cfg Config) (*Server, error) {
	if cfg.Identity == nil {
		return nil, errors.New("mtls: nil identity")
	}
	if cfg.Peers == nil {
		return nil, errors.New("mtls: nil peer store")
	}
	if cfg.Handler == nil {
		return nil, errors.New("mtls: nil handler")
	}
	if cfg.ReadHeaderTimeout == 0 {
		cfg.ReadHeaderTimeout = 10 * time.Second
	}
	if cfg.Addr == "" {
		cfg.Addr = "0.0.0.0:0"
	}

	tlsCfg, err := identity.ServerTLSConfig(cfg.Identity, cfg.Peers)
	if err != nil {
		return nil, fmt.Errorf("mtls: tls config: %w", err)
	}
	// HTTP/2 needs h2 in NextProtos; HTTP/1.1 stays as a fallback so
	// curl debugging works without --http2-prior-knowledge.
	tlsCfg.NextProtos = []string{"h2", "http/1.1"}

	hs := &http.Server{
		Handler:           cfg.Handler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}
	if err := http2.ConfigureServer(hs, &http2.Server{}); err != nil {
		return nil, fmt.Errorf("mtls: configure http2: %w", err)
	}
	return &Server{cfg: cfg, hs: hs}, nil
}

// Listen binds the configured Addr but does not yet accept
// connections. Splitting Listen from Serve lets tests grab the port
// before any goroutine is in flight.
func (s *Server) Listen() error {
	ln, err := tls.Listen("tcp", s.cfg.Addr, s.hs.TLSConfig)
	if err != nil {
		return fmt.Errorf("mtls: listen %s: %w", s.cfg.Addr, err)
	}
	s.ln = ln
	return nil
}

// Addr returns the actual bound address. Only meaningful after
// Listen has succeeded.
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
		return errors.New("mtls: Serve called before Listen")
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
