package brokerclient_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/localai-org/darkaifactory/internal/brokerclient"
	healthv1 "github.com/localai-org/darkaifactory/internal/genpb/health/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/health/v1/healthv1connect"
	"github.com/localai-org/darkaifactory/internal/identity"
)

// healthMessage implements healthv1connect.HealthServiceHandler with
// a deterministic echo, so the brokered inner http2 path has a
// real Connect-RPC to exercise. Health is the smallest stable RPC
// surface; the actual WorkspaceService wiring lands when the
// daft/daft-sandboxd CLIs migrate.
const testWorkspaceName = "src"

type healthMessage struct {
	healthv1connect.UnimplementedHealthServiceHandler
}

func (healthMessage) Ping(_ context.Context, req *connect.Request[healthv1.PingRequest]) (*connect.Response[healthv1.PingResponse], error) {
	return connect.NewResponse(&healthv1.PingResponse{Message: req.Msg.GetMessage()}), nil
}

// TestServeBrokeredAndAttachRoundTrip wires the user-side
// ServeBrokered against the sandbox-side AttachAndDial through a
// real orchestrator. A Connect Health.Ping over inner mTLS
// confirms request/response round-tripping end-to-end.
func TestServeBrokeredAndAttachRoundTrip(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// User-side serve: a mux with HealthService handler, running
	// inside ServeBrokered's per-attach http2.Server.
	mux := http.NewServeMux()
	healthPath, healthHandler := healthv1connect.NewHealthServiceHandler(healthMessage{})
	mux.Handle(healthPath, healthHandler)

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- r.cli.ServeBrokered(ctx, brokerclient.ServeOptions{
			Workspace: testWorkspaceName,
			Handler:   mux,
		})
	}()

	// Sandbox-side attach: register, then issue the cross-identity
	// RequestAttach via the user-host so the broker pairs us.
	attached := make(chan *brokerclient.InnerClient, 1)
	attachErr := make(chan error, 1)
	go func() {
		ic, err := r.sb.AttachAndDial(ctx, brokerclient.AttachOptions{
			Workspace:        testWorkspaceName,
			SandboxID:        "sb-1",
			ServeFingerprint: r.cliID.Fingerprint(),
		})
		if err != nil {
			attachErr <- err
			return
		}
		attached <- ic
	}()

	// Give both sides time to subscribe before RequestAttach so
	// the broker has them both registered.
	waitForRegistration(t, r)
	if _, _, err := r.cli.Attach(ctx, r.cliID.Fingerprint(), testWorkspaceName, "sb-1"); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	var ic *brokerclient.InnerClient
	select {
	case ic = <-attached:
	case err := <-attachErr:
		t.Fatalf("AttachAndDial: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("AttachAndDial didn't return")
	}
	defer func() { _ = ic.Close() }()

	hc := healthv1connect.NewHealthServiceClient(ic.HTTPClient, ic.BaseURL)
	resp, err := hc.Ping(ctx, connect.NewRequest(&healthv1.PingRequest{Message: "roundtrip"}))
	if err != nil {
		t.Fatalf("inner Ping: %v", err)
	}
	if resp.Msg.GetMessage() != "roundtrip" {
		t.Errorf("echo = %q, want roundtrip", resp.Msg.GetMessage())
	}

	cancel()
	select {
	case err := <-serveDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("ServeBrokered: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeBrokered didn't exit on ctx cancel")
	}
}

// waitForRegistration polls the matcher until both a serve and a
// sandbox are subscribed. Each Registration returns once its
// initial Ready event has been consumed, but the orchestrator's
// SubscribeServe / SubscribeSandbox runs in a handler goroutine
// that races the Ready send — this closes the gap.
func waitForRegistration(t *testing.T, r *rig) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		serves, sandboxes := r.server.Matcher().Counts()
		if serves >= 1 && sandboxes >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("subscriptions never landed in the matcher")
}

func TestServeBrokeredRejectsBadOptions(t *testing.T) {
	r := newRig(t)
	err := r.cli.ServeBrokered(context.Background(), brokerclient.ServeOptions{Handler: http.NewServeMux()})
	if err == nil {
		t.Error("expected error for empty workspace")
	}
	err = r.cli.ServeBrokered(context.Background(), brokerclient.ServeOptions{Workspace: "x"})
	if err == nil {
		t.Error("expected error for nil handler")
	}
}

func TestAttachAndDialRejectsBadOptions(t *testing.T) {
	r := newRig(t)
	cases := []brokerclient.AttachOptions{
		{SandboxID: "sb", ServeFingerprint: r.cliID.Fingerprint()},
		{Workspace: testWorkspaceName, ServeFingerprint: r.cliID.Fingerprint()},
		{Workspace: testWorkspaceName, SandboxID: "sb"}, // zero fingerprint
	}
	for i, opts := range cases {
		if _, err := r.sb.AttachAndDial(context.Background(), opts); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

// TestAttachAndDialContextCancelDuringWait exercises the ctx-done
// branch of the events-channel select.
func TestAttachAndDialContextCancelDuringWait(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := r.sb.AttachAndDial(ctx, brokerclient.AttachOptions{
			Workspace:        testWorkspaceName,
			SandboxID:        "sb-cancel",
			ServeFingerprint: r.cliID.Fingerprint(),
		})
		errCh <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AttachAndDial didn't return on cancel")
	}
}

// TestAttachAndDialPeerMismatch: the orchestrator says the
// brokered peer is X, but the caller expected Y → reject.
func TestAttachAndDialPeerMismatch(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Stand up a no-op serve so the broker has something to pair.
	mux := http.NewServeMux()
	_, h := healthv1connect.NewHealthServiceHandler(healthMessage{})
	mux.Handle("/", h)
	go func() {
		_ = r.cli.ServeBrokered(ctx, brokerclient.ServeOptions{
			Workspace: testWorkspaceName, Handler: mux,
		})
	}()

	errCh := make(chan error, 1)
	go func() {
		// Wrong expected fingerprint.
		bogus := mustGenerate(t, identity.RoleClient).Fingerprint()
		_, err := r.sb.AttachAndDial(ctx, brokerclient.AttachOptions{
			Workspace:        testWorkspaceName,
			SandboxID:        "sb-mismatch",
			ServeFingerprint: bogus,
		})
		errCh <- err
	}()

	waitForRegistration(t, r)
	if _, _, err := r.cli.Attach(ctx, r.cliID.Fingerprint(), testWorkspaceName, "sb-mismatch"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected mismatch error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("AttachAndDial didn't reject mismatch")
	}
}

// TestAttachAndDialClosesRelayThenRegistration checks that
// InnerClient.Close tears down both the inner TLS conn and the
// outer registration, and that calling it twice is a no-op for
// the underlying close func.
func TestAttachAndDialClosesRelayThenRegistration(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mux := http.NewServeMux()
	_, h := healthv1connect.NewHealthServiceHandler(healthMessage{})
	mux.Handle("/", h)
	go func() {
		_ = r.cli.ServeBrokered(ctx, brokerclient.ServeOptions{
			Workspace: testWorkspaceName, Handler: mux,
		})
	}()
	attached := make(chan *brokerclient.InnerClient, 1)
	go func() {
		ic, err := r.sb.AttachAndDial(ctx, brokerclient.AttachOptions{
			Workspace: testWorkspaceName, SandboxID: "sb-close",
			ServeFingerprint: r.cliID.Fingerprint(),
		})
		if err == nil {
			attached <- ic
		}
	}()
	waitForRegistration(t, r)
	_, _, _ = r.cli.Attach(ctx, r.cliID.Fingerprint(), testWorkspaceName, "sb-close")

	var ic *brokerclient.InnerClient
	select {
	case ic = <-attached:
	case <-time.After(3 * time.Second):
		t.Fatal("AttachAndDial didn't return")
	}
	if err := ic.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := ic.Close(); err != nil {
		t.Errorf("second Close (should be idempotent): %v", err)
	}
}
