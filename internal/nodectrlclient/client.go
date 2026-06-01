// Package nodectrlclient is the sandbox-host side of the
// orchestrator's NodeControlService. It wraps the generated
// Connect client with the outer mTLS transport, the
// RegisterNode stream lifecycle, and a heartbeat ticker that
// runs until the caller's context cancels.
//
// The package owns no domain state — it just speaks the wire
// protocol on behalf of `daft-sandboxd serve`.
//
// See PLAN.md "Direction (2026-05-12)" Phase A.
package nodectrlclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	"github.com/localai-org/darkaifactory/internal/assert"
	nodectrlv1 "github.com/localai-org/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/nodectrl/v1/nodectrlv1connect"
	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/nodectrl"
)

// DefaultHeartbeatInterval is the cadence at which RegisterAndServe
// pings the orchestrator if Config.HeartbeatInterval is zero. The
// orchestrator's expiry policy (Slice B5) will be tuned to this
// default plus headroom; pick something that's clearly faster than
// any human-perceptible "is the sandbox alive?" delay but slow
// enough to be invisible in the trace tail.
const DefaultHeartbeatInterval = 5 * time.Second

// SandboxEventHandler is invoked once per SandboxWatchEvent the
// orchestrator pushes on the RegisterNode stream. Implementations
// run on the stream's consumer goroutine; long-running work should
// be moved off-thread by the handler itself.
//
// Returning an error tears down the registration — the client
// surfaces the error to its caller, and the orchestrator's `defer
// Forget` releases the entry. Non-error returns continue to drain
// the stream.
type SandboxEventHandler interface {
	HandleSandboxEvent(ctx context.Context, evt *nodectrlv1.SandboxWatchEvent) error
}

// SandboxEventHandlerFunc adapts a plain function to the
// SandboxEventHandler interface, mirroring net/http's HandlerFunc.
// Useful for tests and for trivial handlers that don't carry state.
type SandboxEventHandlerFunc func(ctx context.Context, evt *nodectrlv1.SandboxWatchEvent) error

// HandleSandboxEvent satisfies SandboxEventHandler.
func (f SandboxEventHandlerFunc) HandleSandboxEvent(ctx context.Context, evt *nodectrlv1.SandboxWatchEvent) error {
	return f(ctx, evt)
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
	// SandboxEvents is the seam invoked on every SandboxWatchEvent
	// pushed by the orchestrator. Nil ⇒ events are drained off the
	// wire and dropped silently — used when running sandboxd in
	// pure register-and-heartbeat mode (e.g. operator inspecting a
	// fleet). Production wiring sets this to an executor that
	// drives the Hello / container pipeline.
	SandboxEvents SandboxEventHandler
	// LiveSnapshot returns the wire-shaped set of sandboxes the
	// Node currently knows about. Called on every Heartbeat tick
	// to populate HeartbeatRequest.live_sandboxes — the
	// level-triggered signal that drives Workspace reconciliation
	// upstream (Slice B4d). Nil ⇒ the field is left empty, which
	// the orchestrator treats as "Node has no live sandboxes" (an
	// authoritative empty, not "no data").
	LiveSnapshot func() []*nodectrlv1.LiveSandbox
	// Bridger is the per-(workspace, port) routing table that
	// receives an inbound byte stream when the orchestrator
	// pushes an OpenPortRequest event. Nil ⇒ OpenPortRequest
	// events surface as bridge errors on Stderr (the
	// orchestrator-side session times out). Production wiring
	// (`daft-sandboxd serve`) instantiates one sandboxgateway.Bridger
	// and shares it with the sandboxexec Executor so the routes the
	// executor registers are the same table this transport reads from.
	Bridger PortBridger
	// AttachBridger is the per-sandbox attach factory invoked when
	// the orchestrator pushes an AttachRequest event (Slice D2a).
	// Nil ⇒ AttachRequest events surface as bridge errors on
	// Stderr. Production wiring (`daft-sandboxd serve`) wires this
	// to the agent-vsock implementation landing in D2b.
	AttachBridger AttachBridger
	// ObserverResolver resolves a sandbox_uid to its host-side
	// observer-log path when the orchestrator pushes an ObserverRequest
	// event (`daft sandbox observer-tail`). Nil ⇒ ObserverRequest
	// events surface as errors on Stderr. Production wiring
	// (`daft-sandboxd serve`) shares one sandboxgateway.ObserverRegistry
	// with the sandboxexec Executor so the routes the executor
	// registers are the same table this transport reads from.
	ObserverResolver ObserverResolver
	// SummaryResolver resolves a sandbox_uid to its host-side
	// build-progress summary path when the orchestrator pushes an
	// ObserverRequest with source=SUMMARY (`daft sandbox observe`).
	// Nil ⇒ such requests surface as errors on Stderr. Production wiring
	// (`daft-sandboxd serve`) shares one sandboxgateway.SummaryRegistry
	// with the sandboxexec Executor, alongside the ObserverRegistry.
	SummaryResolver ObserverResolver
	// Stderr is where per-bridge errors are logged. Defaults to
	// os.Stderr when nil.
	Stderr io.Writer
}

// openPortLogSink returns the configured Stderr, falling back to
// os.Stderr so a zero-Config still produces visible diagnostics.
func (c Config) openPortLogSink() io.Writer {
	if c.Stderr != nil {
		return c.Stderr
	}
	return os.Stderr
}

// Client wraps a generated NodeControlServiceClient with a
// preconfigured mTLS transport.
type Client struct {
	cfg   Config
	httpc *http.Client
	ctrl  nodectrlv1connect.NodeControlServiceClient
}

// New builds a Client. Returns an error if cfg is missing required
// fields or the TLS template can't be built.
func New(cfg Config) (*Client, error) {
	if cfg.URL == "" {
		return nil, errors.New("nodectrlclient: empty URL")
	}
	if cfg.Identity == nil {
		return nil, errors.New("nodectrlclient: nil identity")
	}
	if cfg.Peers == nil {
		return nil, errors.New("nodectrlclient: nil peer store")
	}
	tlsCfg, err := identity.ClientTLSConfig(cfg.Identity, cfg.Peers, "")
	if err != nil {
		return nil, fmt.Errorf("nodectrlclient: build TLS config: %w", err)
	}
	// No http.Client.Timeout: the RegisterNode stream lives for
	// the lifetime of the sandbox. Per-call cancellation is via
	// context.Context on each RPC.
	httpc := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}}
	return &Client{
		cfg:   cfg,
		httpc: httpc,
		ctrl:  nodectrlv1connect.NewNodeControlServiceClient(httpc, cfg.URL),
	}, nil
}

// Identity returns the client's own identity.
func (c *Client) Identity() *identity.Identity { return c.cfg.Identity }

// RegisterAndServe opens the RegisterNode stream, blocks until
// the orchestrator's SubscriptionReady event lands, and then
// drives a Heartbeat ticker until ctx cancels or the stream
// errors. resources is what gets advertised at register time and
// echoed on every heartbeat.
//
// Returns nil for a clean ctx-cancel teardown; a non-nil error
// for stream failures or heartbeat RPC errors. The registration
// is released on the orchestrator side automatically when this
// function returns (the handler's defer h.reg.Forget runs).
func (c *Client) RegisterAndServe(ctx context.Context, resources nodectrl.Resources) error {
	done := ctx.Done()
	assert.True(done != nil,
		"RegisterAndServe blocks indefinitely; pass a cancellable context (SIGTERM handler / test t.Context())")

	interval := c.cfg.HeartbeatInterval
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}

	stream, err := c.ctrl.RegisterNode(ctx, connect.NewRequest(
		&nodectrlv1.RegisterNodeRequest{
			Version:         c.cfg.Version,
			Resources:       resourcesToProto(resources),
			ProtocolVersion: localProtocolVersionProto(),
		},
	))
	if err != nil {
		return fmt.Errorf("nodectrlclient: RegisterNode: %w", err)
	}
	defer func() { _ = stream.Close() }()

	// First event must be SubscriptionReady — without it the
	// orchestrator has not actually recorded our registration.
	if !stream.Receive() {
		return fmt.Errorf("nodectrlclient: stream closed before Ready: %w", stream.Err())
	}
	ready := stream.Msg().GetReady()
	if ready == nil {
		return errors.New("nodectrlclient: first event was not SubscriptionReady")
	}
	// Server-side already validated us; now validate them. A
	// wire-incompatible orchestrator must not be allowed to drive
	// Dispatch into a sandboxd that can't decode the events.
	pv := ready.GetProtocolVersion()
	if err := nodectrl.CheckProtocolCompatibility(
		pv.GetMajor(), pv.GetMinor(), pv.GetPatch(),
	); err != nil {
		return err
	}

	streamErr := make(chan error, 1)
	go c.consumeStream(ctx, stream, streamErr)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	//daft:lint:disable=forloopbound reason="exits on ctx.Done (asserted cancellable above), stream-end via streamErr, or unrecoverable heartbeat error"
	for {
		select {
		case <-done:
			return nil
		case err := <-streamErr:
			if err == nil {
				return errors.New("nodectrlclient: registration stream closed by server")
			}
			return fmt.Errorf("nodectrlclient: registration stream: %w", err)
		case <-ticker.C:
			if err := c.sendHeartbeat(ctx, resources); err != nil {
				return err
			}
		}
	}
}

// consumeStream drains the RegisterNode stream for the lifetime of
// the registration. SandboxWatchEvents go to the configured
// SandboxEventHandler; OpenPortRequest events go to the Client's
// own handleOpenPort. A handler error on either path tears the
// stream down by surfacing through errCh.
func (c *Client) consumeStream(
	ctx context.Context,
	stream *connect.ServerStreamForClient[nodectrlv1.NodeEvent],
	errCh chan<- error,
) {
	//daft:lint:disable=forloopbound reason="bounded by stream lifetime; Receive returns false when the server closes or ctx cancels the underlying connection"
	for stream.Receive() {
		if err := c.dispatchEvent(ctx, stream.Msg()); err != nil {
			errCh <- err
			return
		}
	}
	errCh <- stream.Err()
}

// dispatchEvent demuxes one NodeEvent to its handler. Returns the
// wrapped handler error (which tears the stream down) or nil to
// continue draining. SubscriptionReady is consumed in
// RegisterAndServe before the loop starts, so seeing one here is
// an orchestrator-side bug — surface it rather than ignoring.
func (c *Client) dispatchEvent(ctx context.Context, msg *nodectrlv1.NodeEvent) error {
	if sb := msg.GetSandbox(); sb != nil {
		if c.cfg.SandboxEvents == nil {
			return nil
		}
		// A sandbox handler blocks for the VM's lifetime; running
		// it inline would freeze the registration-stream loop and
		// every follow-up AttachRequest / OpenPort would hang
		// until the sandbox exited. Spawn it like the other event
		// kinds. Handler errors that previously tore the stream
		// down now surface via the log sink — losing the stream
		// for an unrelated dispatch would be a worse failure mode
		// than a logged error per dispatch.
		go func() {
			if err := c.cfg.SandboxEvents.HandleSandboxEvent(ctx, sb); err != nil {
				_, _ = fmt.Fprintf(c.cfg.openPortLogSink(),
					"nodectrlclient: sandbox-event handler: %v\n", err)
			}
		}()
		return nil
	}
	if op := msg.GetOpenPort(); op != nil {
		// Run the bridge in its own goroutine — a single
		// long-lived attach must not block subsequent events on
		// the same registration stream.
		go func() {
			if err := c.handleOpenPort(ctx, op); err != nil {
				_, _ = fmt.Fprintf(c.cfg.openPortLogSink(), "nodectrlclient: openport bridge: %v\n", err)
			}
		}()
		return nil
	}
	if at := msg.GetAttach(); at != nil {
		go func() {
			if err := c.handleAttach(ctx, at); err != nil {
				_, _ = fmt.Fprintf(c.cfg.openPortLogSink(), "nodectrlclient: attach bridge: %v\n", err)
			}
		}()
		return nil
	}
	if obs := msg.GetObserver(); obs != nil {
		// Own goroutine — a long-lived follow tail must not block
		// subsequent events on the registration stream.
		go func() {
			if err := c.handleObserver(ctx, obs); err != nil {
				_, _ = fmt.Fprintf(c.cfg.openPortLogSink(), "nodectrlclient: observer tail: %v\n", err)
			}
		}()
		return nil
	}
	return nil
}

func (c *Client) sendHeartbeat(ctx context.Context, resources nodectrl.Resources) error {
	req := &nodectrlv1.HeartbeatRequest{Current: resourcesToProto(resources)}
	if c.cfg.LiveSnapshot != nil {
		req.LiveSandboxes = c.cfg.LiveSnapshot()
	}
	_, err := c.ctrl.Heartbeat(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("nodectrlclient: Heartbeat: %w", err)
	}
	return nil
}

func resourcesToProto(r nodectrl.Resources) *nodectrlv1.HostResources {
	return &nodectrlv1.HostResources{
		Cpus:        r.CPUs,
		MemoryBytes: r.MemoryBytes,
	}
}

// localProtocolVersionProto builds the wire ProtocolVersion message
// from this build's compiled-in constants. Centralized so the value
// is set identically on every outbound RegisterNode.
func localProtocolVersionProto() *nodectrlv1.ProtocolVersion {
	return &nodectrlv1.ProtocolVersion{
		Major: nodectrl.ProtocolVersionMajor,
		Minor: nodectrl.ProtocolVersionMinor,
		Patch: nodectrl.ProtocolVersionPatch,
	}
}
