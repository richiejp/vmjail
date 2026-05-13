// Package sandboxctrlclient is the sandbox-host side of the
// orchestrator's SandboxControlService. It wraps the generated
// Connect client with the outer mTLS transport, the
// RegisterSandbox stream lifecycle, and a heartbeat ticker that
// runs until the caller's context cancels.
//
// The package owns no domain state — it just speaks the wire
// protocol on behalf of `daft-sandboxd serve`.
//
// See PLAN.md "Direction (2026-05-12)" Phase A.
package sandboxctrlclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"github.com/richiejp/darkaifactory/internal/assert"
	sandboxctrlv1 "github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1/sandboxctrlv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/sandboxctrl"
)

// DefaultHeartbeatInterval is the cadence at which RegisterAndServe
// pings the orchestrator if Config.HeartbeatInterval is zero. The
// orchestrator's expiry policy (Slice B5) will be tuned to this
// default plus headroom; pick something that's clearly faster than
// any human-perceptible "is the sandbox alive?" delay but slow
// enough to be invisible in the trace tail.
const DefaultHeartbeatInterval = 5 * time.Second

// DispatchHandler is invoked once per Dispatch event the
// orchestrator pushes on the RegisterSandbox stream. Implementations
// run on the stream's consumer goroutine; long-running work should
// be moved off-thread by the handler itself.
//
// Returning an error tears down the registration — the client
// surfaces the error to its caller, and the orchestrator's `defer
// Forget` releases the entry. Non-error returns continue to drain
// the stream.
type DispatchHandler interface {
	HandleDispatch(ctx context.Context, dispatch *sandboxctrlv1.Dispatch) error
}

// Config is everything the client needs to dial the orchestrator
// and keep a sandbox registration alive.
type Config struct {
	// URL is the orchestrator's full https://host:port endpoint.
	URL string
	// Identity is the sandboxd's own keypair + cert.
	Identity *identity.Identity
	// Peers is the TOFU store containing the orchestrator's
	// fingerprint. Must be pre-approved by `daft auth approve`
	// (or equivalent) before this client will succeed.
	Peers *identity.PeerStore
	// Version is reported alongside the resource advertisement so
	// the orchestrator can reject too-old sandboxes.
	Version string
	// HeartbeatInterval is the ticker cadence. Zero ⇒
	// DefaultHeartbeatInterval.
	HeartbeatInterval time.Duration
	// Dispatch is the seam invoked on every Dispatch event from the
	// orchestrator. Nil ⇒ events are accepted off the wire and
	// dropped silently — used when running sandboxd in pure
	// register-and-heartbeat mode (e.g. operator inspecting a
	// fleet). Production wiring sets this to an executor that
	// drives the Hello / container pipeline (Slice A4b).
	Dispatch DispatchHandler
}

// Client wraps a generated SandboxControlServiceClient with a
// preconfigured mTLS transport.
type Client struct {
	cfg   Config
	httpc *http.Client
	ctrl  sandboxctrlv1connect.SandboxControlServiceClient
}

// New builds a Client. Returns an error if cfg is missing required
// fields or the TLS template can't be built.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("sandboxctrlclient: empty URL")
	}
	if cfg.Identity == nil {
		return nil, errors.New("sandboxctrlclient: nil identity")
	}
	if cfg.Peers == nil {
		return nil, errors.New("sandboxctrlclient: nil peer store")
	}
	tlsCfg, err := identity.ClientTLSConfig(cfg.Identity, cfg.Peers, "")
	if err != nil {
		return nil, fmt.Errorf("sandboxctrlclient: build TLS config: %w", err)
	}
	// No http.Client.Timeout: the RegisterSandbox stream lives for
	// the lifetime of the sandbox. Per-call cancellation is via
	// context.Context on each RPC.
	httpc := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}}
	return &Client{
		cfg:   cfg,
		httpc: httpc,
		ctrl:  sandboxctrlv1connect.NewSandboxControlServiceClient(httpc, cfg.URL),
	}, nil
}

// Identity returns the client's own identity.
func (c *Client) Identity() *identity.Identity { return c.cfg.Identity }

// RegisterAndServe opens the RegisterSandbox stream, blocks until
// the orchestrator's SubscriptionReady event lands, and then
// drives a Heartbeat ticker until ctx cancels or the stream
// errors. resources is what gets advertised at register time and
// echoed on every heartbeat.
//
// Returns nil for a clean ctx-cancel teardown; a non-nil error
// for stream failures or heartbeat RPC errors. The registration
// is released on the orchestrator side automatically when this
// function returns (the handler's defer h.reg.Forget runs).
func (c *Client) RegisterAndServe(ctx context.Context, resources sandboxctrl.Resources) error {
	done := ctx.Done()
	assert.True(done != nil,
		"RegisterAndServe blocks indefinitely; pass a cancellable context (SIGTERM handler / test t.Context())")

	interval := c.cfg.HeartbeatInterval
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}

	stream, err := c.ctrl.RegisterSandbox(ctx, connect.NewRequest(
		&sandboxctrlv1.RegisterSandboxRequest{
			Version:   c.cfg.Version,
			Resources: resourcesToProto(resources),
		},
	))
	if err != nil {
		return fmt.Errorf("sandboxctrlclient: RegisterSandbox: %w", err)
	}
	defer func() { _ = stream.Close() }()

	// First event must be SubscriptionReady — without it the
	// orchestrator has not actually recorded our registration.
	if !stream.Receive() {
		return fmt.Errorf("sandboxctrlclient: stream closed before Ready: %w", stream.Err())
	}
	if ev := stream.Msg().GetReady(); ev == nil {
		return errors.New("sandboxctrlclient: first event was not SubscriptionReady")
	}

	streamErr := make(chan error, 1)
	go consumeStream(ctx, stream, c.cfg.Dispatch, streamErr)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	//daft:lint:disable=forloopbound reason="exits on ctx.Done (asserted cancellable above), stream-end via streamErr, or unrecoverable heartbeat error"
	for {
		select {
		case <-done:
			return nil
		case err := <-streamErr:
			if err == nil {
				return errors.New("sandboxctrlclient: registration stream closed by server")
			}
			return fmt.Errorf("sandboxctrlclient: registration stream: %w", err)
		case <-ticker.C:
			if err := c.sendHeartbeat(ctx, resources); err != nil {
				return err
			}
		}
	}
}

// consumeStream drains the RegisterSandbox stream for the lifetime
// of the registration. Dispatch events go to handler when non-nil;
// nil handler drops them silently (register-only mode). A handler
// error tears the stream down by surfacing through errCh.
func consumeStream(
	ctx context.Context,
	stream *connect.ServerStreamForClient[sandboxctrlv1.RegisterEvent],
	handler DispatchHandler,
	errCh chan<- error,
) {
	//daft:lint:disable=forloopbound reason="bounded by stream lifetime; Receive returns false when the server closes or ctx cancels the underlying connection"
	for stream.Receive() {
		d := stream.Msg().GetDispatch()
		if d == nil || handler == nil {
			continue
		}
		if err := handler.HandleDispatch(ctx, d); err != nil {
			errCh <- fmt.Errorf("sandboxctrlclient: dispatch handler: %w", err)
			return
		}
	}
	errCh <- stream.Err()
}

func (c *Client) sendHeartbeat(ctx context.Context, resources sandboxctrl.Resources) error {
	_, err := c.ctrl.Heartbeat(ctx, connect.NewRequest(
		&sandboxctrlv1.HeartbeatRequest{Current: resourcesToProto(resources)},
	))
	if err != nil {
		return fmt.Errorf("sandboxctrlclient: Heartbeat: %w", err)
	}
	return nil
}

func resourcesToProto(r sandboxctrl.Resources) *sandboxctrlv1.HostResources {
	return &sandboxctrlv1.HostResources{
		Cpus:        r.CPUs,
		MemoryBytes: r.MemoryBytes,
	}
}
