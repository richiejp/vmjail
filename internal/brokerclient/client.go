// Package brokerclient is the user/sandbox-host side of the
// orchestrator workspace broker. It wraps the generated
// BrokerServiceClient with helpers that build the outer mTLS
// transport to the orchestrator, register persistent control
// streams (RegisterServe / RegisterSandbox), dial Relay, and
// adapt the resulting bidi byte channel as a net.Conn the
// caller can wrap in inner mTLS + HTTP/2.
//
// The package owns *no* state about workspaces or sandboxes —
// that's the caller's job. It only owns the broker connection
// and the proto-level handshake.
//
// See PLAN.md "Slice 3.5 design" for the architectural rationale.
package brokerclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	workspacebrokerv1 "github.com/richiejp/darkaifactory/internal/genpb/workspacebroker/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspacebroker/v1/workspacebrokerv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/workspacebroker"
)

// Config is everything the broker client needs to dial the
// orchestrator. URL must be a full https://host:port. Identity is
// our own keypair; Peers must contain the orchestrator's
// fingerprint (TOFU-approved out of band, same as ping).
type Config struct {
	URL      string
	Identity *identity.Identity
	Peers    *identity.PeerStore
}

// Client is a thin handle around a BrokerServiceClient. Safe for
// concurrent use; each high-level operation (Register*, Attach,
// DialRelay) is independent of the others.
type Client struct {
	cfg    Config
	httpc  *http.Client
	broker workspacebrokerv1connect.BrokerServiceClient
}

// New builds a Client. Returns an error if cfg.Identity/Peers/URL
// are missing or if the TLS template can't be built.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("brokerclient: empty URL")
	}
	if cfg.Identity == nil {
		return nil, errors.New("brokerclient: nil identity")
	}
	if cfg.Peers == nil {
		return nil, errors.New("brokerclient: nil peer store")
	}
	tlsCfg, err := identity.ClientTLSConfig(cfg.Identity, cfg.Peers, "")
	if err != nil {
		return nil, fmt.Errorf("brokerclient: build TLS config: %w", err)
	}
	// No http.Client.Timeout: long-lived server-streaming RPCs
	// (RegisterServe / RegisterSandbox) live for the whole serve
	// lifetime. Per-call cancellation is via context.Context.
	httpc := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}}
	return &Client{
		cfg:    cfg,
		httpc:  httpc,
		broker: workspacebrokerv1connect.NewBrokerServiceClient(httpc, cfg.URL),
	}, nil
}

// Broker exposes the generated client for callers that need the
// raw RPC surface (tests, future bespoke flows).
func (c *Client) Broker() workspacebrokerv1connect.BrokerServiceClient { return c.broker }

// Identity returns the client's own identity.
func (c *Client) Identity() *identity.Identity { return c.cfg.Identity }

// Registration is a live RegisterServe / RegisterSandbox stream.
// Events streams PendingAttach frames as the orchestrator brokers
// attaches. Close releases the stream; Err surfaces a stream-level
// transport error after Events closes. Construction blocks until
// the orchestrator's initial SubscriptionReady event arrives,
// guaranteeing the subscription is in place server-side before
// any dependent operation (e.g. a peer's RequestAttach) is issued.
type Registration struct {
	stream interface {
		Receive() bool
		Msg() *workspacebrokerv1.RegistrationEvent
		Err() error
		Close() error
	}
	events chan *workspacebrokerv1.PendingAttach
	done   chan struct{}
	err    error
}

// Events returns a channel of PendingAttach events. The channel
// closes when the underlying stream ends; Err() then reports any
// transport-level error.
func (r *Registration) Events() <-chan *workspacebrokerv1.PendingAttach { return r.events }

// Err returns any error observed on the stream after Events
// closed. Nil for a clean ctx-cancel teardown.
func (r *Registration) Err() error {
	<-r.done
	return r.err
}

// Close tears down the underlying stream. Idempotent.
func (r *Registration) Close() error { return r.stream.Close() }

// RegisterServe opens a RegisterServe stream for workspace, blocks
// until SubscriptionReady, and returns a live Registration whose
// Events channel surfaces PendingAttach frames.
func (c *Client) RegisterServe(ctx context.Context, workspace string) (*Registration, error) {
	stream, err := c.broker.RegisterServe(ctx, connect.NewRequest(
		&workspacebrokerv1.RegisterServeRequest{Workspace: workspace},
	))
	if err != nil {
		return nil, fmt.Errorf("brokerclient: RegisterServe: %w", err)
	}
	return newRegistration(stream)
}

// RegisterSandbox opens a RegisterSandbox stream for sandboxID,
// blocks until SubscriptionReady, and returns a live Registration.
func (c *Client) RegisterSandbox(ctx context.Context, sandboxID string) (*Registration, error) {
	stream, err := c.broker.RegisterSandbox(ctx, connect.NewRequest(
		&workspacebrokerv1.RegisterSandboxRequest{SandboxId: sandboxID},
	))
	if err != nil {
		return nil, fmt.Errorf("brokerclient: RegisterSandbox: %w", err)
	}
	return newRegistration(stream)
}

// newRegistration synchronously consumes the initial Ready event
// then spawns a goroutine that forwards subsequent PendingAttach
// events on the returned Registration's channel.
func newRegistration(stream *connect.ServerStreamForClient[workspacebrokerv1.RegistrationEvent]) (*Registration, error) {
	if !stream.Receive() {
		_ = stream.Close()
		if err := stream.Err(); err != nil {
			return nil, fmt.Errorf("brokerclient: stream closed before Ready: %w", err)
		}
		return nil, errors.New("brokerclient: stream closed before Ready")
	}
	if stream.Msg().GetReady() == nil {
		_ = stream.Close()
		return nil, fmt.Errorf("brokerclient: first event was not Ready: %v", stream.Msg())
	}
	r := &Registration{
		stream: stream,
		events: make(chan *workspacebrokerv1.PendingAttach, 4),
		done:   make(chan struct{}),
	}
	go func() {
		defer close(r.done)
		defer close(r.events)
		for stream.Receive() {
			ev := stream.Msg().GetAttach()
			if ev == nil {
				continue
			}
			r.events <- ev
		}
		r.err = stream.Err()
	}()
	return r, nil
}

// Attach calls BrokerService.RequestAttach. The caller is the
// user-host's `daft workspace attach`; serveFP is the workspace
// owner's fingerprint (our own identity in v1, since the v1 ACL
// is caller==serveOwner). Returns session_id and the
// orchestrator's relay endpoint.
func (c *Client) Attach(ctx context.Context, serveFP identity.Fingerprint, workspace, sandboxID string) (sessionID, relayEndpoint string, err error) {
	resp, err := c.broker.RequestAttach(ctx, connect.NewRequest(
		&workspacebrokerv1.RequestAttachRequest{
			ServeFingerprint: serveFP.String(),
			Workspace:        workspace,
			SandboxId:        sandboxID,
		},
	))
	if err != nil {
		return "", "", fmt.Errorf("brokerclient: RequestAttach: %w", err)
	}
	return resp.Msg.GetSessionId(), resp.Msg.GetRelayEndpoint(), nil
}

// DialRelay opens a Relay stream, sends the initial Hello with
// sessionID, and returns a net.Conn over the byte channel. Caller
// should Close the conn to tear down the stream. The returned
// conn carries opaque bytes — inner mTLS and HTTP/2 are the
// caller's responsibility.
func (c *Client) DialRelay(ctx context.Context, sessionID string) (net.Conn, error) {
	if sessionID == "" {
		return nil, errors.New("brokerclient: empty session_id")
	}
	stream := c.broker.Relay(ctx)
	if err := stream.Send(&workspacebrokerv1.RelayMessage{
		Body: &workspacebrokerv1.RelayMessage_Hello{
			Hello: &workspacebrokerv1.RelayHello{SessionId: sessionID},
		},
	}); err != nil {
		_ = stream.CloseRequest()
		_ = stream.CloseResponse()
		return nil, fmt.Errorf("brokerclient: send Hello: %w", err)
	}
	adapter := &clientRelayAdapter{stream: stream}
	conn := workspacebroker.NewRelayConn(adapter, func() error {
		// Close both halves; ignore CloseRequest errors that come
		// from the server having already closed (io.EOF and the
		// connect "stream is closed" sentinel).
		_ = stream.CloseRequest()
		return stream.CloseResponse()
	})
	return conn, nil
}

// clientRelayAdapter adapts the client-side bidi stream as a
// workspacebroker.DataStream. RecvData returns io.EOF when the
// server closes the response side; SendData returns the
// transport's error verbatim.
type clientRelayAdapter struct {
	stream *connect.BidiStreamForClient[workspacebrokerv1.RelayMessage, workspacebrokerv1.RelayMessage]
}

func (a *clientRelayAdapter) RecvData() ([]byte, error) {
	msg, err := a.stream.Receive()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, err
	}
	return msg.GetData(), nil
}

func (a *clientRelayAdapter) SendData(data []byte) error {
	return a.stream.Send(&workspacebrokerv1.RelayMessage{
		Body: &workspacebrokerv1.RelayMessage_Data{Data: data},
	})
}
