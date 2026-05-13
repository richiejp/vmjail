package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	sandboxctrlv1 "github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1/sandboxctrlv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/sandboxctrl"
)

// TestNewSandboxControlHandlerRejectsNilRegistry covers the
// constructor's input guard.
func TestNewSandboxControlHandlerRejectsNilRegistry(t *testing.T) {
	if _, err := NewSandboxControlHandler(nil); err == nil {
		t.Error("expected error for nil registry")
	}
}

// TestSandboxControlHandlerRejectsMissingPeerIdentity drives the
// handler methods with a Connect request that lacks the
// X-Daft-Peer-Fingerprint header and verifies each one returns
// Unauthenticated. Mirrors TestBrokerHandlerRejectsMissingPeerIdentity.
func TestSandboxControlHandlerRejectsMissingPeerIdentity(t *testing.T) {
	h, err := NewSandboxControlHandler(sandboxctrl.NewRegistry(nil))
	if err != nil {
		t.Fatalf("NewSandboxControlHandler: %v", err)
	}
	req := connect.NewRequest(&sandboxctrlv1.RegisterSandboxRequest{Version: "v0"})
	if err := h.RegisterSandbox(t.Context(), req, nil); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("RegisterSandbox: code = %s, want Unauthenticated", connect.CodeOf(err))
	}
	req2 := connect.NewRequest(&sandboxctrlv1.HeartbeatRequest{})
	if _, err := h.Heartbeat(t.Context(), req2); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("Heartbeat: code = %s, want Unauthenticated", connect.CodeOf(err))
	}
}

// TestHeartbeatUnknownSandboxMapsToFailedPrecondition pins the
// error mapping so a client can tell "I'm not registered" apart
// from a transport problem.
func TestHeartbeatUnknownSandboxMapsToFailedPrecondition(t *testing.T) {
	id := mustGenerate(t, identity.RoleSandboxHost)
	h, err := NewSandboxControlHandler(sandboxctrl.NewRegistry(nil))
	if err != nil {
		t.Fatalf("NewSandboxControlHandler: %v", err)
	}
	req := connect.NewRequest(&sandboxctrlv1.HeartbeatRequest{})
	req.Header().Set(peerFingerprintHeader, id.Fingerprint().String())
	_, err = h.Heartbeat(t.Context(), req)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want FailedPrecondition", connect.CodeOf(err))
	}
}

// TestResourcesFromProtoNilHandling covers the nil-proto branch.
func TestResourcesFromProtoNilHandling(t *testing.T) {
	if got := resourcesFromProto(nil); got != (sandboxctrl.Resources{}) {
		t.Errorf("nil → %+v, want zero", got)
	}
	got := resourcesFromProto(&sandboxctrlv1.HostResources{Cpus: 4, MemoryBytes: 1024})
	want := sandboxctrl.Resources{CPUs: 4, MemoryBytes: 1024}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestServerExposesSandboxRegistry verifies Config.SandboxRegistry
// flows through and is exposed via Server.SandboxRegistry().
func TestServerExposesSandboxRegistry(t *testing.T) {
	srvID := mustGenerate(t, identity.RoleOrchestrator)
	store := mustOpenStoreForSandboxCtrl(t)
	reg := sandboxctrl.NewRegistry(nil)
	srv, err := New(Config{
		Identity:        srvID,
		Peers:           store,
		SandboxRegistry: reg,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv.SandboxRegistry() != reg {
		t.Error("SandboxRegistry() did not return the configured registry")
	}
}

// TestServerCreatesDefaultSandboxRegistry verifies New mints a
// default registry when Config.SandboxRegistry is nil.
func TestServerCreatesDefaultSandboxRegistry(t *testing.T) {
	srvID := mustGenerate(t, identity.RoleOrchestrator)
	store := mustOpenStoreForSandboxCtrl(t)
	srv, err := New(Config{Identity: srvID, Peers: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv.SandboxRegistry() == nil {
		t.Error("SandboxRegistry() = nil, want default")
	}
}

func mustOpenStoreForSandboxCtrl(t *testing.T) *identity.PeerStore {
	t.Helper()
	s, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	return s
}

// TestRegisterSandboxRecordsEntryAndForgetsOnDisconnect exercises
// the RegisterSandbox happy path end-to-end against a real Connect
// server (so the server-stream + ctx-cancel lifecycle is real).
// Coverage-wise this is what lifts orchestrator/sandboxctrl.go
// above the per-file gate.
func TestRegisterSandboxRecordsEntryAndForgetsOnDisconnect(t *testing.T) {
	rig := newSandboxCtrlRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stream, err := rig.client.RegisterSandbox(ctx, connect.NewRequest(
		&sandboxctrlv1.RegisterSandboxRequest{
			Version:   "v0.0.0",
			Resources: &sandboxctrlv1.HostResources{Cpus: 2, MemoryBytes: 1 << 30},
		},
	))
	if err != nil {
		t.Fatalf("RegisterSandbox: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	if !stream.Receive() {
		t.Fatalf("stream closed before Ready: %v", stream.Err())
	}
	if stream.Msg().GetReady() == nil {
		t.Fatal("first event was not SubscriptionReady")
	}

	entry, ok := rig.reg.Lookup(rig.sbID.Fingerprint())
	if !ok {
		t.Fatal("registry has no entry post-Ready")
	}
	if entry.Resources.CPUs != 2 {
		t.Errorf("Resources.CPUs = %d, want 2", entry.Resources.CPUs)
	}

	// Heartbeat over the same connection.
	hbResp, err := rig.client.Heartbeat(ctx, connect.NewRequest(
		&sandboxctrlv1.HeartbeatRequest{Current: &sandboxctrlv1.HostResources{Cpus: 2, MemoryBytes: 512 << 20}},
	))
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !hbResp.Msg.GetSeenAt().IsValid() {
		t.Error("Heartbeat returned invalid seen_at")
	}

	// Close stream → handler returns → defer Forget releases entry.
	_ = stream.Close()
	deadline := time.Now().Add(2 * time.Second)
	//daft:lint:disable=forloopbound reason="exits when the registry releases the entry or on 2s wall-clock timeout"
	for time.Now().Before(deadline) {
		if _, ok := rig.reg.Lookup(rig.sbID.Fingerprint()); !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("registry entry not released after stream close")
}

// sandboxCtrlRig wires an in-process orchestrator + a Connect
// client for the SandboxControlService. Kept local to this test
// file — exporting it would couple orchestrator's test surface to
// internal layering decisions that are still in flux for Slice A.
type sandboxCtrlRig struct {
	server *Server
	reg    *sandboxctrl.Registry
	sbID   *identity.Identity
	client sandboxctrlv1connect.SandboxControlServiceClient
}

func newSandboxCtrlRig(t *testing.T) *sandboxCtrlRig {
	t.Helper()
	srvID := mustGenerate(t, identity.RoleOrchestrator)
	sbID := mustGenerate(t, identity.RoleSandboxHost)

	srvPeers, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	if err := srvPeers.Add(identity.Peer{Fingerprint: sbID.Fingerprint()}); err != nil {
		t.Fatalf("Add peer: %v", err)
	}
	reg := sandboxctrl.NewRegistry(nil)

	srv, err := New(Config{
		Identity:        srvID,
		Peers:           srvPeers,
		Addr:            "127.0.0.1:0",
		SandboxRegistry: reg,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	var srvErrOnce sync.Once
	srvErr := make(chan error, 1)
	go func() {
		err := srv.Serve()
		srvErrOnce.Do(func() { srvErr <- err })
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case e := <-srvErr:
			if e != nil && !errors.Is(e, http.ErrServerClosed) {
				t.Logf("Serve: %v", e)
			}
		case <-time.After(2 * time.Second):
		}
	})

	sbPeers, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "sb-peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore sb: %v", err)
	}
	if err := sbPeers.Add(identity.Peer{Fingerprint: srvID.Fingerprint()}); err != nil {
		t.Fatalf("sb approve srv: %v", err)
	}
	tlsCfg, err := identity.ClientTLSConfig(sbID, sbPeers, "")
	if err != nil {
		t.Fatalf("ClientTLSConfig: %v", err)
	}
	httpc := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}}
	url := "https://" + srv.Addr()
	client := sandboxctrlv1connect.NewSandboxControlServiceClient(httpc, url)
	return &sandboxCtrlRig{server: srv, reg: reg, sbID: sbID, client: client}
}
