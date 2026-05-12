package brokerclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/http2"

	"github.com/richiejp/darkaifactory/internal/identity"
)

// ServeOptions configures a broker-mode serve loop. Handler is the
// inner HTTP/2 handler — typically a mux with WorkspaceService
// registered. Per-attach inner-mTLS handshake deadline applies to
// the TLS handshake only; once it succeeds, request lifetimes are
// governed by the caller's context.
type ServeOptions struct {
	// Workspace name to register with the broker.
	Workspace string
	// Handler is the http.Handler that every inner request lands
	// on. Connect-RPC handlers ride on a standard http.ServeMux.
	Handler http.Handler
	// HandshakeTimeout caps the inner-mTLS handshake per attach.
	// Defaults to 10s.
	HandshakeTimeout time.Duration
}

// ServeBrokered registers a workspace serve with the orchestrator
// and accepts inbound attaches forever (until ctx is cancelled or
// the registration stream ends). Each PendingAttach event spawns a
// goroutine that:
//
//  1. Dials Relay with the announced session_id.
//  2. Runs an inner-mTLS handshake as TLS *server*, trusting only
//     the peer fingerprint the orchestrator announced.
//  3. Hands the handshaked conn to a fresh http2.Server with the
//     caller-supplied Handler.
//
// Inner-mTLS trust is per-attach-ephemeral: a NewMemoryPeerStore
// is built fresh for each attach with the announced fingerprint as
// the only approved peer. The user's persistent peers.json is not
// consulted (broker-mode trust is mediated by the orchestrator).
//
// Returns the underlying registration error from the broker stream
// — typically nil for a clean ctx-cancel teardown.
func (c *Client) ServeBrokered(ctx context.Context, opts ServeOptions) error {
	if opts.Workspace == "" {
		return errors.New("brokerclient: ServeBrokered: empty workspace")
	}
	if opts.Handler == nil {
		return errors.New("brokerclient: ServeBrokered: nil handler")
	}
	if opts.HandshakeTimeout == 0 {
		opts.HandshakeTimeout = 10 * time.Second
	}
	reg, err := c.RegisterServe(ctx, opts.Workspace)
	if err != nil {
		return err
	}
	defer func() { _ = reg.Close() }()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-reg.Events():
			if !ok {
				return reg.Err()
			}
			// We trust the orchestrator (it's in our peer
			// store) to emit well-formed fingerprints. A
			// parse failure would crash inner-TLS handshake
			// inside serveOneAttach; we don't need a
			// defensive guard here.
			peer, _ := identity.ParseFingerprint(ev.GetPeerFingerprint())
			wg.Add(1)
			go func(sid string, peer identity.Fingerprint) {
				defer wg.Done()
				_ = c.serveOneAttach(ctx, sid, peer, opts)
			}(ev.GetSessionId(), peer)
		}
	}
}

// serveOneAttach handles a single PendingAttach by dialling Relay,
// handshaking inner mTLS as server, and running http2.Server on the
// resulting conn. Returns any error so callers (or test harnesses)
// can react; live use ignores it.
func (c *Client) serveOneAttach(ctx context.Context, sid string, peer identity.Fingerprint, opts ServeOptions) error {
	conn, err := c.DialRelay(ctx, sid)
	if err != nil {
		return fmt.Errorf("brokerclient: DialRelay: %w", err)
	}
	defer func() { _ = conn.Close() }()

	innerStore := identity.NewMemoryPeerStore(peer)
	tlsCfg, err := identity.ServerTLSConfig(c.cfg.Identity, innerStore)
	if err != nil {
		return fmt.Errorf("brokerclient: inner TLS config: %w", err)
	}
	// HTTP/2 ALPN advertisement so http2.Server.ServeConn is happy.
	tlsCfg.NextProtos = []string{"h2"}

	tlsConn := tls.Server(conn, tlsCfg)
	hsCtx, cancel := context.WithTimeout(ctx, opts.HandshakeTimeout)
	defer cancel()
	if err := tlsConn.HandshakeContext(hsCtx); err != nil {
		return fmt.Errorf("brokerclient: inner handshake: %w", err)
	}

	// http2.Server.ServeConn blocks until the conn closes. Wire
	// ctx-cancel to a Close so the parent's ctx cancellation
	// terminates idle attaches promptly.
	stopWatch := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = tlsConn.Close()
		case <-stopWatch:
		}
	}()
	defer close(stopWatch)

	h2s := &http2.Server{}
	h2s.ServeConn(tlsConn, &http2.ServeConnOpts{
		Context: ctx,
		Handler: opts.Handler,
		BaseConfig: &http.Server{
			ReadHeaderTimeout: opts.HandshakeTimeout,
		},
	})
	return nil
}
