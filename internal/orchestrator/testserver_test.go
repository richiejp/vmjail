package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/richiejp/darkaifactory/internal/identity"
)

// orchTestEnv is a started in-process orchestrator paired with an
// mTLS-approved caller identity. Per-service rigs (sandbox-ctrl,
// workspace-catalog) wrap this with their specific Connect client
// rather than re-implementing the listen + lifecycle + TLS dance
// that lint correctly flags as duplicated when copy-pasted.
type orchTestEnv struct {
	server   *Server
	callerID *identity.Identity
	httpc    *http.Client
	baseURL  string
}

// newOrchTestEnv brings up an orchestrator that accepts mTLS
// connections from a freshly-generated caller of the given role.
// cfg.Identity, cfg.Peers, and cfg.Addr are filled in by this
// helper; tests pass any other Config fields (e.g. NodeRegistry,
// WorkspaceCatalog) they need to observe.
func newOrchTestEnv(t *testing.T, callerRole identity.Role, cfg Config) *orchTestEnv {
	t.Helper()
	srvID := mustGenerate(t, identity.RoleOrchestrator)
	callerID := mustGenerate(t, callerRole)

	srvPeers, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "srv-peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore srv: %v", err)
	}
	if err := srvPeers.Add(identity.Peer{Fingerprint: callerID.Fingerprint(), Role: callerRole}); err != nil {
		t.Fatalf("srv approve caller: %v", err)
	}
	cfg.Identity = srvID
	cfg.Peers = srvPeers
	cfg.Addr = "127.0.0.1:0"

	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	var srvErrOnce sync.Once
	srvErr := make(chan error, 1)
	go func() {
		err := srv.Serve()
		srvErrOnce.Do(func() { srvErr <- err })
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case e := <-srvErr:
			if e != nil && !errors.Is(e, http.ErrServerClosed) {
				t.Logf("Serve: %v", e)
			}
		case <-time.After(2 * time.Second):
		}
	})

	callerPeers, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "caller-peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore caller: %v", err)
	}
	if err := callerPeers.Add(identity.Peer{Fingerprint: srvID.Fingerprint()}); err != nil {
		t.Fatalf("caller approve srv: %v", err)
	}
	tlsCfg, err := identity.ClientTLSConfig(callerID, callerPeers, "")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	return &orchTestEnv{
		server:   srv,
		callerID: callerID,
		httpc:    &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}},
		baseURL:  "https://" + srv.Addr(),
	}
}
