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
// run inner-mTLS as TLS *client*, and return an InnerClient whose
// HTTPClient speaks to the user-host's WorkspaceService.
//
// The caller is responsible for invoking RequestAttach via some
// other identity (usually the user-host running `daft workspace
// attach`) — the v1 ACL forbids the sandbox-host from initiating
// the attach itself.
//
// Returns when the inner connection is ready, or an error if any
// stage fails. The InnerClient.Close func tears everything down.
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
	// One defer covers every early-return below; the success path
	// flips done=true and the InnerClient.Close func takes over
	// the registration teardown.
	done := false
	defer func() {
		if !done {
			_ = reg.Close()
		}
	}()
	peer, sessionID, err := waitForAttach(ctx, reg, opts.ServeFingerprint)
	if err != nil {
		return nil, err
	}
	conn, err := c.DialRelay(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("DialRelay: %w", err)
	}
	tlsConn, err := c.innerClientHandshake(ctx, conn, peer, opts.HandshakeTimeout)
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
func waitForAttach(ctx context.Context, reg *Registration, expectedFP identity.Fingerprint) (identity.Fingerprint, string, error) {
	select {
	case raw, ok := <-reg.Events():
		if !ok {
			return identity.Fingerprint{}, "", fmt.Errorf("brokerclient: registration ended before attach: %w", reg.Err())
		}
		peer, err := identity.ParseFingerprint(raw.GetPeerFingerprint())
		if err != nil {
			return identity.Fingerprint{}, "", fmt.Errorf("brokerclient: parse peer fingerprint: %w", err)
		}
		if peer != expectedFP {
			return identity.Fingerprint{}, "", fmt.Errorf("brokerclient: orchestrator announced peer %s, expected %s",
				peer, expectedFP)
		}
		return peer, raw.GetSessionId(), nil
	case <-ctx.Done():
		return identity.Fingerprint{}, "", ctx.Err()
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
		// DialTLSContext yields the already-handshaked conn so
		// http2.Transport doesn't try to redial. TLSClientConfig
		// is unused when DialTLSContext returns directly.
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
		// The host portion is opaque — DialTLSContext ignores
		// it — but Connect-RPC needs a parseable URL.
		BaseURL: "https://daft-relay",
		Close: func() error {
			once.Do(func() {
				err = tlsConn.Close()
				cleanupReg()
			})
			return err
		},
	}
}
