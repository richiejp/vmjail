package identity

import (
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func mustGenerate(t *testing.T, role Role) *Identity {
	t.Helper()
	id, err := Generate(role)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return id
}

func mustOpenStore(t *testing.T) *PeerStore {
	t.Helper()
	s, err := OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	return s
}

func TestServerTLSConfigRejectsBadInputs(t *testing.T) {
	id := mustGenerate(t, RoleOrchestrator)
	store := mustOpenStore(t)

	cases := map[string]struct {
		id    *Identity
		store *PeerStore
	}{
		"nil identity": {nil, store},
		"nil store":    {id, nil},
		"empty identity": {
			&Identity{},
			store,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ServerTLSConfig(c.id, c.store); err == nil {
				t.Errorf("ServerTLSConfig: expected error")
			}
			if _, err := ClientTLSConfig(c.id, c.store, ""); err == nil {
				t.Errorf("ClientTLSConfig: expected error")
			}
		})
	}
}

func TestServerTLSConfigShape(t *testing.T) {
	id := mustGenerate(t, RoleOrchestrator)
	store := mustOpenStore(t)

	cfg, err := ServerTLSConfig(id, store)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAnyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAnyClientCert", cfg.ClientAuth)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %v, want TLS 1.3", cfg.MinVersion)
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Error("VerifyPeerCertificate should be set")
	}
}

func TestClientTLSConfigShape(t *testing.T) {
	id := mustGenerate(t, RoleClient)
	store := mustOpenStore(t)

	cfg, err := ClientTLSConfig(id, store, "host.example")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	if !cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify should be true (trust is via VerifyPeerCertificate)")
	}
	if cfg.ServerName != "host.example" {
		t.Errorf("ServerName = %q, want host.example", cfg.ServerName)
	}
}

func TestVerifierRejectsEmptyCerts(t *testing.T) {
	verify := makeVerifier(mustOpenStore(t))
	if err := verify(nil, nil); !errors.Is(err, ErrNoPeerCert) {
		t.Errorf("err = %v, want ErrNoPeerCert", err)
	}
}

func TestVerifierRejectsCorruptCert(t *testing.T) {
	verify := makeVerifier(mustOpenStore(t))
	if err := verify([][]byte{[]byte("garbage")}, nil); err == nil {
		t.Fatal("expected parse error for garbage cert")
	}
}

func TestVerifierRejectsUnknownFingerprint(t *testing.T) {
	id := mustGenerate(t, RoleClient)
	verify := makeVerifier(mustOpenStore(t))
	if err := verify([][]byte{id.DER}, nil); !errors.Is(err, ErrUnknownPeer) {
		t.Errorf("err = %v, want ErrUnknownPeer", err)
	}
}

func TestVerifierAcceptsApprovedFingerprint(t *testing.T) {
	id := mustGenerate(t, RoleClient)
	store := mustOpenStore(t)
	if err := store.Add(Peer{Fingerprint: id.Fingerprint(), Role: RoleClient}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	verify := makeVerifier(store)
	if err := verify([][]byte{id.DER}, nil); err != nil {
		t.Errorf("approved peer rejected: %v", err)
	}
}

// TestEndToEndMTLSHandshake spins up an httptest server with our
// ServerTLSConfig and dials it with our ClientTLSConfig. Both sides
// trust each other via PeerStore approvals; the handshake must
// succeed and a trivial GET must reach the handler. Then we revoke
// the client's approval and confirm the handshake fails.
func TestEndToEndMTLSHandshake(t *testing.T) {
	srvID := mustGenerate(t, RoleOrchestrator)
	cliID := mustGenerate(t, RoleClient)

	srvStore := mustOpenStore(t)
	cliStore := mustOpenStore(t)
	if err := srvStore.Add(Peer{Fingerprint: cliID.Fingerprint()}); err != nil {
		t.Fatalf("server Add: %v", err)
	}
	if err := cliStore.Add(Peer{Fingerprint: srvID.Fingerprint()}); err != nil {
		t.Fatalf("client Add: %v", err)
	}

	srvTLS, err := ServerTLSConfig(srvID, srvStore)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	srv.TLS = srvTLS
	srv.StartTLS()
	defer srv.Close()

	cliTLS, err := ClientTLSConfig(cliID, cliStore, "")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: cliTLS},
		Timeout:   5 * time.Second,
	}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("approved Get: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if err := resp.Body.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if string(body) != "ok" {
		t.Errorf("body = %q, want ok", string(body))
	}

	// Revoke the client's approval on the server side; next request
	// must fail at handshake.
	if err := srvStore.Remove(cliID.Fingerprint()); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// Burn the connection pool so the new handshake actually runs.
	client.CloseIdleConnections()
	resp2, err := client.Get(srv.URL)
	if err == nil {
		_ = resp2.Body.Close()
		t.Fatal("expected handshake failure after revoke")
	}
}

// TestEndToEndMTLSRejectsClientWithUnknownServer verifies the client
// side of the trust check: server is unknown to the client even
// though the client is approved by the server. Handshake fails.
func TestEndToEndMTLSRejectsClientWithUnknownServer(t *testing.T) {
	srvID := mustGenerate(t, RoleOrchestrator)
	cliID := mustGenerate(t, RoleClient)

	srvStore := mustOpenStore(t)
	cliStore := mustOpenStore(t) // client doesn't know the server.
	if err := srvStore.Add(Peer{Fingerprint: cliID.Fingerprint()}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	srvTLS, err := ServerTLSConfig(srvID, srvStore)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = srvTLS
	srv.StartTLS()
	defer srv.Close()

	cliTLS, err := ClientTLSConfig(cliID, cliStore, "")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: cliTLS},
		Timeout:   5 * time.Second,
	}
	resp, err := client.Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected client-side handshake failure")
	}
}

// Ensure low-level Dial path compiles and rejects an unapproved
// client at the TCP layer (not just at HTTP). Belt-and-braces for
// future transports that aren't HTTP/2.
func TestRawTLSHandshakeRejectsUnapprovedClient(t *testing.T) {
	srvID := mustGenerate(t, RoleOrchestrator)
	cliID := mustGenerate(t, RoleClient)

	srvStore := mustOpenStore(t) // server has no approvals.
	cliStore := mustOpenStore(t)
	if err := cliStore.Add(Peer{Fingerprint: srvID.Fingerprint()}); err != nil {
		t.Fatalf("client Add: %v", err)
	}

	srvTLS, err := ServerTLSConfig(srvID, srvStore)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvTLS)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()

	cliTLS, err := ClientTLSConfig(cliID, cliStore, "")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}

	conn, err := tls.Dial("tcp", ln.Addr().String(), cliTLS)
	if err == nil {
		_ = conn.Close()
		t.Fatal("expected handshake failure for unapproved client")
	}
}
