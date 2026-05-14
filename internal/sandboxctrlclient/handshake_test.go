package sandboxctrlclient_test

import (
	"context"
	"crypto/tls"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	sandboxctrlv1 "github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1/sandboxctrlv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/sandboxctrl"
	"github.com/richiejp/darkaifactory/internal/sandboxctrlclient"
)

// badVersionHandler is a stub SandboxControlService that always
// replies to RegisterSandbox with a SubscriptionReady carrying a
// deliberately-incompatible ProtocolVersion. Used to verify the
// client rejects a wire-incompatible orchestrator before any
// Dispatch can flow.
type badVersionHandler struct {
	sandboxctrlv1connect.UnimplementedSandboxControlServiceHandler
}

func (badVersionHandler) RegisterSandbox(
	_ context.Context,
	_ *connect.Request[sandboxctrlv1.RegisterSandboxRequest],
	stream *connect.ServerStream[sandboxctrlv1.RegisterEvent],
) error {
	return stream.Send(&sandboxctrlv1.RegisterEvent{
		Event: &sandboxctrlv1.RegisterEvent_Ready{
			Ready: &sandboxctrlv1.SubscriptionReady{
				AcceptedAt: timestamppb.Now(),
				ProtocolVersion: &sandboxctrlv1.ProtocolVersion{
					Major: sandboxctrl.ProtocolVersionMajor + 99,
				},
			},
		},
	})
}

// TestRegisterAndServeRejectsIncompatibleServerProtocol pins the
// client-side half of the version handshake: an orchestrator that
// echoes back a wire-incompatible ProtocolVersion must cause
// RegisterAndServe to surface the mismatch and tear down the
// registration before any Dispatch is accepted.
func TestRegisterAndServeRejectsIncompatibleServerProtocol(t *testing.T) {
	id, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate sb: %v", err)
	}
	srvID, err := identity.Generate(identity.RoleOrchestrator)
	if err != nil {
		t.Fatalf("Generate srv: %v", err)
	}
	srvPeers, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "srv-peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore srv: %v", err)
	}
	if err := srvPeers.Add(identity.Peer{Fingerprint: id.Fingerprint()}); err != nil {
		t.Fatalf("srv approve sb: %v", err)
	}
	srvTLS, err := identity.ServerTLSConfig(srvID, srvPeers)
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	// Mirror mtls.Server's ALPN so HTTP/2 negotiates correctly.
	srvTLS.NextProtos = []string{"h2", "http/1.1"}

	mux := http.NewServeMux()
	path, hh := sandboxctrlv1connect.NewSandboxControlServiceHandler(badVersionHandler{})
	mux.Handle(path, hh)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvTLS)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	srv := &http.Server{
		Handler:           mux,
		TLSConfig:         srvTLS,
		ReadHeaderTimeout: 2 * time.Second,
	}
	var serveWg sync.WaitGroup
	serveWg.Add(1)
	go func() {
		defer serveWg.Done()
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		serveWg.Wait()
	})

	sbPeers, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "sb-peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore sb: %v", err)
	}
	if err := sbPeers.Add(identity.Peer{Fingerprint: srvID.Fingerprint()}); err != nil {
		t.Fatalf("sb approve srv: %v", err)
	}

	client, err := sandboxctrlclient.New(sandboxctrlclient.Config{
		URL:      "https://" + ln.Addr().String(),
		Identity: id,
		Peers:    sbPeers,
		Version:  testFixtureVer,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = client.RegisterAndServe(ctx, sandboxctrl.Resources{})
	if err == nil {
		t.Fatal("RegisterAndServe accepted an incompatible orchestrator")
	}
	if !strings.Contains(err.Error(), "incompatible protocol major") {
		t.Errorf("err = %v, want 'incompatible protocol major' substring", err)
	}
}
