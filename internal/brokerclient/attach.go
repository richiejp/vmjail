package brokerclient

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

// innerBaseURL is the host portion of an InnerClient request URL.
// It is opaque — DialTLSContext returns the pre-handshaked conn
// without consulting it — but Connect-RPC's URL parser still
// requires a syntactically valid https:// origin.
const innerBaseURL = "https://daft-relay"

// InnerClient bundles an http.Client and the base URL that
// callers point Connect-RPC clients at. Both are wired to a single
// brokered + inner-mTLS conn — close via the returned Close func
// (idempotent) when the attach is done; closing tears down both
// the inner TLS conn and the outer Relay stream.
type InnerClient struct {
	HTTPClient *http.Client
	BaseURL    string
	Close      func() error
}

// AttachOptions configures a sandbox-side attach. Workspace +
// SandboxID + ServeFingerprint identify the workspace owner /
// session; HandshakeTimeout caps the inner-mTLS handshake.
type AttachOptions struct {
	Workspace        string
	SandboxID        string
	ServeFingerprint identity.Fingerprint
	HandshakeTimeout time.Duration
}

// AttachAndDial drives the sandbox-host side of a brokered attach:
// register, wait for the orchestrator's PendingAttach, dial Relay,
// run inner-mTLS as TLS client, and return an InnerClient whose
// HTTPClient speaks to the user-host's WorkspaceService.
//
// The caller does not call RequestAttach — the matcher's ACL
// rejects it from the sandbox-host's identity. The user-host
// (`daft workspace attach`) is the only legal initiator. This
// call blocks until that trigger fires.
//
// On success, InnerClient.Close tears down both the inner TLS
// conn and the outer broker registration.
func (c *Client) AttachAndDial(ctx context.Context, opts AttachOptions) (*InnerClient, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if opts.HandshakeTimeout == 0 {
		opts.HandshakeTimeout = 10 * time.Second
	}
	reg, err := c.RegisterSandbox(ctx, opts.SandboxID)
	if err != nil {
		return nil, fmt.Errorf("RegisterSandbox: %w", err)
	}
	done := false
	defer func() {
		if !done {
			_ = reg.Close()
		}
	}()
	sessionID, err := waitForAttach(ctx, reg, opts.ServeFingerprint)
	if err != nil {
		return nil, err
	}
	conn, err := c.DialRelay(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("DialRelay: %w", err)
	}
	tlsConn, err := c.innerClientHandshake(ctx, conn, opts.ServeFingerprint, opts.HandshakeTimeout)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	done = true
	return innerClientFromTLSConn(tlsConn, func() { _ = reg.Close() }), nil
}

// DialServeOptions configures the consumer side of a node-hosted
// sync attach (the role-inverted twin of AttachAndDial). Workspace
// is the serve key this client registers under; NodeFingerprint is
// the target node whose ServeBrokeredAsSandbox is waiting, and
// doubles as the broker sandbox_id.
type DialServeOptions struct {
	Workspace        string
	NodeFingerprint  identity.Fingerprint
	HandshakeTimeout time.Duration
}

// DialBrokeredServe drives the consumer side of a sync attach where
// the *node* hosts the WorkspaceService (ServeBrokeredAsSandbox) and
// this client drives it. Unlike AttachAndDial, this side is both the
// broker serve owner *and* the initiator, so it may legally call
// RequestAttach (the matcher's ACL is caller==serveOwner): register
// a serve, RequestAttach naming the node's fingerprint, wait for the
// pairing on our own serve stream, dial Relay, run inner mTLS as TLS
// client, and return an InnerClient pointed at the node's
// WorkspaceService.
//
// On success, InnerClient.Close tears down both the inner TLS conn
// and the outer broker registration.
func (c *Client) DialBrokeredServe(ctx context.Context, opts DialServeOptions) (*InnerClient, error) {
	switch {
	case opts.Workspace == "":
		return nil, errors.New("brokerclient: DialBrokeredServe: empty workspace")
	case opts.NodeFingerprint == (identity.Fingerprint{}):
		return nil, errors.New("brokerclient: DialBrokeredServe: zero node fingerprint")
	}
	if opts.HandshakeTimeout == 0 {
		opts.HandshakeTimeout = 10 * time.Second
	}
	self := c.cfg.Identity.Fingerprint()
	reg, err := c.RegisterServe(ctx, opts.Workspace)
	if err != nil {
		return nil, err
	}
	done := false
	defer func() {
		if !done {
			_ = reg.Close()
		}
	}()
	sandboxID := opts.NodeFingerprint.String()
	if _, _, err := c.Attach(ctx, self, opts.Workspace, sandboxID); err != nil {
		return nil, err
	}
	sessionID, err := waitForAttach(ctx, reg, opts.NodeFingerprint)
	if err != nil {
		return nil, err
	}
	conn, err := c.DialRelay(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("DialRelay: %w", err)
	}
	tlsConn, err := c.innerClientHandshake(ctx, conn, opts.NodeFingerprint, opts.HandshakeTimeout)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	done = true
	return innerClientFromTLSConn(tlsConn, func() { _ = reg.Close() }), nil
}

func (o AttachOptions) validate() error {
	switch {
	case o.Workspace == "":
		return errors.New("brokerclient: AttachAndDial: empty workspace")
	case o.SandboxID == "":
		return errors.New("brokerclient: AttachAndDial: empty sandbox_id")
	case o.ServeFingerprint == (identity.Fingerprint{}):
		return errors.New("brokerclient: AttachAndDial: zero serve fingerprint")
	}
	return nil
}

// waitForAttach blocks until the orchestrator pushes a
// PendingAttach on reg.Events() or ctx is cancelled. The
// announced peer fingerprint must match expectedFP — the
// caller's defence against an orchestrator pairing us with the
// wrong workspace owner.
func waitForAttach(ctx context.Context, reg *Registration, expectedFP identity.Fingerprint) (string, error) {
	select {
	case raw, ok := <-reg.Events():
		if !ok {
			return "", fmt.Errorf("brokerclient: registration ended before attach: %w", reg.Err())
		}
		peer, err := identity.ParseFingerprint(raw.GetPeerFingerprint())
		if err != nil {
			return "", fmt.Errorf("brokerclient: parse peer fingerprint: %w", err)
		}
		if peer != expectedFP {
			return "", fmt.Errorf("brokerclient: orchestrator announced peer %s, expected %s",
				peer, expectedFP)
		}
		return raw.GetSessionId(), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (c *Client) innerClientHandshake(ctx context.Context, conn net.Conn, peer identity.Fingerprint, timeout time.Duration) (*tls.Conn, error) {
	innerStore := identity.NewMemoryPeerStore(peer)
	tlsCfg, err := identity.ClientTLSConfig(c.cfg.Identity, innerStore, "")
	if err != nil {
		return nil, fmt.Errorf("inner TLS config: %w", err)
	}
	tlsCfg.NextProtos = []string{"h2"}
	tlsConn := tls.Client(conn, tlsCfg)
	hsCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := tlsConn.HandshakeContext(hsCtx); err != nil {
		return nil, fmt.Errorf("inner handshake: %w", err)
	}
	return tlsConn, nil
}

func innerClientFromTLSConn(tlsConn *tls.Conn, cleanupReg func()) *InnerClient {
	tr := &http2.Transport{
		DialTLSContext: func(_ context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			return tlsConn, nil
		},
	}
	var (
		once sync.Once
		err  error
	)
	return &InnerClient{
		HTTPClient: &http.Client{Transport: tr},
		BaseURL:    innerBaseURL,
		Close: func() error {
			once.Do(func() {
				err = tlsConn.Close()
				cleanupReg()
			})
			return err
		},
	}
}
