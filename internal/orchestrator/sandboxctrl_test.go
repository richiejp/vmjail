package orchestrator

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

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
	req3 := connect.NewRequest(&sandboxctrlv1.ListSandboxesRequest{})
	if _, err := h.ListSandboxes(t.Context(), req3); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("ListSandboxes: code = %s, want Unauthenticated", connect.CodeOf(err))
	}
}

// TestListSandboxesReflectsRegistry pins the projection from the
// registry snapshot onto the wire SandboxRegistration shape: every
// registered entry shows up exactly once, sorted by fingerprint, with
// version + resources + timestamps preserved.
func TestListSandboxesReflectsRegistry(t *testing.T) {
	reg := sandboxctrl.NewRegistry(nil)
	h, err := NewSandboxControlHandler(reg)
	if err != nil {
		t.Fatalf("NewSandboxControlHandler: %v", err)
	}
	caller := mustGenerate(t, identity.RoleOrchestrator)
	sandboxA := mustGenerate(t, identity.RoleSandboxHost)
	sandboxB := mustGenerate(t, identity.RoleSandboxHost)
	reg.Register(sandboxA.Fingerprint(), "v1.0.0", sandboxctrl.Resources{CPUs: 2, MemoryBytes: 1 << 30})
	reg.Register(sandboxB.Fingerprint(), "v1.0.1", sandboxctrl.Resources{CPUs: 4, MemoryBytes: 4 << 30})

	req := connect.NewRequest(&sandboxctrlv1.ListSandboxesRequest{})
	req.Header().Set(peerFingerprintHeader, caller.Fingerprint().String())
	resp, err := h.ListSandboxes(t.Context(), req)
	if err != nil {
		t.Fatalf("ListSandboxes: %v", err)
	}
	got := resp.Msg.GetSandboxes()
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
	var entryA *sandboxctrlv1.SandboxRegistration
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
	if entryA.GetResources().GetCpus() != 2 || entryA.GetResources().GetMemoryBytes() != 1<<30 {
		t.Errorf("resources = %+v, want {2, 1GiB}", entryA.GetResources())
	}
	if !entryA.GetRegisteredAt().IsValid() || !entryA.GetLastSeenAt().IsValid() {
		t.Error("timestamps not populated")
	}
}

// TestListSandboxesEmpty exercises the zero-entry path explicitly.
// Skipping it would leave the empty Registry projection uncovered
// when the test above populates the registry first.
func TestListSandboxesEmpty(t *testing.T) {
	h, err := NewSandboxControlHandler(sandboxctrl.NewRegistry(nil))
	if err != nil {
		t.Fatalf("NewSandboxControlHandler: %v", err)
	}
	caller := mustGenerate(t, identity.RoleOrchestrator)
	req := connect.NewRequest(&sandboxctrlv1.ListSandboxesRequest{})
	req.Header().Set(peerFingerprintHeader, caller.Fingerprint().String())
	resp, err := h.ListSandboxes(t.Context(), req)
	if err != nil {
		t.Fatalf("ListSandboxes: %v", err)
	}
	if len(resp.Msg.GetSandboxes()) != 0 {
		t.Errorf("len(sandboxes) = %d, want 0", len(resp.Msg.GetSandboxes()))
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
			Version:         "v0.0.0",
			Resources:       &sandboxctrlv1.HostResources{Cpus: 2, MemoryBytes: 1 << 30},
			ProtocolVersion: testProtocolVersion(),
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
	reg := sandboxctrl.NewRegistry(nil)
	env := newOrchTestEnv(t, identity.RoleSandboxHost, Config{SandboxRegistry: reg})
	client := sandboxctrlv1connect.NewSandboxControlServiceClient(env.httpc, env.baseURL)
	return &sandboxCtrlRig{
		server: env.server,
		reg:    reg,
		sbID:   env.callerID,
		client: client,
	}
}

// TestDispatchReachesActiveStream drives Dispatch end-to-end: open
// a RegisterSandbox stream, push a Dispatch on the server side,
// receive the event over the wire, decode the spec.
func TestDispatchReachesActiveStream(t *testing.T) {
	rig := newSandboxCtrlRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stream, err := rig.client.RegisterSandbox(ctx, connect.NewRequest(
		&sandboxctrlv1.RegisterSandboxRequest{
			Version:         "v0",
			ProtocolVersion: testProtocolVersion(),
		},
	))
	if err != nil {
		t.Fatalf("RegisterSandbox: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	if !stream.Receive() || stream.Msg().GetReady() == nil {
		t.Fatalf("expected Ready, got err=%v", stream.Err())
	}

	// Wait briefly for the handler to install its sender; the
	// Ready ack is sent before attachStream returns in the loop,
	// so a direct Dispatch right after Receive may race.
	want := &sandboxctrlv1.SandboxSpec{ImageRef: "alpine:3", WorkspaceUuid: "ws-1"}
	if err := dispatchUntilReady(rig.server.SandboxControl(), rig.sbID.Fingerprint(), "d-1", want); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if !stream.Receive() {
		t.Fatalf("expected Dispatch, got err=%v", stream.Err())
	}
	d := stream.Msg().GetDispatch()
	if d == nil {
		t.Fatalf("event = %+v, want Dispatch", stream.Msg())
	}
	if d.GetDispatchId() != "d-1" {
		t.Errorf("dispatch_id = %q, want d-1", d.GetDispatchId())
	}
	if d.GetSpec().GetImageRef() != "alpine:3" {
		t.Errorf("image_ref = %q, want alpine:3", d.GetSpec().GetImageRef())
	}
}

// dispatchUntilReady retries Dispatch for up to 1s while the
// handler's attachStream call is still racing in. ErrSandboxNotConnected
// is the only retryable error.
func dispatchUntilReady(
	h *SandboxControlHandler,
	fp identity.Fingerprint,
	id string,
	spec *sandboxctrlv1.SandboxSpec,
) error {
	deadline := time.Now().Add(time.Second)
	//daft:lint:disable=forloopbound reason="wall-clock bounded to 1s while the handler's attachStream goroutine races with the client's first Receive"
	for time.Now().Before(deadline) {
		err := h.Dispatch(fp, id, spec)
		if !errors.Is(err, ErrSandboxNotConnected) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ErrSandboxNotConnected
}

// TestDispatchPanicsOnNilSpec pins the precondition assert — a
// nil spec is a wiring bug and must panic loudly, not produce a
// half-formed dispatch envelope.
func TestDispatchPanicsOnNilSpec(t *testing.T) {
	h, err := NewSandboxControlHandler(sandboxctrl.NewRegistry(nil))
	if err != nil {
		t.Fatalf("NewSandboxControlHandler: %v", err)
	}
	id := mustGenerate(t, identity.RoleSandboxHost)
	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("Dispatch did not panic on nil spec")
		}
		msg, _ := got.(string)
		if !strings.Contains(msg, "non-nil spec") {
			t.Errorf("panic = %q, want substring 'non-nil spec'", msg)
		}
	}()
	_ = h.Dispatch(id.Fingerprint(), "d-1", nil)
}

// TestDispatchPanicsOnEmptyID pins the dispatch_id assert.
func TestDispatchPanicsOnEmptyID(t *testing.T) {
	h, err := NewSandboxControlHandler(sandboxctrl.NewRegistry(nil))
	if err != nil {
		t.Fatalf("NewSandboxControlHandler: %v", err)
	}
	id := mustGenerate(t, identity.RoleSandboxHost)
	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("Dispatch did not panic on empty id")
		}
		msg, _ := got.(string)
		if !strings.Contains(msg, "non-empty dispatch_id") {
			t.Errorf("panic = %q, want substring 'non-empty dispatch_id'", msg)
		}
	}()
	_ = h.Dispatch(id.Fingerprint(), "", &sandboxctrlv1.SandboxSpec{})
}

// TestDispatchUnknownSandbox covers ErrSandboxNotConnected: no
// stream was ever opened, so the handler has no sender to push to.
func TestDispatchUnknownSandbox(t *testing.T) {
	h, err := NewSandboxControlHandler(sandboxctrl.NewRegistry(nil))
	if err != nil {
		t.Fatalf("NewSandboxControlHandler: %v", err)
	}
	id := mustGenerate(t, identity.RoleSandboxHost)
	err = h.Dispatch(id.Fingerprint(), "d-1", &sandboxctrlv1.SandboxSpec{})
	if !errors.Is(err, ErrSandboxNotConnected) {
		t.Errorf("err = %v, want ErrSandboxNotConnected", err)
	}
}

// testProtocolVersion returns a wire ProtocolVersion that matches
// this build's local constants — the value every shipped client
// sends. Tests use it so that adding a new field to the handshake
// only changes one place.
func testProtocolVersion() *sandboxctrlv1.ProtocolVersion {
	return &sandboxctrlv1.ProtocolVersion{
		Major: sandboxctrl.ProtocolVersionMajor,
		Minor: sandboxctrl.ProtocolVersionMinor,
		Patch: sandboxctrl.ProtocolVersionPatch,
	}
}

// TestRegisterSandboxRejectsIncompatibleProtocolMajor pins the
// server-side half of the version handshake: a peer reporting a
// different major must be refused before any registry mutation.
func TestRegisterSandboxRejectsIncompatibleProtocolMajor(t *testing.T) {
	rig := newSandboxCtrlRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stream, err := rig.client.RegisterSandbox(ctx, connect.NewRequest(
		&sandboxctrlv1.RegisterSandboxRequest{
			Version: "v0",
			ProtocolVersion: &sandboxctrlv1.ProtocolVersion{
				Major: sandboxctrl.ProtocolVersionMajor + 99,
			},
		},
	))
	if err != nil {
		t.Fatalf("RegisterSandbox: %v", err)
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

// TestRegisterSandboxRejectsMissingProtocolVersion pins that the
// zero value (omitted field) is treated as major=0, which never
// matches our local major. Catches the "I forgot to set the field"
// bug class.
func TestRegisterSandboxRejectsMissingProtocolVersion(t *testing.T) {
	rig := newSandboxCtrlRig(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stream, err := rig.client.RegisterSandbox(ctx, connect.NewRequest(
		&sandboxctrlv1.RegisterSandboxRequest{Version: "v0"},
	))
	if err != nil {
		t.Fatalf("RegisterSandbox: %v", err)
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

	stream, err := rig.client.RegisterSandbox(ctx, connect.NewRequest(
		&sandboxctrlv1.RegisterSandboxRequest{
			Version:         "v0",
			ProtocolVersion: testProtocolVersion(),
		},
	))
	if err != nil {
		t.Fatalf("RegisterSandbox: %v", err)
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
	if pv.GetMajor() != sandboxctrl.ProtocolVersionMajor ||
		pv.GetMinor() != sandboxctrl.ProtocolVersionMinor ||
		pv.GetPatch() != sandboxctrl.ProtocolVersionPatch {
		t.Errorf(
			"Ready.protocol_version = %d.%d.%d, want %d.%d.%d",
			pv.GetMajor(), pv.GetMinor(), pv.GetPatch(),
			sandboxctrl.ProtocolVersionMajor,
			sandboxctrl.ProtocolVersionMinor,
			sandboxctrl.ProtocolVersionPatch,
		)
	}
}

// TestDispatchBacklogFull covers ErrDispatchBacklog: fill the
// per-stream channel without anyone draining it, then push one
// more. attachStream/detachStream are exercised directly so we
// don't need a real client.
func TestDispatchBacklogFull(t *testing.T) {
	h, err := NewSandboxControlHandler(sandboxctrl.NewRegistry(nil))
	if err != nil {
		t.Fatalf("NewSandboxControlHandler: %v", err)
	}
	id := mustGenerate(t, identity.RoleSandboxHost)
	fp := id.Fingerprint()
	h.attachStream(fp)
	t.Cleanup(func() { h.detachStream(fp) })

	for range dispatchBacklog {
		if err := h.Dispatch(fp, "x", &sandboxctrlv1.SandboxSpec{}); err != nil {
			t.Fatalf("Dispatch (fill): %v", err)
		}
	}
	if err := h.Dispatch(fp, "x", &sandboxctrlv1.SandboxSpec{}); !errors.Is(err, ErrDispatchBacklog) {
		t.Errorf("err = %v, want ErrDispatchBacklog", err)
	}
}
