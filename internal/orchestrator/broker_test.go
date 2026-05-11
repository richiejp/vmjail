package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	workspacebrokerv1 "github.com/richiejp/darkaifactory/internal/genpb/workspacebroker/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspacebroker/v1/workspacebrokerv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/workspacebroker"
)

const (
	testWorkspace = "src"
	testSandboxID = "sb-1"
)

// brokerHarness builds a running orchestrator with two approved
// peers (a "client" identity that will register a serve, and a
// "sandbox" identity that will register a sandbox-host). Both
// peers get their own Connect-RPC client to the broker.
type brokerHarness struct {
	server    *Server
	url       string
	cliID     *identity.Identity
	sbID      *identity.Identity
	cliBroker workspacebrokerv1connect.BrokerServiceClient
	sbBroker  workspacebrokerv1connect.BrokerServiceClient
}

func newBrokerHarness(t *testing.T) *brokerHarness {
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

	url := "https://" + srv.Addr()
	return &brokerHarness{
		server:    srv,
		url:       url,
		cliID:     cliID,
		sbID:      sbID,
		cliBroker: brokerClient(t, cliID, srvID, url),
		sbBroker:  brokerClient(t, sbID, srvID, url),
	}
}

func brokerClient(t *testing.T, peer, server *identity.Identity, url string) workspacebrokerv1connect.BrokerServiceClient {
	t.Helper()
	store := mustOpenStore(t)
	if err := store.Add(identity.Peer{Fingerprint: server.Fingerprint()}); err != nil {
		t.Fatalf("peer Add: %v", err)
	}
	tlsCfg, err := identity.ClientTLSConfig(peer, store, "")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	// No http.Client.Timeout: that's a hard deadline on the
	// entire response (including streaming response bodies),
	// which would race the test's wait window for server-streaming
	// subscriptions to register. Per-request cancellation is via
	// context.Context instead.
	httpc := &http.Client{
		Transport: &http2.Transport{TLSClientConfig: tlsCfg},
	}
	return workspacebrokerv1connect.NewBrokerServiceClient(httpc, url)
}

// TestBrokerHappyPath wires a real serve and a real sandbox to
// the broker, fires RequestAttach, and verifies both stream
// peers receive the PendingAttach.
func TestBrokerHappyPath(t *testing.T) {
	h := newBrokerHarness(t)
	serveEvCh, sandEvCh := registerBoth(t, h, testWorkspace, testSandboxID)

	resp, err := h.cliBroker.RequestAttach(context.Background(), connect.NewRequest(
		&workspacebrokerv1.RequestAttachRequest{
			ServeFingerprint: h.cliID.Fingerprint().String(),
			Workspace:        testWorkspace,
			SandboxId:        testSandboxID,
		},
	))
	if err != nil {
		t.Fatalf("RequestAttach: %v", err)
	}
	if resp.Msg.GetSessionId() == "" {
		t.Error("empty session id")
	}
	if !strings.HasPrefix(resp.Msg.GetRelayEndpoint(), "127.0.0.1:") {
		t.Errorf("relay endpoint = %q, want 127.0.0.1:port", resp.Msg.GetRelayEndpoint())
	}

	serveEv := recvChan(t, serveEvCh)
	sandEv := recvChan(t, sandEvCh)
	if serveEv.GetSessionId() != resp.Msg.GetSessionId() {
		t.Errorf("serve session = %q, want %q", serveEv.GetSessionId(), resp.Msg.GetSessionId())
	}
	if sandEv.GetSessionId() != resp.Msg.GetSessionId() {
		t.Errorf("sandbox session = %q, want %q", sandEv.GetSessionId(), resp.Msg.GetSessionId())
	}
	if serveEv.GetPeerFingerprint() != h.sbID.Fingerprint().String() {
		t.Errorf("serve peer_fingerprint = %q, want sandbox %q",
			serveEv.GetPeerFingerprint(), h.sbID.Fingerprint())
	}
	if sandEv.GetPeerFingerprint() != h.cliID.Fingerprint().String() {
		t.Errorf("sandbox peer_fingerprint = %q, want client %q",
			sandEv.GetPeerFingerprint(), h.cliID.Fingerprint())
	}
}

// registerBoth opens a RegisterServe and a RegisterSandbox
// stream and pumps Receive() in background goroutines so the
// HTTP/2 request bodies are actually transmitted and the
// server-side handlers subscribe with the matcher. Returns
// channels carrying the events.
func registerBoth(t *testing.T, h *brokerHarness, workspace, sandboxID string) (serveEvs, sandboxEvs <-chan *workspacebrokerv1.PendingAttach) {
	t.Helper()
	serveEv := pumpServer(t, func(ctx context.Context) (*connect.ServerStreamForClient[workspacebrokerv1.RegistrationEvent], error) {
		return h.cliBroker.RegisterServe(ctx, connect.NewRequest(
			&workspacebrokerv1.RegisterServeRequest{Workspace: workspace},
		))
	})
	sandEv := pumpServer(t, func(ctx context.Context) (*connect.ServerStreamForClient[workspacebrokerv1.RegistrationEvent], error) {
		return h.sbBroker.RegisterSandbox(ctx, connect.NewRequest(
			&workspacebrokerv1.RegisterSandboxRequest{SandboxId: sandboxID},
		))
	})
	// pumpServer already consumed the Ready event for each, so
	// the subscription is guaranteed to be live server-side
	// without polling.
	return serveEv, sandEv
}

// pumpServer opens a server-streaming RPC, spawns a goroutine
// to drain it, and returns a channel of PendingAttach events.
// The initial SubscriptionReady event is consumed before
// returning so callers see only real attaches. The goroutine
// and the stream are torn down via t.Cleanup.
func pumpServer(
	t *testing.T,
	open func(context.Context) (*connect.ServerStreamForClient[workspacebrokerv1.RegistrationEvent], error),
) <-chan *workspacebrokerv1.PendingAttach {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := open(ctx)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	// Receive the initial Ready event synchronously so the
	// caller can know the subscription is in place server-side
	// before doing anything that depends on it.
	if !stream.Receive() {
		cancel()
		t.Fatalf("stream closed before Ready: %v", stream.Err())
	}
	if stream.Msg().GetReady() == nil {
		cancel()
		t.Fatalf("first event not Ready: %v", stream.Msg())
	}
	out := make(chan *workspacebrokerv1.PendingAttach, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for stream.Receive() {
			ev := stream.Msg().GetAttach()
			if ev == nil {
				continue
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = stream.Close()
		<-done
	})
	return out
}

func recvChan(t *testing.T, ch <-chan *workspacebrokerv1.PendingAttach) *workspacebrokerv1.PendingAttach {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for PendingAttach")
		return nil
	}
}

// TestBrokerRejectsAttachAsOtherIdentity: the client may only
// request an attach for a workspace it itself owns. Trying to
// attach using a different peer's fingerprint is PermissionDenied.
func TestBrokerRejectsAttachAsOtherIdentity(t *testing.T) {
	h := newBrokerHarness(t)
	_, err := h.cliBroker.RequestAttach(context.Background(), connect.NewRequest(
		&workspacebrokerv1.RequestAttachRequest{
			ServeFingerprint: h.sbID.Fingerprint().String(), // not ours
			Workspace:        testWorkspace,
			SandboxId:        testSandboxID,
		},
	))
	if err == nil {
		t.Fatal("expected error")
	}
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Errorf("code = %s, want PermissionDenied", got)
	}
}

func TestBrokerRequestAttachWithoutServe(t *testing.T) {
	h := newBrokerHarness(t)
	_ = pumpServer(t, func(ctx context.Context) (*connect.ServerStreamForClient[workspacebrokerv1.RegistrationEvent], error) {
		return h.sbBroker.RegisterSandbox(ctx, connect.NewRequest(
			&workspacebrokerv1.RegisterSandboxRequest{SandboxId: testSandboxID},
		))
	})

	_, err := h.cliBroker.RequestAttach(context.Background(), connect.NewRequest(
		&workspacebrokerv1.RequestAttachRequest{
			ServeFingerprint: h.cliID.Fingerprint().String(),
			Workspace:        "absent",
			SandboxId:        testSandboxID,
		},
	))
	if err == nil {
		t.Fatal("expected error")
	}
	if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want FailedPrecondition", got)
	}
}

func TestBrokerRequestAttachRejectsBadFingerprint(t *testing.T) {
	h := newBrokerHarness(t)
	_, err := h.cliBroker.RequestAttach(context.Background(), connect.NewRequest(
		&workspacebrokerv1.RequestAttachRequest{
			ServeFingerprint: "not-a-fingerprint",
			Workspace:        testWorkspace,
			SandboxId:        testSandboxID,
		},
	))
	if err == nil {
		t.Fatal("expected error")
	}
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", got)
	}
}

func TestBrokerRegisterRejectsBadInput(t *testing.T) {
	h := newBrokerHarness(t)

	serveStream, err := h.cliBroker.RegisterServe(context.Background(), connect.NewRequest(
		&workspacebrokerv1.RegisterServeRequest{Workspace: ""},
	))
	if err != nil {
		t.Fatalf("RegisterServe: %v", err)
	}
	defer func() { _ = serveStream.Close() }()
	// The stream error surfaces on first Recv.
	if serveStream.Receive() {
		t.Errorf("Receive succeeded; want error from empty workspace")
	}
	if got := connect.CodeOf(serveStream.Err()); got != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", got)
	}

	sandStream, err := h.sbBroker.RegisterSandbox(context.Background(), connect.NewRequest(
		&workspacebrokerv1.RegisterSandboxRequest{SandboxId: ""},
	))
	if err != nil {
		t.Fatalf("RegisterSandbox: %v", err)
	}
	defer func() { _ = sandStream.Close() }()
	if sandStream.Receive() {
		t.Errorf("Receive succeeded; want error from empty sandbox_id")
	}
	if got := connect.CodeOf(sandStream.Err()); got != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", got)
	}
}

// TestBrokerHandlerRejectsMissingPeerIdentity drives the handler
// methods with a Connect request that lacks the
// X-Daft-Peer-Fingerprint header (i.e. no mTLS identity surfaced)
// and verifies each one returns Unauthenticated. Calling the
// handler directly bypasses the mTLS gate, which is the only
// way to reach this branch under test.
func TestBrokerHandlerRejectsMissingPeerIdentity(t *testing.T) {
	bh, err := NewBrokerHandler(workspacebroker.New(), func() string { return "x" })
	if err != nil {
		t.Fatalf("NewBrokerHandler: %v", err)
	}

	req := connect.NewRequest(&workspacebrokerv1.RegisterServeRequest{Workspace: "x"})
	if err := bh.RegisterServe(context.Background(), req, nil); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("RegisterServe: code = %s, want Unauthenticated", connect.CodeOf(err))
	}
	req2 := connect.NewRequest(&workspacebrokerv1.RegisterSandboxRequest{SandboxId: "x"})
	if err := bh.RegisterSandbox(context.Background(), req2, nil); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("RegisterSandbox: code = %s, want Unauthenticated", connect.CodeOf(err))
	}
	req3 := connect.NewRequest(&workspacebrokerv1.RequestAttachRequest{})
	if _, err := bh.RequestAttach(context.Background(), req3); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("RequestAttach: code = %s, want Unauthenticated", connect.CodeOf(err))
	}
}

// TestBrokerHandlerRequestAttachEmptyFields covers the
// workspace=="" || sandbox_id=="" branch.
func TestBrokerHandlerRequestAttachEmptyFields(t *testing.T) {
	bh, err := NewBrokerHandler(workspacebroker.New(), func() string { return "x" })
	if err != nil {
		t.Fatalf("NewBrokerHandler: %v", err)
	}
	id := mustGenerate(t, identity.RoleClient)
	req := connect.NewRequest(&workspacebrokerv1.RequestAttachRequest{
		ServeFingerprint: id.Fingerprint().String(),
		Workspace:        "",
		SandboxId:        "sb",
	})
	req.Header().Set(peerFingerprintHeader, id.Fingerprint().String())
	if _, err := bh.RequestAttach(context.Background(), req); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", connect.CodeOf(err))
	}
}

// TestServerExposesMatcher verifies the orchestrator's Server
// hands the same matcher to the handler and to public callers.
func TestServerExposesMatcher(t *testing.T) {
	h := newBrokerHarness(t)
	if h.server.Matcher() == nil {
		t.Fatal("Matcher() returned nil")
	}
	// Subscribe directly via the matcher and confirm Counts
	// reflects it.
	alice := mustGenerate(t, identity.RoleClient).Fingerprint()
	cancel, _ := h.server.Matcher().SubscribeServe(
		workspacebroker.ServeKey{Identity: alice, Workspace: "direct"},
		make(chan *workspacebroker.PendingAttach, 1),
	)
	defer cancel()
	if s, _ := h.server.Matcher().Counts(); s != 1 {
		t.Errorf("serves = %d, want 1", s)
	}
}

// TestNewBrokerHandlerRejectsNilArgs covers the constructor's
// input guards.
func TestNewBrokerHandlerRejectsNilArgs(t *testing.T) {
	if _, err := NewBrokerHandler(nil, func() string { return "x" }); err == nil {
		t.Error("expected error for nil matcher")
	}
	if _, err := NewBrokerHandler(workspacebroker.New(), nil); err == nil {
		t.Error("expected error for nil relay endpoint resolver")
	}
}

// TestPeerFingerprintHeaderHelper exercises the header parser
// directly: empty header → false; malformed → false; valid → true.
func TestPeerFingerprintHeaderHelper(t *testing.T) {
	emptyHdr := http.Header{}
	if _, ok := PeerFingerprintFromRequestHeader(emptyHdr); ok {
		t.Error("expected false for empty header")
	}
	badHdr := http.Header{peerFingerprintHeader: []string{"not-a-fingerprint"}}
	if _, ok := PeerFingerprintFromRequestHeader(badHdr); ok {
		t.Error("expected false for malformed fingerprint")
	}
	id := mustGenerate(t, identity.RoleClient)
	goodHdr := http.Header{peerFingerprintHeader: []string{id.Fingerprint().String()}}
	fp, ok := PeerFingerprintFromRequestHeader(goodHdr)
	if !ok || fp != id.Fingerprint() {
		t.Errorf("good header: ok=%v fp=%s want %s", ok, fp, id.Fingerprint())
	}
}

// TestMapMatcherErr pins the mapping from internal matcher errors
// to Connect status codes so RPC clients can react sensibly.
func TestMapMatcherErr(t *testing.T) {
	cases := []struct {
		in   error
		want connect.Code
	}{
		{workspacebroker.ErrNoServe, connect.CodeFailedPrecondition},
		{workspacebroker.ErrNoSandbox, connect.CodeFailedPrecondition},
		{workspacebroker.ErrSubscriberGone, connect.CodeUnavailable},
		{errors.New("something else"), connect.CodeInternal},
	}
	for _, tc := range cases {
		got := connect.CodeOf(mapMatcherErr(tc.in))
		if got != tc.want {
			t.Errorf("mapMatcherErr(%v) → %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestBrokerRelayUnimplemented pins the 3.5a stub behaviour —
// when 3.5b lands this test will need updating.
func TestBrokerRelayUnimplemented(t *testing.T) {
	h := newBrokerHarness(t)
	stream := h.cliBroker.Relay(context.Background())
	if err := stream.Send(&workspacebrokerv1.RelayMessage{
		Body: &workspacebrokerv1.RelayMessage_Hello{
			Hello: &workspacebrokerv1.RelayHello{SessionId: "x"},
		},
	}); err != nil {
		t.Logf("Send err (expected, may differ by transport): %v", err)
	}
	if _, err := stream.Receive(); err == nil {
		t.Error("expected error from Relay stub")
	} else if got := connect.CodeOf(err); got != connect.CodeUnimplemented {
		t.Errorf("code = %s, want Unimplemented", got)
	}
	_ = stream.CloseRequest()
	_ = stream.CloseResponse()
}
