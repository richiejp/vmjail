// Package orchestrator hosts the central daft control-plane: it
// authenticates clients and sandbox-hosts via mTLS (with TOFU peer
// approvals from internal/identity) and serves Connect-RPC endpoints
// over HTTP/2 — currently Health.Ping, TraceService.ReportSpans,
// the workspace BrokerService (matcher only; relay lands later),
// and a /debug/traces dump endpoint.
//
// The actual mTLS HTTP/2 listener lives in internal/mtls — this
// package is just the orchestrator-specific service surface.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/richiejp/darkaifactory/internal/component"
	"github.com/richiejp/darkaifactory/internal/genpb/health/v1/healthv1connect"
	"github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1/nodectrlv1connect"
	"github.com/richiejp/darkaifactory/internal/genpb/trace/v1/tracev1connect"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/genpb/workspacebroker/v1/workspacebrokerv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/mtls"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/reconciler"
	"github.com/richiejp/darkaifactory/internal/trace"
	"github.com/richiejp/darkaifactory/internal/workspacebroker"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

// OCIRegistryPathPrefix is the HTTP path the orchestrator's embedded
// OCI registry is mounted at (the OCI distribution-spec root). Exported
// so the sandbox-host puller can recognise orchestrator-hosted image
// refs and pushers can target the right URL.
const OCIRegistryPathPrefix = "/v2/"

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

	// Matcher pairs `daft workspace serve` peers with sandbox-host
	// peers. New constructs a default matcher if this is nil.
	// Tests pass a pre-populated matcher to inspect state.
	Matcher *workspacebroker.Matcher

	// NodeRegistry tracks live sandbox-host registrations
	// (`daft-sandboxd serve` callers) and their heartbeats. New
	// constructs a default registry if this is nil. Tests pass a
	// pre-built registry to observe state directly.
	NodeRegistry *nodectrl.Registry

	// WorkspaceCatalog is the orchestrator's store of Workspace
	// records (PLAN.md Phase B). New constructs a default catalog
	// if this is nil. The Connect-RPC service surface that exposes
	// the catalog to clients lands in Slice B2.
	WorkspaceCatalog *workspacecatalog.Store

	// WorkspaceDispatcher backs WorkspaceCatalogService.Launch.
	// Defaults to the orchestrator's own NodeControlHandler so
	// production wiring is implicit; tests inject a fake to
	// observe the SandboxSpec without standing up a sandbox-host
	// stream.
	WorkspaceDispatcher Dispatcher

	// WorkspacePortBridger backs WorkspaceCatalogService.ForwardPort
	// (Slice C3.1). Defaults to the orchestrator's own
	// NodeControlHandler (its OpenPortBridge); tests inject a fake
	// that returns a pre-paired net.Conn so the splice loop can be
	// exercised without a real sandboxd.
	WorkspacePortBridger WorkspacePortBridger

	// WorkspaceAttachBridger backs WorkspaceCatalogService.AttachSandbox
	// (Slice D2a). Defaults to the orchestrator's own
	// NodeControlHandler (its OpenAttachBridge); tests inject a
	// fake that returns a pre-paired AttachSession.
	WorkspaceAttachBridger WorkspaceAttachBridger

	// WorkspaceObserverBridger backs WorkspaceCatalogService.ObserverTail.
	// Defaults to the orchestrator's own NodeControlHandler (its
	// OpenObserverBridge); tests inject a fake returning a canned
	// io.ReadCloser.
	WorkspaceObserverBridger WorkspaceObserverBridger

	// Reconciler tunables (Slice B5). Zero values fall through to
	// the reconciler package's defaults — tests usually override
	// MissThreshold and SweepInterval to fire quickly.
	ReconcilerMissThreshold int
	ReconcilerSweepInterval time.Duration
	ReconcilerNodeGrace     time.Duration

	// RegistrySeeds are images loaded into the embedded OCI registry at
	// construction, before the listener binds — so they are pullable the
	// moment a node registers. The daft-orchestrator binary uses this to
	// auto-seed its bundled build image (internal/buildimage) under
	// ociregistry.BuiltinBuildRef(), so a build-based devcontainer needs no
	// DAFT_BUILD_IMAGE_REF. Kept generic (an image + ref) so internal/
	// orchestrator never imports the embed and stays testable with a tiny
	// fake image.
	RegistrySeeds []RegistrySeed
}

// RegistrySeed is an image to load into the embedded registry at startup.
type RegistrySeed struct {
	// Ref is the orchestrator-hosted ref to push under (e.g.
	// ociregistry.BuiltinBuildRef()).
	Ref string
	// Image is the image to push. Lazy images are fine — the layers are
	// read during the seed push.
	Image v1.Image
}

// Server is the orchestrator's mTLS-gated control-plane listener.
// It's a thin wrapper around mtls.Server that registers the
// orchestrator's Connect-RPC handlers (Health.Ping,
// TraceService.ReportSpans, BrokerService, NodeControlService)
// plus a /debug/traces dump endpoint.
type Server struct {
	inner            *mtls.Server
	tracer           *trace.Tracer
	reaper           *trace.IngressReaper
	matcher          *workspacebroker.Matcher
	nodeReg          *nodectrl.Registry
	sandboxCtrl      *NodeControlHandler
	workspaceCatalog *workspacecatalog.Store
	reconciler       *reconciler.Reconciler

	reaperStart  sync.Once
	reaperStop   sync.Once
	reaperCancel context.CancelFunc
	reaperDone   chan struct{}

	reconcilerStart  sync.Once
	reconcilerStop   sync.Once
	reconcilerCancel context.CancelFunc
	reconcilerDone   chan struct{}
}

// Matcher returns the orchestrator's workspace-broker matcher.
// Exposed for tests that want to assert matcher state directly.
func (s *Server) Matcher() *workspacebroker.Matcher { return s.matcher }

// NodeRegistry returns the orchestrator's sandbox-control
// registry. Exposed for tests that want to assert registration
// state directly, and (Slice A2) for ListNodes.
func (s *Server) NodeRegistry() *nodectrl.Registry { return s.nodeReg }

// SandboxControl returns the orchestrator's sandbox-control
// handler. Exposed so operator tooling (and tests) can call
// Dispatch on a live sandbox without a separate RPC client.
func (s *Server) SandboxControl() *NodeControlHandler { return s.sandboxCtrl }

// WorkspaceCatalog returns the orchestrator's workspace catalog.
// Exposed for tests that want to assert catalog state directly,
// and (Slice B2) for the workspace-service handlers.
func (s *Server) WorkspaceCatalog() *workspacecatalog.Store { return s.workspaceCatalog }

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
	matcher := cfg.Matcher
	if matcher == nil {
		matcher = workspacebroker.New()
	}
	nodeReg := cfg.NodeRegistry
	if nodeReg == nil {
		nodeReg = nodectrl.NewRegistry(nil)
	}
	workspaceCat := cfg.WorkspaceCatalog
	if workspaceCat == nil {
		workspaceCat = workspacecatalog.NewStore(nil)
	}
	sandboxCtrl, scErr := NewNodeControlHandler(nodeReg)
	if scErr != nil {
		return nil, scErr
	}
	workspaceDispatcher := cfg.WorkspaceDispatcher
	if workspaceDispatcher == nil {
		workspaceDispatcher = sandboxCtrl
	}
	workspaceCtrl, wcErr := NewWorkspaceCatalogHandler(workspaceCat, workspaceDispatcher)
	if wcErr != nil {
		return nil, wcErr
	}
	// Reconciler (Slice B5) owns per-heartbeat reconcile, the
	// missing-heartbeat threshold → SandboxLost, periodic sweep
	// for unreachable Nodes, and the Launch/Stop pre-action hook.
	rec := buildReconciler(cfg, workspaceCat, nodeReg)
	sandboxCtrl.SetReconciler(rec)
	workspaceCtrl.SetPreActionReconciler(rec)
	workspaceCtrl.SetNodeLister(nodeReg)
	wireWorkspaceBridgers(workspaceCtrl, cfg, sandboxCtrl)
	// The broker handler needs the orchestrator's bound address
	// to return as the relay endpoint, but the listener doesn't
	// bind until Listen(). Close over a late-bound *mtls.Server
	// that gets filled in once mtls.New returns below.
	var inner *mtls.Server
	getRelayEndpoint := func() string {
		if inner == nil {
			return ""
		}
		return inner.Addr()
	}
	if err := registerServices(mux, interceptor, tracer, matcher, sandboxCtrl, workspaceCtrl, getRelayEndpoint, cfg.RegistrySeeds); err != nil {
		return nil, err
	}
	handler := withPeerMiddleware(mux)

	var err error
	inner, err = mtls.New(mtls.Config{
		Identity:          cfg.Identity,
		Peers:             cfg.Peers,
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	})
	if err != nil {
		return nil, err
	}
	reaper := cfg.Reaper
	if reaper == nil {
		reaper = trace.NewIngressReaper(tracer, component.Orchestrator)
	}
	return &Server{
		inner:            inner,
		tracer:           tracer,
		reaper:           reaper,
		matcher:          matcher,
		nodeReg:          nodeReg,
		sandboxCtrl:      sandboxCtrl,
		workspaceCatalog: workspaceCat,
		reconciler:       rec,
	}, nil
}

// buildReconciler constructs the Reconciler with Config-supplied
// tunables (when set) and the orchestrator's catalog + node
// registry. Extracted from New so the field-wiring stanza there
// stays readable.
func buildReconciler(cfg Config, cat *workspacecatalog.Store, nodes *nodectrl.Registry) *reconciler.Reconciler {
	return reconciler.New(reconciler.Config{
		Catalog:       cat,
		Nodes:         nodes,
		MissThreshold: cfg.ReconcilerMissThreshold,
		SweepInterval: cfg.ReconcilerSweepInterval,
		NodeGrace:     cfg.ReconcilerNodeGrace,
	})
}

// resolvePortBridger returns cfg.WorkspacePortBridger when set,
// otherwise the orchestrator's own NodeControlHandler — the
// production wiring path that routes ForwardPort through the
// same Bridger the sandboxd registers routes on.
func resolvePortBridger(cfg Config, sandboxCtrl *NodeControlHandler) WorkspacePortBridger {
	if cfg.WorkspacePortBridger != nil {
		return cfg.WorkspacePortBridger
	}
	return sandboxCtrl
}

// resolveAttachBridger returns cfg.WorkspaceAttachBridger when
// set, otherwise the orchestrator's own NodeControlHandler.
// Production wiring; tests inject a fake to exercise the splice
// loop without a sandboxd.
func resolveAttachBridger(cfg Config, sandboxCtrl *NodeControlHandler) WorkspaceAttachBridger {
	if cfg.WorkspaceAttachBridger != nil {
		return cfg.WorkspaceAttachBridger
	}
	return sandboxCtrl
}

// resolveObserverBridger returns cfg.WorkspaceObserverBridger when
// set, otherwise the orchestrator's own NodeControlHandler. Same
// shape as resolveAttachBridger.
func resolveObserverBridger(cfg Config, sandboxCtrl *NodeControlHandler) WorkspaceObserverBridger {
	if cfg.WorkspaceObserverBridger != nil {
		return cfg.WorkspaceObserverBridger
	}
	return sandboxCtrl
}

// wireWorkspaceBridgers attaches the port/attach/observer bridgers to
// the workspace handler, each falling back to the orchestrator's own
// NodeControlHandler. Factored out of New to keep it under funlen.
func wireWorkspaceBridgers(workspaceCtrl *WorkspaceCatalogHandler, cfg Config, sandboxCtrl *NodeControlHandler) {
	workspaceCtrl.SetPortBridger(resolvePortBridger(cfg, sandboxCtrl))
	workspaceCtrl.SetAttachBridger(resolveAttachBridger(cfg, sandboxCtrl))
	workspaceCtrl.SetObserverBridger(resolveObserverBridger(cfg, sandboxCtrl))
}

// registerServices installs every Connect-RPC handler plus the
// /debug/traces endpoint on mux. Extracted from New so that Slice
// A's incremental services keep New itself under funlen's cap.
func registerServices(
	mux *http.ServeMux,
	interceptor connect.HandlerOption,
	tracer *trace.Tracer,
	matcher *workspacebroker.Matcher,
	sandboxCtrl *NodeControlHandler,
	workspaceCtrl *WorkspaceCatalogHandler,
	relayEndpointFn func() string,
	registrySeeds []RegistrySeed,
) error {
	healthPath, healthHandler := healthv1connect.NewHealthServiceHandler(
		NewHealthHandler(), interceptor,
	)
	mux.Handle(healthPath, healthHandler)

	traceHandler, err := NewTraceHandler(tracer)
	if err != nil {
		return err
	}
	tracePath, traceConnectHandler := tracev1connect.NewTraceServiceHandler(
		traceHandler, interceptor,
	)
	mux.Handle(tracePath, traceConnectHandler)
	mux.Handle("/debug/traces", debugTracesHandler(tracer))

	brokerHandler, err := NewBrokerHandler(matcher, relayEndpointFn)
	if err != nil {
		return err
	}
	brokerPath, brokerConnectHandler := workspacebrokerv1connect.NewBrokerServiceHandler(
		brokerHandler, interceptor,
	)
	mux.Handle(brokerPath, brokerConnectHandler)

	sandboxCtrlPath, sandboxCtrlConnectHandler := nodectrlv1connect.NewNodeControlServiceHandler(
		sandboxCtrl, interceptor,
	)
	mux.Handle(sandboxCtrlPath, sandboxCtrlConnectHandler)

	workspaceCtrlPath, workspaceCtrlConnectHandler := workspacev1connect.NewWorkspaceCatalogServiceHandler(
		workspaceCtrl, interceptor,
	)
	mux.Handle(workspaceCtrlPath, workspaceCtrlConnectHandler)

	// Embedded OCI registry (PLAN.md item 8): served on the same mTLS
	// endpoint, so the existing peer gate is its access control — only
	// approved sandbox-hosts and clients can push or pull. In-memory and
	// ephemeral; the minimum needed to host workspace images without an
	// external registry. The /v2/ prefix never collides with the Connect
	// service paths (all /daft.*.v1.*/...). The registry's request log is
	// discarded to keep it off the orchestrator's stdout.
	regHandler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	if err := seedRegistry(regHandler, registrySeeds); err != nil {
		return err
	}
	mux.Handle(OCIRegistryPathPrefix, regHandler)
	return nil
}

// seedRegistry loads each seed image into the embedded registry handler
// before the listener binds, so the images are pullable the moment a node
// registers. Used by the daft-orchestrator binary to auto-seed its bundled
// build image.
//
// It serves the same handler instance on a throwaway loopback HTTP listener
// and pushes over plain HTTP: the registry stores blobs/manifests keyed by
// repository path (not host), so an image pushed as
// 127.0.0.1:PORT/<repo>:<tag> is later served for the hosted
// daft-registry.internal/<repo>:<tag> through the orchestrator's mTLS mux —
// same handler, same in-memory store. (A direct in-process RoundTripper is
// too lossy: the registry + go-containerregistry pusher rely on real upload
// URL / Location-header resolution.)
// seedListen is the listener factory seedRegistry uses, a test seam so the
// listener-failure path is exercisable.
//
//nolint:gochecknoglobals // single test seam, mirrors buildDispatcher.
var seedListen = net.Listen

func seedRegistry(h http.Handler, seeds []RegistrySeed) error {
	if len(seeds) == 0 {
		return nil
	}
	ln, err := seedListen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("orchestrator: seed registry listener: %w", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	host := ln.Addr().String()

	for _, s := range seeds {
		repoTag, err := hostedRepoTag(s.Ref)
		if err != nil {
			return err
		}
		// crane.Insecure → plain HTTP to the loopback listener.
		if err := crane.Push(s.Image, host+"/"+repoTag, crane.Insecure); err != nil {
			return fmt.Errorf("orchestrator: seed registry image %s: %w", s.Ref, err)
		}
	}
	return nil
}

// hostedRepoTag strips a hosted ref's registry host, returning "<repo>:<tag>"
// — the host-independent storage key the embedded registry uses, so a seed
// pushed via loopback is served for the same repo under the sentinel host.
func hostedRepoTag(ref string) (string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("orchestrator: parse seed ref %q: %w", ref, err)
	}
	return r.Context().RepositoryStr() + ":" + r.Identifier(), nil
}

// Tracer returns the orchestrator's Tracer. Callers can use it to
// emit additional spans (e.g. from background workers) or to read
// the buffer directly for tests.
func (s *Server) Tracer() *trace.Tracer { return s.tracer }

func (s *Server) Listen() error { return s.inner.Listen() }
func (s *Server) Addr() string  { return s.inner.Addr() }

// Serve starts the ingress reaper + the reconciler sweep, then
// blocks on the inner HTTP server. Both background lifetimes span
// the Serve→Shutdown window.
func (s *Server) Serve() error {
	s.startReaper()
	s.startReconciler()
	return s.inner.Serve()
}

// Shutdown stops the reconciler and reaper before draining the
// HTTP server so neither can race a teardown that's about to lose
// its span sources.
func (s *Server) Shutdown(ctx context.Context) error {
	s.stopReconciler()
	s.stopReaper()
	return s.inner.Shutdown(ctx)
}

func (s *Server) startReconciler() {
	s.reconcilerStart.Do(func() {
		if s.reconciler == nil {
			return
		}
		// Reconciler lifetime is Serve→Shutdown, not
		// request-scoped — same shape as the reaper.
		ctx, cancel := context.WithCancel(context.Background()) //nolint:contextcheck // see above
		s.reconcilerCancel = cancel
		s.reconcilerDone = make(chan struct{})
		go func() {
			defer close(s.reconcilerDone)
			_ = s.reconciler.Run(ctx)
		}()
	})
}

func (s *Server) stopReconciler() {
	s.reconcilerStop.Do(func() {
		if s.reconcilerCancel == nil {
			return
		}
		s.reconcilerCancel()
		<-s.reconcilerDone
	})
}

func (s *Server) startReaper() {
	s.reaperStart.Do(func() {
		// Reaper lifetime is Serve→Shutdown, not request-scoped.
		ctx, cancel := context.WithCancel(context.Background()) //nolint:contextcheck // see above
		s.reaperCancel = cancel
		s.reaperDone = make(chan struct{})
		go func() {
			defer close(s.reaperDone)
			s.reaper.Run(ctx)
		}()
	})
}

func (s *Server) stopReaper() {
	s.reaperStop.Do(func() {
		if s.reaperCancel == nil {
			return
		}
		s.reaperCancel()
		<-s.reaperDone
	})
}
