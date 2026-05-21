package nodectrlclient_test

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

	nodectrlv1 "github.com/localai-org/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/nodectrl/v1/nodectrlv1connect"
	"github.com/localai-org/darkaifactory/internal/identity"
	"github.com/localai-org/darkaifactory/internal/nodectrl"
	"github.com/localai-org/darkaifactory/internal/nodectrlclient"
)

// badVersionHandler is a stub NodeControlService that always
// replies to RegisterNode with a SubscriptionReady carrying a
// deliberately-incompatible ProtocolVersion. Used to verify the
// client rejects a wire-incompatible orchestrator before any
// Dispatch can flow.
type badVersionHandler struct {
	nodectrlv1connect.UnimplementedNodeControlServiceHandler
}

func (badVersionHandler) RegisterNode(
	_ context.Context,
	_ *connect.Request[nodectrlv1.RegisterNodeRequest],
	stream *connect.ServerStream[nodectrlv1.NodeEvent],
) error {
	return stream.Send(&nodectrlv1.NodeEvent{
		Event: &nodectrlv1.NodeEvent_Ready{
			Ready: &nodectrlv1.SubscriptionReady{
				AcceptedAt: timestamppb.Now(),
				ProtocolVersion: &nodectrlv1.ProtocolVersion{
					Major: nodectrl.ProtocolVersionMajor + 99,
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
	path, hh := nodectrlv1connect.NewNodeControlServiceHandler(badVersionHandler{})
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

	client, err := nodectrlclient.New(nodectrlclient.Config{
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
	err = client.RegisterAndServe(ctx, nodectrl.Resources{})
	if err == nil {
		t.Fatal("RegisterAndServe accepted an incompatible orchestrator")
	}
	if !strings.Contains(err.Error(), "incompatible protocol major") {
		t.Errorf("err = %v, want 'incompatible protocol major' substring", err)
	}
}
