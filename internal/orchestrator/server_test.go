package orchestrator

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	healthv1 "github.com/richiejp/darkaifactory/internal/genpb/health/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/health/v1/healthv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
)

// testListenAddr is the "any free port on loopback" we use for all
// orchestrator tests. Defined once because golangci-lint's goconst
// catches the repetition otherwise.
const testListenAddr = "127.0.0.1:0"

// testPair builds a server + client identity, peer-stores that
// approve each other, and a started orchestrator listening on a
// random port. Returns the server's URL and a Connect client wired
// against it. All cleanup happens via t.Cleanup.
type testPair struct {
	server   *Server
	client   healthv1connect.HealthServiceClient
	cliStore *identity.PeerStore
	srvStore *identity.PeerStore
	cliID    *identity.Identity
	srvID    *identity.Identity
	url      string
}

func mustGenerate(t *testing.T, role identity.Role) *identity.Identity {
	t.Helper()
	id, err := identity.Generate(role)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return id
}

func mustOpenStore(t *testing.T) *identity.PeerStore {
	t.Helper()
	s, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	return s
}

func newTestPair(t *testing.T, approveClient, approveServer bool) *testPair {
	t.Helper()
	srvID := mustGenerate(t, identity.RoleOrchestrator)
	cliID := mustGenerate(t, identity.RoleClient)
	srvStore := mustOpenStore(t)
	cliStore := mustOpenStore(t)
	if approveClient {
		if err := srvStore.Add(identity.Peer{Fingerprint: cliID.Fingerprint()}); err != nil {
			t.Fatalf("server Add: %v", err)
		}
	}
	if approveServer {
		if err := cliStore.Add(identity.Peer{Fingerprint: srvID.Fingerprint()}); err != nil {
			t.Fatalf("client Add: %v", err)
		}
	}

	srv, err := New(Config{
		Identity: srvID,
		Peers:    srvStore,
		Addr:     testListenAddr,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() {
		if err := srv.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	cliTLS, err := identity.ClientTLSConfig(cliID, cliStore, "")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	httpc := &http.Client{
		Transport: &http2.Transport{
			TLSClientConfig: cliTLS,
		},
		Timeout: 5 * time.Second,
	}
	url := "https://" + srv.Addr()
	client := healthv1connect.NewHealthServiceClient(httpc, url)
	return &testPair{
		server:   srv,
		client:   client,
		cliStore: cliStore,
		srvStore: srvStore,
		cliID:    cliID,
		srvID:    srvID,
		url:      url,
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("expected error for missing identity")
	}
	if _, err := New(Config{Identity: mustGenerate(t, identity.RoleOrchestrator)}); err == nil {
		t.Error("expected error for missing peer store")
	}
	if _, err := New(Config{Identity: &identity.Identity{}, Peers: mustOpenStore(t)}); err == nil {
		t.Error("expected error for empty identity")
	}
}

func TestServerAddrEmptyBeforeListen(t *testing.T) {
	srv, err := New(Config{
		Identity: mustGenerate(t, identity.RoleOrchestrator),
		Peers:    mustOpenStore(t),
		Addr:     testListenAddr,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := srv.Addr(); got != "" {
		t.Errorf("Addr before Listen = %q, want empty", got)
	}
	if err := srv.Serve(); err == nil {
		t.Error("Serve before Listen should error")
	}
}

func TestPingApprovedPeer(t *testing.T) {
	p := newTestPair(t, true, true)
	resp, err := p.client.Ping(context.Background(), connect.NewRequest(&healthv1.PingRequest{Message: "hello"}))
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := resp.Msg.GetMessage(); got != "hello" {
		t.Errorf("response = %q, want %q", got, "hello")
	}
}

func TestPingRejectsUnapprovedClient(t *testing.T) {
	p := newTestPair(t, false, true) // server doesn't approve client.
	_, err := p.client.Ping(context.Background(), connect.NewRequest(&healthv1.PingRequest{Message: "x"}))
	if err == nil {
		t.Fatal("expected error for unapproved client")
	}
}

func TestPingRejectsUnknownServer(t *testing.T) {
	p := newTestPair(t, true, false) // client doesn't approve server.
	_, err := p.client.Ping(context.Background(), connect.NewRequest(&healthv1.PingRequest{Message: "x"}))
	if err == nil {
		t.Fatal("expected error for unknown server")
	}
}

func TestServerShutdownIsIdempotent(t *testing.T) {
	p := newTestPair(t, true, true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.server.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := p.server.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

func TestListenFailsForBadAddr(t *testing.T) {
	srv, err := New(Config{
		Identity: mustGenerate(t, identity.RoleOrchestrator),
		Peers:    mustOpenStore(t),
		Addr:     "not-a-valid-address",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Listen(); err == nil {
		t.Fatal("expected Listen error for malformed addr")
	}
}

// TestPingDirectHandler exercises the in-process handler without TLS
// to make sure the response body is wired up correctly. Belt-and-
// braces — the e2e tests above already cover the wire path, but a
// direct handler test makes regressions in the message-mapping much
// easier to localise.
func TestPingDirectHandler(t *testing.T) {
	h := NewHealthHandler()
	resp, err := h.Ping(context.Background(), connect.NewRequest(&healthv1.PingRequest{Message: "ping"}))
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := resp.Msg.GetMessage(); got != "ping" {
		t.Errorf("got %q, want ping", got)
	}
}

// shut linter up about unused if approveServer is false in some
// fixture wiring.
var _ = (*tls.Config)(nil)
