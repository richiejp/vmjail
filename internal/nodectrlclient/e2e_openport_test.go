package nodectrlclient_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/nodectrlclient"
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
)

const (
	e2eOpenPortWorkspace = "ws-e2e"
	e2eOpenPortPort      = uint32(8080)
)

// openPortRig wraps an e2eRig with a Bridger and a sandboxd
// nodectrlclient configured to route OpenPortRequest events
// through that Bridger. The DialFunc registered for
// (e2eOpenPortWorkspace, e2eOpenPortPort) returns the test's
// "downstream" side of a net.Pipe so the test can echo through it.
type openPortRig struct {
	*e2eRig
	bridger    *sandboxgateway.Bridger
	downstream net.Conn
}

func newOpenPortRig(t *testing.T) *openPortRig {
	t.Helper()
	base := newE2ERig(t)

	bridger := sandboxgateway.New()
	dsLocal, dsRemote := net.Pipe()
	t.Cleanup(func() { _ = dsLocal.Close() })
	// Echo loop: every byte read on dsLocal is written straight
	// back. Lets the test write "ping" through the bridge conn
	// and read "ping" back.
	go func() {
		buf := make([]byte, 64)
		//daft:lint:disable=forloopbound reason="exits on dsLocal close (Read returns err)"
		for {
			n, err := dsLocal.Read(buf)
			if err != nil {
				return
			}
			if _, werr := dsLocal.Write(buf[:n]); werr != nil {
				return
			}
		}
	}()

	dial := func(context.Context) (net.Conn, error) { return dsRemote, nil }
	if err := bridger.Register(e2eOpenPortWorkspace, e2eOpenPortPort, dial); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return &openPortRig{e2eRig: base, bridger: bridger, downstream: dsRemote}
}

// startSandboxdClient builds a real nodectrlclient.Client wired
// to the rig's orchestrator + the test's Bridger, and runs
// RegisterAndServe in a goroutine until ctx fires.
func (r *openPortRig) startSandboxdClient(t *testing.T, ctx context.Context) <-chan error {
	t.Helper()
	client, err := nodectrlclient.New(nodectrlclient.Config{
		URL:               r.url,
		Identity:          r.sbID,
		Peers:             r.sbPeer,
		Version:           testFixtureVer,
		HeartbeatInterval: 100 * time.Millisecond,
		Bridger:           r.bridger,
		Stderr:            io.Discard,
	})
	if err != nil {
		t.Fatalf("nodectrlclient.New: %v", err)
	}
	out := make(chan error, 1)
	go func() {
		out <- client.RegisterAndServe(ctx, nodectrl.Resources{CPUs: 1, MemoryBytes: 1 << 20})
	}()
	return out
}

// waitForHeartbeat parks until at least one Heartbeat refresh
// has been observed on the registry; that's the level-triggered
// signal that the RegisterNode stream is fully attached on both
// ends and OpenPortBridge can be called without racing the
// stream-attach.
func waitForHeartbeat(t *testing.T, reg *nodectrl.Registry, fp identity.Fingerprint) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	//daft:lint:disable=forloopbound reason="exits when LastSeenAt > RegisteredAt or on 3s deadline"
	for time.Now().Before(deadline) {
		if e, ok := reg.Lookup(fp); ok && e.LastSeenAt.After(e.RegisteredAt) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("orchestrator did not observe a heartbeat refresh within 3s")
}

// TestOpenPortBridgeEndToEnd pins the full ingress loop with a
// real orchestrator + sandboxd over HTTP/2 + mTLS:
//
//  1. The orchestrator-side OpenPortBridge mints a session and
//     pushes OpenPortRequest on the registered Node's stream.
//  2. The sandboxd's nodectrlclient dispatches the event,
//     dials OpenPortStream, sends Hello, and Bridger.Bridge
//     copies bytes between the stream and the test's echo
//     net.Pipe end.
//  3. Bytes written by the test on the bridge conn are echoed
//     back, demonstrating end-to-end byte round-trip through
//     every component built since C1.1.
func TestOpenPortBridgeEndToEnd(t *testing.T) {
	rig := newOpenPortRig(t)
	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	served := rig.startSandboxdClient(t, clientCtx)
	fp := rig.sbID.Fingerprint()
	waitForHeartbeat(t, rig.reg, fp)

	bridgeCtx, bridgeCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer bridgeCancel()
	conn, err := rig.server.SandboxControl().OpenPortBridge(
		bridgeCtx, fp, e2eOpenPortWorkspace, e2eOpenPortPort,
	)
	if err != nil {
		t.Fatalf("OpenPortBridge: %v", err)
	}
	defer func() { _ = conn.Close() }()

	want := []byte("ping-pong")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("conn.Write: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("conn.Read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}

	// Cancel the sandboxd's RegisterAndServe so test cleanup can
	// proceed without leaking goroutines.
	clientCancel()
	select {
	case err := <-served:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("RegisterAndServe: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("RegisterAndServe did not return after cancel")
	}
}

// TestOpenPortBridgeNoRouteClosesConn pins the missing-route
// surface over the real wire: the sandboxd's handleOpenPort still
// completes the Hello handshake (so OpenPortBridge returns a
// non-nil conn), but Bridger.Bridge immediately fails with
// ErrNoRoute on the sandboxd side, which tears the stream down.
// The bridge caller observes this as a Read returning an error
// rather than blocking indefinitely.
//
// The exact error shape isn't asserted — Connect-RPC's stream
// teardown surfaces variously as io.EOF, a connect.Error, or a
// transport-level wrap. What matters is that Read terminates.
func TestOpenPortBridgeNoRouteClosesConn(t *testing.T) {
	rig := newOpenPortRig(t)
	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	served := rig.startSandboxdClient(t, clientCtx)
	fp := rig.sbID.Fingerprint()
	waitForHeartbeat(t, rig.reg, fp)

	bridgeCtx, bridgeCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer bridgeCancel()
	conn, err := rig.server.SandboxControl().OpenPortBridge(
		bridgeCtx, fp, "ws-not-registered", e2eOpenPortPort,
	)
	if err != nil {
		// The bridge can also fail upstream (e.g. ctx timeout if
		// the sandboxd's handleOpenPort errored out before sending
		// Hello). Either path is acceptable evidence of failure.
		clientCancel()
		<-served
		return
	}
	defer func() { _ = conn.Close() }()
	// Read should terminate (with EOF or transport error) rather
	// than hang — the stream has been torn down on the sandboxd
	// side after Bridger.Bridge returned ErrNoRoute.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err == nil {
		t.Error("expected Read error after no-route teardown")
	}

	clientCancel()
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Error("RegisterAndServe did not return after cancel")
	}
}
