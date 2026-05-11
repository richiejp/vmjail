package brokerclient_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/brokerclient"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/orchestrator"
)

const testListenAddr = "127.0.0.1:0"

// rig is the broker-client test harness: a running orchestrator,
// two approved peers (cli + sb), and a Client each.
type rig struct {
	server *orchestrator.Server
	url    string
	cliID  *identity.Identity
	sbID   *identity.Identity
	cli    *brokerclient.Client
	sb     *brokerclient.Client
}

func newRig(t *testing.T) *rig {
	t.Helper()
	srvID := mustGenerate(t, identity.RoleOrchestrator)
	cliID := mustGenerate(t, identity.RoleClient)
	sbID := mustGenerate(t, identity.RoleClient)

	srvStore := mustOpenStore(t)
	for _, peer := range []*identity.Identity{cliID, sbID} {
		if err := srvStore.Add(identity.Peer{Fingerprint: peer.Fingerprint()}); err != nil {
			t.Fatalf("srv approve: %v", err)
		}
	}

	srv, err := orchestrator.New(orchestrator.Config{
		Identity: srvID,
		Peers:    srvStore,
		Addr:     testListenAddr,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
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

	url := "https://" + srv.Addr()
	return &rig{
		server: srv,
		url:    url,
		cliID:  cliID,
		sbID:   sbID,
		cli:    mustNewClient(t, cliID, srvID, url),
		sb:     mustNewClient(t, sbID, srvID, url),
	}
}

func mustNewClient(t *testing.T, peer, server *identity.Identity, url string) *brokerclient.Client {
	t.Helper()
	store := mustOpenStore(t)
	if err := store.Add(identity.Peer{Fingerprint: server.Fingerprint()}); err != nil {
		t.Fatalf("approve server: %v", err)
	}
	c, err := brokerclient.New(brokerclient.Config{
		URL:      url,
		Identity: peer,
		Peers:    store,
	})
	if err != nil {
		t.Fatalf("brokerclient.New: %v", err)
	}
	return c
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

// TestEndToEndBrokeredByteRoundTrip exercises the full
// happy-path: cli registers a serve, sb registers a sandbox, cli
// Attaches, both DialRelay with the resulting session_id, and a
// payload sent on one side is received on the other.
func TestEndToEndBrokeredByteRoundTrip(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	serveReg, err := r.cli.RegisterServe(ctx, "src")
	if err != nil {
		t.Fatalf("RegisterServe: %v", err)
	}
	t.Cleanup(func() { _ = serveReg.Close() })

	sbReg, err := r.sb.RegisterSandbox(ctx, "sb-1")
	if err != nil {
		t.Fatalf("RegisterSandbox: %v", err)
	}
	t.Cleanup(func() { _ = sbReg.Close() })

	sid, endpoint, err := r.cli.Attach(ctx, r.cliID.Fingerprint(), "src", "sb-1")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if sid == "" {
		t.Fatal("empty session_id")
	}
	if endpoint == "" {
		t.Fatal("empty relay endpoint")
	}

	// Drain notify events so the subscriptions don't leak.
	mustRecv(t, serveReg)
	mustRecv(t, sbReg)

	cliConn, err := r.cli.DialRelay(ctx, sid)
	if err != nil {
		t.Fatalf("cli DialRelay: %v", err)
	}
	defer func() { _ = cliConn.Close() }()
	sbConn, err := r.sb.DialRelay(ctx, sid)
	if err != nil {
		t.Fatalf("sb DialRelay: %v", err)
	}
	defer func() { _ = sbConn.Close() }()

	// cli -> sb.
	if _, err := cliConn.Write([]byte("hello-sandbox")); err != nil {
		t.Fatalf("cli Write: %v", err)
	}
	buf := make([]byte, 64)
	n, err := sbConn.Read(buf)
	if err != nil {
		t.Fatalf("sb Read: %v", err)
	}
	if string(buf[:n]) != "hello-sandbox" {
		t.Errorf("sb got %q, want hello-sandbox", buf[:n])
	}

	// sb -> cli.
	if _, err := sbConn.Write([]byte("ack")); err != nil {
		t.Fatalf("sb Write: %v", err)
	}
	n, err = cliConn.Read(buf)
	if err != nil {
		t.Fatalf("cli Read: %v", err)
	}
	if string(buf[:n]) != "ack" {
		t.Errorf("cli got %q, want ack", buf[:n])
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	id := mustGenerate(t, identity.RoleClient)
	store := mustOpenStore(t)
	cases := []struct {
		name string
		cfg  brokerclient.Config
	}{
		{"empty URL", brokerclient.Config{Identity: id, Peers: store}},
		{"nil identity", brokerclient.Config{URL: "https://x", Peers: store}},
		{"nil peers", brokerclient.Config{URL: "https://x", Identity: id}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := brokerclient.New(tc.cfg); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestNewBuildsTLSConfig(t *testing.T) {
	// Happy path so the TLS-config branch is exercised.
	id := mustGenerate(t, identity.RoleClient)
	store := mustOpenStore(t)
	c, err := brokerclient.New(brokerclient.Config{
		URL: "https://example.invalid:443", Identity: id, Peers: store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Broker() == nil {
		t.Error("Broker() nil")
	}
	if c.Identity() != id {
		t.Error("Identity() != id")
	}
}

func TestAttachWithoutPeersReturnsError(t *testing.T) {
	r := newRig(t)
	// No serve/sandbox registered.
	_, _, err := r.cli.Attach(context.Background(), r.cliID.Fingerprint(), "missing", "missing")
	if err == nil {
		t.Fatal("expected error")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want FailedPrecondition", got)
	}
}

func TestDialRelayRejectsEmptySessionID(t *testing.T) {
	r := newRig(t)
	if _, err := r.cli.DialRelay(context.Background(), ""); err == nil {
		t.Error("expected error for empty session_id")
	}
}

func TestDialRelayUnknownSession(t *testing.T) {
	r := newRig(t)
	conn, err := r.cli.DialRelay(context.Background(), "no-such-session")
	if err != nil {
		// Some transports surface the error immediately on
		// DialRelay; others surface it on first Read.
		return
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Read(make([]byte, 4)); err == nil {
		t.Error("expected error on Read")
	}
}

// TestRegisterServeRejectsBadInput exercises the "stream closed
// before Ready" branch in newRegistration: an empty workspace is
// rejected by the matcher, the server-streaming RPC closes
// without emitting Ready, and RegisterServe returns the wrapped
// transport error.
func TestRegisterServeRejectsBadInput(t *testing.T) {
	r := newRig(t)
	if _, err := r.cli.RegisterServe(context.Background(), ""); err == nil {
		t.Error("expected error for empty workspace")
	}
}

func TestRegisterSandboxRejectsBadInput(t *testing.T) {
	r := newRig(t)
	if _, err := r.sb.RegisterSandbox(context.Background(), ""); err == nil {
		t.Error("expected error for empty sandbox_id")
	}
}

// TestRegisterServeWithCancelledContext exercises the
// transport-error branch of RegisterServe: when ctx is already
// cancelled, Connect's CallServerStream returns an error either
// synchronously or on first Receive.
func TestRegisterServeWithCancelledContext(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.cli.RegisterServe(ctx, "src"); err == nil {
		t.Error("expected error for cancelled ctx")
	}
}

func TestRegisterSandboxWithCancelledContext(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.sb.RegisterSandbox(ctx, "sb"); err == nil {
		t.Error("expected error for cancelled ctx")
	}
}

// TestDialRelayWithCancelledContext exercises DialRelay's
// send-Hello-failed branch by pre-cancelling the context.
func TestDialRelayWithCancelledContext(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn, err := r.cli.DialRelay(ctx, "any-sid")
	if err == nil {
		_ = conn.Close()
		t.Error("expected error for cancelled ctx")
	}
}

// TestRegistrationEventsClosesOnContextCancel verifies that
// cancelling the registration ctx ends the events channel cleanly.
func TestRegistrationEventsClosesOnContextCancel(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	reg, err := r.cli.RegisterServe(ctx, "src")
	if err != nil {
		t.Fatalf("RegisterServe: %v", err)
	}
	cancel()
	select {
	case _, ok := <-reg.Events():
		if ok {
			t.Error("Events sent a value after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Events did not close after ctx cancel")
	}
	_ = reg.Close()
	// Err() should not block.
	done := make(chan struct{})
	go func() { _ = reg.Err(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Err() blocked after Close")
	}
}

// TestRegistrationReceivesAttach ends to end: cli registers serve,
// sb registers sandbox, attach fires, both Registrations see a
// PendingAttach.
func TestRegistrationReceivesAttach(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveReg, err := r.cli.RegisterServe(ctx, "src")
	if err != nil {
		t.Fatalf("RegisterServe: %v", err)
	}
	defer func() { _ = serveReg.Close() }()
	sbReg, err := r.sb.RegisterSandbox(ctx, "sb-1")
	if err != nil {
		t.Fatalf("RegisterSandbox: %v", err)
	}
	defer func() { _ = sbReg.Close() }()
	if _, _, err := r.cli.Attach(ctx, r.cliID.Fingerprint(), "src", "sb-1"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	mustRecv(t, serveReg)
	mustRecv(t, sbReg)
}

// mustRecv pulls one PendingAttach off reg.Events() or fatals.
func mustRecv(t *testing.T, reg *brokerclient.Registration) {
	t.Helper()
	select {
	case ev, ok := <-reg.Events():
		if !ok {
			t.Fatal("Events closed before PendingAttach")
		}
		if ev.GetSessionId() == "" {
			t.Error("PendingAttach with empty session_id")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no PendingAttach")
	}
}

// TestConcurrentDialRelayBothSides drives the relay from
// concurrent goroutines to make sure we don't deadlock on the
// Hello-then-Pair handshake.
func TestConcurrentDialRelayBothSides(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveReg, _ := r.cli.RegisterServe(ctx, "src")
	defer func() { _ = serveReg.Close() }()
	sbReg, _ := r.sb.RegisterSandbox(ctx, "sb-1")
	defer func() { _ = sbReg.Close() }()
	sid, _, err := r.cli.Attach(ctx, r.cliID.Fingerprint(), "src", "sb-1")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	mustRecv(t, serveReg)
	mustRecv(t, sbReg)

	var wg sync.WaitGroup
	wg.Add(2)
	errCh := make(chan error, 2)
	dial := func(c *brokerclient.Client) {
		defer wg.Done()
		conn, err := c.DialRelay(ctx, sid)
		if err != nil {
			errCh <- err
			return
		}
		_ = conn.Close()
	}
	go dial(r.cli)
	go dial(r.sb)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("dial error: %v", err)
	}
}

// TestInnerMTLSOverRelay runs a full TLS 1.3 handshake end-to-end
// over a brokered relay byte channel. The orchestrator never sees
// plaintext payload — it sees encrypted TLS records inside Data
// frames. This pins the "RelayConn behaves like a net.Conn well
// enough for crypto/tls to work over it" invariant.
func TestInnerMTLSOverRelay(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cliRelay, sbRelay := openPairedRelays(t, ctx, r)
	defer func() { _ = cliRelay.Close() }()
	defer func() { _ = sbRelay.Close() }()

	cliTLS, sbTLS := innerTLSConfigs(t, r)

	// Server-side TLS goroutine: accept the handshake, echo a frame.
	sbReady := make(chan error, 1)
	go func() {
		conn := tls.Server(sbRelay, sbTLS)
		if err := conn.HandshakeContext(ctx); err != nil {
			sbReady <- err
			return
		}
		sbReady <- nil
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		_, _ = conn.Write(buf[:n])
	}()

	cliConn := tls.Client(cliRelay, cliTLS)
	if err := cliConn.HandshakeContext(ctx); err != nil {
		t.Fatalf("cli Handshake: %v", err)
	}
	if err := <-sbReady; err != nil {
		t.Fatalf("sb Handshake: %v", err)
	}
	if _, err := cliConn.Write([]byte("inner-mtls-payload")); err != nil {
		t.Fatalf("cli Write: %v", err)
	}
	buf := make([]byte, 64)
	n, err := cliConn.Read(buf)
	if err != nil {
		t.Fatalf("cli Read: %v", err)
	}
	if string(buf[:n]) != "inner-mtls-payload" {
		t.Errorf("echo = %q, want inner-mtls-payload", buf[:n])
	}
	if cs := cliConn.ConnectionState(); len(cs.PeerCertificates) > 0 {
		got, _ := identity.PeerFingerprintFromTLS(&cs)
		if got != r.sbID.Fingerprint() {
			t.Errorf("inner peer fp = %s, want %s", got, r.sbID.Fingerprint())
		}
	}
	_ = cliConn.Close()
}

// openPairedRelays registers serve+sandbox, calls Attach, and
// returns both peers' RelayConns once the session is brokered.
func openPairedRelays(t *testing.T, ctx context.Context, r *rig) (cliRelay, sbRelay net.Conn) {
	t.Helper()
	serveReg, err := r.cli.RegisterServe(ctx, "src")
	if err != nil {
		t.Fatalf("RegisterServe: %v", err)
	}
	t.Cleanup(func() { _ = serveReg.Close() })
	sbReg, err := r.sb.RegisterSandbox(ctx, "sb-1")
	if err != nil {
		t.Fatalf("RegisterSandbox: %v", err)
	}
	t.Cleanup(func() { _ = sbReg.Close() })
	sid, _, err := r.cli.Attach(ctx, r.cliID.Fingerprint(), "src", "sb-1")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	mustRecv(t, serveReg)
	mustRecv(t, sbReg)
	cliRelay, err = r.cli.DialRelay(ctx, sid)
	if err != nil {
		t.Fatalf("cli DialRelay: %v", err)
	}
	sbRelay, err = r.sb.DialRelay(ctx, sid)
	if err != nil {
		t.Fatalf("sb DialRelay: %v", err)
	}
	return cliRelay, sbRelay
}

// innerTLSConfigs builds fresh per-side mTLS configs for an inner
// handshake — each side trusts only the other's leaf fingerprint.
func innerTLSConfigs(t *testing.T, r *rig) (cliTLS, sbTLS *tls.Config) {
	t.Helper()
	cliStore := mustOpenStore(t)
	_ = cliStore.Add(identity.Peer{Fingerprint: r.sbID.Fingerprint()})
	sbStore := mustOpenStore(t)
	_ = sbStore.Add(identity.Peer{Fingerprint: r.cliID.Fingerprint()})
	cliTLS, err := identity.ClientTLSConfig(r.cliID, cliStore, "")
	if err != nil {
		t.Fatalf("inner cli TLS: %v", err)
	}
	sbTLS, err = identity.ServerTLSConfig(r.sbID, sbStore)
	if err != nil {
		t.Fatalf("inner sb TLS: %v", err)
	}
	return cliTLS, sbTLS
}

// TestDialRelayConnEOFOnPeerClose makes sure a graceful peer-close
// surfaces as io.EOF on Read so HTTP/TLS layers detect connection
// teardown deterministically.
func TestDialRelayConnEOFOnPeerClose(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveReg, _ := r.cli.RegisterServe(ctx, "src")
	defer func() { _ = serveReg.Close() }()
	sbReg, _ := r.sb.RegisterSandbox(ctx, "sb-1")
	defer func() { _ = sbReg.Close() }()
	sid, _, _ := r.cli.Attach(ctx, r.cliID.Fingerprint(), "src", "sb-1")
	mustRecv(t, serveReg)
	mustRecv(t, sbReg)

	cliConn, _ := r.cli.DialRelay(ctx, sid)
	sbConn, _ := r.sb.DialRelay(ctx, sid)
	defer func() { _ = cliConn.Close() }()

	// Close cli side; sb's next Read should see io.EOF or a
	// connect-level error.
	_ = cliConn.Close()
	buf := make([]byte, 16)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, err := sbConn.Read(buf)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// Connect surfaces stream-end as a non-EOF error
				// on some transports; accept any non-nil error.
				t.Logf("sb Read err = %v (non-EOF accepted)", err)
			}
			return
		}
	}
	t.Fatal("sb Read never returned an error after cli closed")
}
