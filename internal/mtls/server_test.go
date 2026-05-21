package mtls

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/localai-org/darkaifactory/internal/identity"
)

const loopbackAny = "127.0.0.1:0"

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

func TestNewRejectsBadConfig(t *testing.T) {
	id := mustGenerate(t, identity.RoleOrchestrator)
	store := mustOpenStore(t)
	mux := http.NewServeMux()

	cases := map[string]Config{
		"nil identity":   {Peers: store, Handler: mux},
		"nil peers":      {Identity: id, Handler: mux},
		"nil handler":    {Identity: id, Peers: store},
		"empty identity": {Identity: &identity.Identity{}, Peers: store, Handler: mux},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(cfg); err == nil {
				t.Errorf("expected error for %s", name)
			}
		})
	}
}

func TestServerLifecycle(t *testing.T) {
	srvID := mustGenerate(t, identity.RoleOrchestrator)
	cliID := mustGenerate(t, identity.RoleClient)
	srvStore := mustOpenStore(t)
	cliStore := mustOpenStore(t)
	if err := srvStore.Add(identity.Peer{Fingerprint: cliID.Fingerprint()}); err != nil {
		t.Fatalf("server Add: %v", err)
	}
	if err := cliStore.Add(identity.Peer{Fingerprint: srvID.Fingerprint()}); err != nil {
		t.Fatalf("client Add: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/echo", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	srv, err := New(Config{
		Identity: srvID, Peers: srvStore, Addr: loopbackAny, Handler: mux,
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
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if got := srv.Addr(); got == "" {
		t.Error("Addr after Listen returned empty")
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
		Transport: &http2.Transport{TLSClientConfig: cliTLS},
		Timeout:   3 * time.Second,
	}
	resp, err := httpc.Get("https://" + srv.Addr() + "/echo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ok" {
		t.Errorf("body = %q, want ok", body)
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	srv, err := New(Config{
		Identity: mustGenerate(t, identity.RoleOrchestrator),
		Peers:    mustOpenStore(t),
		Addr:     loopbackAny,
		Handler:  http.NewServeMux(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("first shutdown: %v", err)
	}
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("second shutdown: %v", err)
	}
}

func TestListenFailsForBadAddr(t *testing.T) {
	srv, err := New(Config{
		Identity: mustGenerate(t, identity.RoleOrchestrator),
		Peers:    mustOpenStore(t),
		Addr:     "not-an-addr",
		Handler:  http.NewServeMux(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Listen(); err == nil {
		t.Fatal("expected Listen error")
	}
}

func TestNewDefaultsAddrToWildcard(t *testing.T) {
	// Empty Addr → 0.0.0.0:0; we don't actually listen, just
	// verify New accepts it.
	srv, err := New(Config{
		Identity: mustGenerate(t, identity.RoleOrchestrator),
		Peers:    mustOpenStore(t),
		Handler:  http.NewServeMux(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = srv
}

// Ensure tls.Config NextProtos was set correctly so HTTP/2 works.
func TestServerNegotiatesH2(t *testing.T) {
	id := mustGenerate(t, identity.RoleOrchestrator)
	store := mustOpenStore(t)
	srv, err := New(Config{
		Identity: id, Peers: store, Addr: loopbackAny,
		Handler: http.NewServeMux(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cfg := srv.hs.TLSConfig
	want := []string{"h2", "http/1.1"}
	if len(cfg.NextProtos) != len(want) {
		t.Fatalf("NextProtos = %v, want %v", cfg.NextProtos, want)
	}
	for i, p := range want {
		if cfg.NextProtos[i] != p {
			t.Errorf("NextProtos[%d] = %q, want %q", i, cfg.NextProtos[i], p)
		}
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %v, want TLS13", cfg.MinVersion)
	}
}
