package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1/nodectrlv1connect"
	sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
)

// TestNewNodeControlHandlerRejectsNilRegistry covers the
// constructor's input guard.
func TestNewNodeControlHandlerRejectsNilRegistry(t *testing.T) {
	if _, err := NewNodeControlHandler(nil); err == nil {
		t.Error("expected error for nil registry")
	}
}

func TestDuplicateRegistrationCleanupPreservesNewerStream(t *testing.T) {
	reg := nodectrl.NewRegistry(nil)
	h, err := NewNodeControlHandler(reg)
	if err != nil {
		t.Fatalf("NewNodeControlHandler: %v", err)
	}
	id, _ := identity.Generate(identity.RoleSandboxHost)
	fp := id.Fingerprint()
	old := h.attachRegistration(fp)
	newer := h.attachRegistration(fp)
	select {
	case <-old.revoked:
	default:
		t.Fatal("replacement registration did not terminate the old stream")
	}
	h.detachRegistration(fp, old)
	h.mu.Lock()
	got := h.streams[fp]
	h.mu.Unlock()
	if got != newer {
		t.Fatal("old registration cleanup removed the newer stream")
	}
	h.detachRegistration(fp, newer)
}

func TestRevokeRegistrationSignalsStreamAndForgetsNode(t *testing.T) {
	reg := nodectrl.NewRegistry(nil)
	h, _ := NewNodeControlHandler(reg)
	id, _ := identity.Generate(identity.RoleSandboxHost)
	fp := id.Fingerprint()
	registration := h.attachRegistration(fp)
	reg.Register(fp, "node", "v1", nodectrl.Resources{})
	h.RevokeRegistration(fp)
	select {
	case <-registration.revoked:
	default:
		t.Fatal("revocation did not signal registration")
	}
	if _, ok := reg.Lookup(fp); ok {
		t.Fatal("revocation did not forget registry entry")
	}
}

// TestNodeControlHandlerRejectsMissingPeerIdentity drives the
// handler methods with a Connect request that lacks the
// X-Daft-Peer-Fingerprint header and verifies each one returns
// Unauthenticated. Mirrors TestBrokerHandlerRejectsMissingPeerIdentity.
func TestNodeControlHandlerRejectsMissingPeerIdentity(t *testing.T) {
	h, err := NewNodeControlHandler(nodectrl.NewRegistry(nil))
	if err != nil {
		t.Fatalf("NewNodeControlHandler: %v", err)
	}
	req := connect.NewRequest(&nodectrlv1.RegisterNodeRequest{Version: "v0"})
	if err := h.RegisterNode(t.Context(), req, nil); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("RegisterNode: code = %s, want Unauthenticated", connect.CodeOf(err))
	}
	req2 := connect.NewRequest(&nodectrlv1.HeartbeatRequest{})
	if _, err := h.Heartbeat(t.Context(), req2); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("Heartbeat: code = %s, want Unauthenticated", connect.CodeOf(err))
	}
	req3 := connect.NewRequest(&nodectrlv1.ListNodesRequest{})
	if _, err := h.ListNodes(t.Context(), req3); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("ListNodes: code = %s, want Unauthenticated", connect.CodeOf(err))
	}
}

// TestListNodesReflectsRegistry pins the projection from the
// registry snapshot onto the wire Node shape: every
// registered entry shows up exactly once, sorted by fingerprint, with
// version + resources + timestamps preserved.
func TestListNodesReflectsRegistry(t *testing.T) {
	reg := nodectrl.NewRegistry(nil)
	h, err := NewNodeControlHandler(reg)
	if err != nil {
		t.Fatalf("NewNodeControlHandler: %v", err)
	}
	caller := mustGenerate(t, identity.RoleOrchestrator)
	sandboxA := mustGenerate(t, identity.RoleSandboxHost)
	sandboxB := mustGenerate(t, identity.RoleSandboxHost)
	reg.Register(sandboxA.Fingerprint(), "host-a", "v1.0.0", nodectrl.Resources{CPUs: 2, MemoryBytes: 1 << 30})
	reg.Register(sandboxB.Fingerprint(), "host-b", "v1.0.1", nodectrl.Resources{CPUs: 4, MemoryBytes: 4 << 30})

	req := connect.NewRequest(&nodectrlv1.ListNodesRequest{})
	req.Header().Set(peerFingerprintHeader, caller.Fingerprint().String())
	resp, err := h.ListNodes(t.Context(), req)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	got := resp.Msg.GetNodes()
	if len(got) != 2 {
		t.Fatalf("len(sandboxes) = %d, want 2", len(got))
	}
	// Snapshot is sorted by fingerprint string; build the same
	// expectation independently so we test against the contract,
	// not the implementation.
	wantFirst, wantSecond := sandboxA.Fingerprint().String(), sandboxB.Fingerprint().String()
	if wantFirst > wantSecond {
		wantFirst, wantSecond = wantSecond, wantFirst
	}
	if got[0].GetFingerprint() != wantFirst || got[1].GetFingerprint() != wantSecond {
		t.Errorf("fingerprints = [%s, %s], want [%s, %s]",
			got[0].GetFingerprint(), got[1].GetFingerprint(), wantFirst, wantSecond)
	}
	// Spot-check the entry that maps to sandboxA.
	var entryA *nodectrlv1.Node
	for _, e := range got {
		if e.GetFingerprint() == sandboxA.Fingerprint().String() {
			entryA = e
		}
	}
	if entryA == nil {
		t.Fatal("sandboxA missing from response")
	}
	if entryA.GetVersion() != "v1.0.0" {
		t.Errorf("version = %q, want v1.0.0", entryA.GetVersion())
	}
	if entryA.GetHostname() != "host-a" {
		t.Errorf("hostname = %q, want host-a", entryA.GetHostname())
	}
	if entryA.GetResources().GetCpus() != 2 || entryA.GetResources().GetMemoryBytes() != 1<<30 {
		t.Errorf("resources = %+v, want {2, 1GiB}", entryA.GetResources())
	}
	if !entryA.GetRegisteredAt().IsValid() || !entryA.GetLastSeenAt().IsValid() {
		t.Error("timestamps not populated")
	}
}

// TestListNodesEmpty exercises the zero-entry path explicitly.
// Skipping it would leave the empty Registry projection uncovered
// when the test above populates the registry first.
func TestListNodesEmpty(t *testing.T) {
	h, err := NewNodeControlHandler(nodectrl.NewRegistry(nil))
	if err != nil {
		t.Fatalf("NewNodeControlHandler: %v", err)
	}
	caller := mustGenerate(t, identity.RoleOrchestrator)
	req := connect.NewRequest(&nodectrlv1.ListNodesRequest{})
	req.Header().Set(peerFingerprintHeader, caller.Fingerprint().String())
	resp, err := h.ListNodes(t.Context(), req)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(resp.Msg.GetNodes()) != 0 {
		t.Errorf("len(sandboxes) = %d, want 0", len(resp.Msg.GetNodes()))
	}
}

// TestHeartbeatUnknownNodeMapsToFailedPrecondition pins the
// error mapping so a client can tell "I'm not registered" apart
// from a transport problem.
func TestHeartbeatUnknownNodeMapsToFailedPrecondition(t *testing.T) {
	id := mustGenerate(t, identity.RoleSandboxHost)
	h, err := NewNodeControlHandler(nodectrl.NewRegistry(nil))
	if err != nil {
		t.Fatalf("NewNodeControlHandler: %v", err)
	}
	req := connect.NewRequest(&nodectrlv1.HeartbeatRequest{})
	req.Header().Set(peerFingerprintHeader, id.Fingerprint().String())
	_, err = h.Heartbeat(t.Context(), req)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %s, want FailedPrecondition", connect.CodeOf(err))
	}
}

func TestHeartbeatRejectsNonNodeRole(t *testing.T) {
	peers := identity.NewMemoryPeerStore()
	id := mustGenerate(t, identity.RoleClient)
	if err := peers.Add(identity.Peer{Fingerprint: id.Fingerprint(), Role: identity.RoleClient}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	h, _ := NewNodeControlHandler(nodectrl.NewRegistry(nil), peers)
	req := connect.NewRequest(&nodectrlv1.HeartbeatRequest{})
	req.Header().Set(peerFingerprintHeader, id.Fingerprint().String())
	if _, err := h.Heartbeat(t.Context(), req); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("Heartbeat code = %v, want permission denied", connect.CodeOf(err))
	}
}

func TestHeartbeatRejectsRemovedPeer(t *testing.T) {
	peers := identity.NewMemoryPeerStore()
	id := mustGenerate(t, identity.RoleSandboxHost)
	h, _ := NewNodeControlHandler(nodectrl.NewRegistry(nil), peers)
	req := connect.NewRequest(&nodectrlv1.HeartbeatRequest{})
	req.Header().Set(peerFingerprintHeader, id.Fingerprint().String())
	if _, err := h.Heartbeat(t.Context(), req); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("Heartbeat code = %v, want unauthenticated", connect.CodeOf(err))
	}
}

// TestResourcesFromProtoNilHandling covers the nil-proto branch.
func TestResourcesFromProtoNilHandling(t *testing.T) {
	if got := resourcesFromProto(nil); got != (nodectrl.Resources{}) {
		t.Errorf("nil → %+v, want zero", got)
	}
	got := resourcesFromProto(&nodectrlv1.HostResources{Cpus: 4, MemoryBytes: 1024})
	want := nodectrl.Resources{CPUs: 4, MemoryBytes: 1024}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestServerExposesNodeRegistry verifies Config.NodeRegistry
// flows through and is exposed via Server.NodeRegistry().
func TestServerExposesNodeRegistry(t *testing.T) {
	srvID := mustGenerate(t, identity.RoleOrchestrator)
	store := mustOpenStoreForSandboxCtrl(t)
	reg := nodectrl.NewRegistry(nil)
	srv, err := New(Config{
		Identity:     srvID,
		Peers:        store,
		NodeRegistry: reg,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv.NodeRegistry() != reg {
		t.Error("NodeRegistry() did not return the configured registry")
	}
}

// TestServerCreatesDefaultNodeRegistry verifies New mints a
// default registry when Config.NodeRegistry is nil.
func TestServerCreatesDefaultNodeRegistry(t *testing.T) {
	srvID := mustGenerate(t, identity.RoleOrchestrator)
	store := mustOpenStoreForSandboxCtrl(t)
	srv, err := New(Config{Identity: srvID, Peers: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if srv.NodeRegistry() == nil {
		t.Error("NodeRegistry() = nil, want default")
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

// TestRegisterNodeRecordsEntryAndForgetsOnDisconnect exercises
// the RegisterNode happy path end-to-end against a real Connect
// server (so the server-stream + ctx-cancel lifecycle is real).
// Coverage-wise this is what lifts orchestrator/nodectrl.go
// above the per-file gate.
func TestRegisterNodeRecordsEntryAndForgetsOnDisconnect(t *testing.T) {
	rig := newSandboxCtrlRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stream, err := rig.client.RegisterNode(ctx, connect.NewRequest(
		&nodectrlv1.RegisterNodeRequest{
			Version:         "v0.0.0",
			Resources:       &nodectrlv1.HostResources{Cpus: 2, MemoryBytes: 1 << 30},
			ProtocolVersion: testProtocolVersion(),
		},
	))
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
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
		&nodectrlv1.HeartbeatRequest{Current: &nodectrlv1.HostResources{Cpus: 2, MemoryBytes: 512 << 20}},
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
// client for the NodeControlService. Kept local to this test
// file — exporting it would couple orchestrator's test surface to
// internal layering decisions that are still in flux for Slice A.
type sandboxCtrlRig struct {
	server *Server
	reg    *nodectrl.Registry
	sbID   *identity.Identity
	client nodectrlv1connect.NodeControlServiceClient
}

func newSandboxCtrlRig(t *testing.T) *sandboxCtrlRig {
	t.Helper()
	reg := nodectrl.NewRegistry(nil)
	env := newOrchTestEnv(t, identity.RoleSandboxHost, Config{NodeRegistry: reg})
	client := nodectrlv1connect.NewNodeControlServiceClient(env.httpc, env.baseURL)
	return &sandboxCtrlRig{
		server: env.server,
		reg:    reg,
		sbID:   env.callerID,
		client: client,
	}
}

// TestDispatchReachesActiveStream drives Dispatch end-to-end: open
// a RegisterNode stream, push a Dispatch on the server side,
// receive the event over the wire, decode the spec.
func TestDispatchReachesActiveStream(t *testing.T) {
	rig := newSandboxCtrlRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stream, err := rig.client.RegisterNode(ctx, connect.NewRequest(
		&nodectrlv1.RegisterNodeRequest{
			Version:         "v0",
			ProtocolVersion: testProtocolVersion(),
		},
	))
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	if !stream.Receive() || stream.Msg().GetReady() == nil {
		t.Fatalf("expected Ready, got err=%v", stream.Err())
	}

	// Wait briefly for the handler to install its sender; the
	// Ready ack is sent before attachStream returns in the loop,
	// so a direct publish right after Receive may race.
	want := &sandboxv1.Sandbox{
		Metadata: &sandboxv1.SandboxMetadata{Uid: testSandboxID},
		Spec:     &sandboxv1.SandboxSpec{ImageRef: "alpine:3", WorkspaceUuid: "ws-1"},
	}
	if err := publishUntilReady(rig.server.SandboxControl(), rig.sbID.Fingerprint(), want); err != nil {
		t.Fatalf("PublishSandboxEvent: %v", err)
	}

	if !stream.Receive() {
		t.Fatalf("expected SandboxWatchEvent, got err=%v", stream.Err())
	}
	evt := stream.Msg().GetSandbox()
	if evt == nil {
		t.Fatalf("event = %+v, want SandboxWatchEvent", stream.Msg())
	}
	if evt.GetType() != sandboxv1.EventType_EVENT_TYPE_ADDED {
		t.Errorf("type = %s, want ADDED", evt.GetType())
	}
	sb := evt.GetSandbox()
	if sb.GetMetadata().GetUid() != testSandboxID {
		t.Errorf("uid = %q, want sb-1", sb.GetMetadata().GetUid())
	}
	if sb.GetSpec().GetImageRef() != "alpine:3" {
		t.Errorf("image_ref = %q, want alpine:3", sb.GetSpec().GetImageRef())
	}
}

// publishUntilReady retries PublishSandboxEvent for up to 1s while
// the handler's attachStream call is still racing in.
// ErrNodeNotConnected is the only retryable error.
func publishUntilReady(
	h *NodeControlHandler,
	fp identity.Fingerprint,
	sandbox *sandboxv1.Sandbox,
) error {
	deadline := time.Now().Add(time.Second)
	//daft:lint:disable=forloopbound reason="wall-clock bounded to 1s while the handler's attachStream goroutine races with the client's first Receive"
	for time.Now().Before(deadline) {
		err := h.PublishSandboxEvent(fp, sandboxv1.EventType_EVENT_TYPE_ADDED, sandbox)
		if !errors.Is(err, ErrNodeNotConnected) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ErrNodeNotConnected
}

// TestPublishSandboxEventPanicsOnNilSandbox pins the precondition
// assert — a nil sandbox is a wiring bug and must panic loudly, not
// produce a half-formed envelope.
func TestPublishSandboxEventPanicsOnNilSandbox(t *testing.T) {
	h, err := NewNodeControlHandler(nodectrl.NewRegistry(nil))
	if err != nil {
		t.Fatalf("NewNodeControlHandler: %v", err)
	}
	id := mustGenerate(t, identity.RoleSandboxHost)
	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("PublishSandboxEvent did not panic on nil Sandbox")
		}
		msg, _ := got.(string)
		if !strings.Contains(msg, "non-nil Sandbox") {
			t.Errorf("panic = %q, want substring 'non-nil Sandbox'", msg)
		}
	}()
	_ = h.PublishSandboxEvent(id.Fingerprint(), sandboxv1.EventType_EVENT_TYPE_ADDED, nil)
}

// TestPublishSandboxEventPanicsOnEmptyUID pins the uid assert.
func TestPublishSandboxEventPanicsOnEmptyUID(t *testing.T) {
	h, err := NewNodeControlHandler(nodectrl.NewRegistry(nil))
	if err != nil {
		t.Fatalf("NewNodeControlHandler: %v", err)
	}
	id := mustGenerate(t, identity.RoleSandboxHost)
	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("PublishSandboxEvent did not panic on empty uid")
		}
		msg, _ := got.(string)
		if !strings.Contains(msg, "non-empty Sandbox.metadata.uid") {
			t.Errorf("panic = %q, want substring 'non-empty Sandbox.metadata.uid'", msg)
		}
	}()
	_ = h.PublishSandboxEvent(id.Fingerprint(), sandboxv1.EventType_EVENT_TYPE_ADDED,
		&sandboxv1.Sandbox{Metadata: &sandboxv1.SandboxMetadata{}})
}

// TestPublishSandboxEventUnknownNode covers ErrNodeNotConnected:
// no stream was ever opened, so the handler has no sender.
func TestPublishSandboxEventUnknownNode(t *testing.T) {
	h, err := NewNodeControlHandler(nodectrl.NewRegistry(nil))
	if err != nil {
		t.Fatalf("NewNodeControlHandler: %v", err)
	}
	id := mustGenerate(t, identity.RoleSandboxHost)
	err = h.PublishSandboxEvent(id.Fingerprint(), sandboxv1.EventType_EVENT_TYPE_ADDED,
		&sandboxv1.Sandbox{Metadata: &sandboxv1.SandboxMetadata{Uid: testSandboxID}})
	if !errors.Is(err, ErrNodeNotConnected) {
		t.Errorf("err = %v, want ErrNodeNotConnected", err)
	}
}

// testProtocolVersion returns a wire ProtocolVersion that matches
// this build's local constants — the value every shipped client
// sends. Tests use it so that adding a new field to the handshake
// only changes one place.
func testProtocolVersion() *nodectrlv1.ProtocolVersion {
	return &nodectrlv1.ProtocolVersion{
		Major: nodectrl.ProtocolVersionMajor,
		Minor: nodectrl.ProtocolVersionMinor,
		Patch: nodectrl.ProtocolVersionPatch,
	}
}

// TestRegisterNodeRejectsIncompatibleProtocolMajor pins the
// server-side half of the version handshake: a peer reporting a
// different major must be refused before any registry mutation.
func TestRegisterNodeRejectsIncompatibleProtocolMajor(t *testing.T) {
	rig := newSandboxCtrlRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stream, err := rig.client.RegisterNode(ctx, connect.NewRequest(
		&nodectrlv1.RegisterNodeRequest{
			Version: "v0",
			ProtocolVersion: &nodectrlv1.ProtocolVersion{
				Major: nodectrl.ProtocolVersionMajor + 99,
			},
		},
	))
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	// The stream should close before any event is delivered, with
	// a FailedPrecondition surfaced via stream.Err().
	if stream.Receive() {
		t.Fatalf("expected stream close, got event %+v", stream.Msg())
	}
	if code := connect.CodeOf(stream.Err()); code != connect.CodeFailedPrecondition {
		t.Errorf("err code = %s, want FailedPrecondition (err=%v)", code, stream.Err())
	}
	if _, ok := rig.reg.Lookup(rig.sbID.Fingerprint()); ok {
		t.Error("registry recorded a peer with incompatible protocol version")
	}
}

// TestRegisterNodeRejectsMissingProtocolVersion pins that the
// zero value (omitted field) is treated as major=0, which never
// matches our local major. Catches the "I forgot to set the field"
// bug class.
func TestRegisterNodeRejectsMissingProtocolVersion(t *testing.T) {
	rig := newSandboxCtrlRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stream, err := rig.client.RegisterNode(ctx, connect.NewRequest(
		&nodectrlv1.RegisterNodeRequest{Version: "v0"},
	))
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	if stream.Receive() {
		t.Fatalf("expected stream close, got event %+v", stream.Msg())
	}
	if code := connect.CodeOf(stream.Err()); code != connect.CodeFailedPrecondition {
		t.Errorf("err code = %s, want FailedPrecondition (err=%v)", code, stream.Err())
	}
}

// TestSubscriptionReadyCarriesProtocolVersion pins that the
// orchestrator echoes its own ProtocolVersion in the first event
// so the sandboxd can check the other direction.
func TestSubscriptionReadyCarriesProtocolVersion(t *testing.T) {
	rig := newSandboxCtrlRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stream, err := rig.client.RegisterNode(ctx, connect.NewRequest(
		&nodectrlv1.RegisterNodeRequest{
			Version:         "v0",
			ProtocolVersion: testProtocolVersion(),
		},
	))
	if err != nil {
		t.Fatalf("RegisterNode: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	if !stream.Receive() {
		t.Fatalf("stream closed before Ready: %v", stream.Err())
	}
	ready := stream.Msg().GetReady()
	if ready == nil {
		t.Fatal("first event was not SubscriptionReady")
	}
	pv := ready.GetProtocolVersion()
	if pv == nil {
		t.Fatal("Ready.protocol_version not set")
	}
	if pv.GetMajor() != nodectrl.ProtocolVersionMajor ||
		pv.GetMinor() != nodectrl.ProtocolVersionMinor ||
		pv.GetPatch() != nodectrl.ProtocolVersionPatch {
		t.Errorf(
			"Ready.protocol_version = %d.%d.%d, want %d.%d.%d",
			pv.GetMajor(), pv.GetMinor(), pv.GetPatch(),
			nodectrl.ProtocolVersionMajor,
			nodectrl.ProtocolVersionMinor,
			nodectrl.ProtocolVersionPatch,
		)
	}
}

// TestSandboxEventBacklogFull covers ErrSandboxEventBacklog: fill
// the per-stream channel without anyone draining it, then push one
// more. attachStream/detachStream are exercised directly so we
// don't need a real client.
func TestSandboxEventBacklogFull(t *testing.T) {
	h, err := NewNodeControlHandler(nodectrl.NewRegistry(nil))
	if err != nil {
		t.Fatalf("NewNodeControlHandler: %v", err)
	}
	id := mustGenerate(t, identity.RoleSandboxHost)
	fp := id.Fingerprint()
	h.attachStream(fp)
	t.Cleanup(func() { h.detachStream(fp) })

	sb := &sandboxv1.Sandbox{Metadata: &sandboxv1.SandboxMetadata{Uid: "sb-x"}}
	for range sandboxEventBacklog {
		if err := h.PublishSandboxEvent(fp, sandboxv1.EventType_EVENT_TYPE_ADDED, sb); err != nil {
			t.Fatalf("PublishSandboxEvent (fill): %v", err)
		}
	}
	if err := h.PublishSandboxEvent(fp, sandboxv1.EventType_EVENT_TYPE_ADDED, sb); !errors.Is(err, ErrSandboxEventBacklog) {
		t.Errorf("err = %v, want ErrSandboxEventBacklog", err)
	}
}
