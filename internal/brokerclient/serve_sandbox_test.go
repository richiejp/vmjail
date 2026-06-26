package brokerclient_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/brokerclient"
	healthv1 "github.com/richiejp/darkaifactory/internal/genpb/health/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/health/v1/healthv1connect"
)

// TestServeBrokeredAsSandboxRoundTrip is the role-inverted twin of
// TestServeBrokeredAndAttachRoundTrip: the *node* (sb) hosts the
// handler via ServeBrokeredAsSandbox while the *client* (cli) drives
// it via DialBrokeredServe — the client is both serve owner and
// initiator, the node stays passive. A Connect Health.Ping over inner
// mTLS confirms request/response round-tripping end to end. This is
// the transport the workspace sync push/pull rides.
func TestServeBrokeredAsSandboxRoundTrip(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Node side: register as a sandbox keyed by the node's own
	// fingerprint, hosting the Health handler.
	mux := http.NewServeMux()
	healthPath, healthHandler := healthv1connect.NewHealthServiceHandler(healthMessage{})
	mux.Handle(healthPath, healthHandler)

	nodeFP := r.sbID.Fingerprint()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- r.sb.ServeBrokeredAsSandbox(ctx, nodeFP.String(), brokerclient.ServeOptions{
			Handler: mux,
		})
	}()

	// Wait for the node's sandbox registration to land in the matcher
	// before the client initiates, else RequestAttach sees ErrNoSandbox.
	waitForSandbox(t, r)

	ic, err := r.cli.DialBrokeredServe(ctx, brokerclient.DialServeOptions{
		Workspace:       testWorkspaceName,
		NodeFingerprint: nodeFP,
	})
	if err != nil {
		t.Fatalf("DialBrokeredServe: %v", err)
	}
	defer func() { _ = ic.Close() }()

	hc := healthv1connect.NewHealthServiceClient(ic.HTTPClient, ic.BaseURL)
	resp, err := hc.Ping(ctx, connect.NewRequest(&healthv1.PingRequest{Message: "sync-roundtrip"}))
	if err != nil {
		t.Fatalf("inner Ping: %v", err)
	}
	if resp.Msg.GetMessage() != "sync-roundtrip" {
		t.Errorf("echo = %q, want sync-roundtrip", resp.Msg.GetMessage())
	}

	cancel()
	select {
	case err := <-serveDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("ServeBrokeredAsSandbox: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeBrokeredAsSandbox didn't exit on ctx cancel")
	}
}

// waitForSandbox polls the matcher until at least one sandbox is
// subscribed. The node's ServeBrokeredAsSandbox runs RegisterSandbox
// in a goroutine that races the test's DialBrokeredServe.
func waitForSandbox(t *testing.T, r *rig) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, sandboxes := r.server.Matcher().Counts(); sandboxes >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sandbox subscription never landed in the matcher")
}

func TestServeBrokeredAsSandboxRejectsBadOptions(t *testing.T) {
	r := newRig(t)
	if err := r.sb.ServeBrokeredAsSandbox(context.Background(), "",
		brokerclient.ServeOptions{Handler: http.NewServeMux()}); err == nil {
		t.Error("expected error for empty sandbox_id")
	}
	if err := r.sb.ServeBrokeredAsSandbox(context.Background(), "sb",
		brokerclient.ServeOptions{}); err == nil {
		t.Error("expected error for nil handler")
	}
}

func TestDialBrokeredServeRejectsBadOptions(t *testing.T) {
	r := newRig(t)
	if _, err := r.cli.DialBrokeredServe(context.Background(), brokerclient.DialServeOptions{
		NodeFingerprint: r.sbID.Fingerprint(),
	}); err == nil {
		t.Error("expected error for empty workspace")
	}
	if _, err := r.cli.DialBrokeredServe(context.Background(), brokerclient.DialServeOptions{
		Workspace: testWorkspaceName,
	}); err == nil {
		t.Error("expected error for zero node fingerprint")
	}
}

// TestDialBrokeredServeNoSandbox: with no node serving, RequestAttach
// fails with FailedPrecondition (ErrNoSandbox) and DialBrokeredServe
// surfaces it.
func TestDialBrokeredServeNoSandbox(t *testing.T) {
	r := newRig(t)
	_, err := r.cli.DialBrokeredServe(context.Background(), brokerclient.DialServeOptions{
		Workspace:       testWorkspaceName,
		NodeFingerprint: r.sbID.Fingerprint(),
	})
	if err == nil {
		t.Fatal("expected error when no node is serving")
	}
}

// TestDialBrokeredServeCancelledCtx covers the RegisterServe error
// branch: a pre-cancelled context makes the serve registration fail
// before any attach is requested.
func TestDialBrokeredServeCancelledCtx(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.cli.DialBrokeredServe(ctx, brokerclient.DialServeOptions{
		Workspace:       testWorkspaceName,
		NodeFingerprint: r.sbID.Fingerprint(),
	}); err == nil {
		t.Error("expected error for cancelled ctx")
	}
}

// TestServeBrokeredAsSandboxContextCancel exercises the clean
// ctx-cancel exit of the accept loop with no attach ever arriving.
func TestServeBrokeredAsSandboxContextCancel(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- r.sb.ServeBrokeredAsSandbox(ctx, r.sbID.Fingerprint().String(),
			brokerclient.ServeOptions{Handler: http.NewServeMux()})
	}()
	waitForSandbox(t, r)
	cancel()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want nil/Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeBrokeredAsSandbox didn't exit on cancel")
	}
}
