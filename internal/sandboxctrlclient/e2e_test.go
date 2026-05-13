package sandboxctrlclient_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	sandboxctrlv1 "github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/orchestrator"
	"github.com/richiejp/darkaifactory/internal/sandboxctrl"
	"github.com/richiejp/darkaifactory/internal/sandboxctrlclient"
)

// dispatchFn adapts a function to the DispatchHandler interface so
// tests can use closures without declaring a struct.
type dispatchFn func(context.Context, *sandboxctrlv1.Dispatch) error

func (f dispatchFn) HandleDispatch(ctx context.Context, d *sandboxctrlv1.Dispatch) error {
	return f(ctx, d)
}

const (
	testListenAddr = "127.0.0.1:0"
	testFixtureVer = "test-v0"
)

// e2eRig is an in-process orchestrator + a sandbox identity that
// has been TOFU-approved on both sides. Tests use it to exercise
// the real RegisterSandbox/Heartbeat path over mTLS.
type e2eRig struct {
	server *orchestrator.Server
	url    string
	sbID   *identity.Identity
	sbPeer *identity.PeerStore
	reg    *sandboxctrl.Registry
}

func newE2ERig(t *testing.T) *e2eRig {
	t.Helper()
	srvID, err := identity.Generate(identity.RoleOrchestrator)
	if err != nil {
		t.Fatalf("Generate orchestrator: %v", err)
	}
	sbID, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate sandbox: %v", err)
	}

	srvPeers, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "srv-peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore srv: %v", err)
	}
	if err := srvPeers.Add(identity.Peer{Fingerprint: sbID.Fingerprint()}); err != nil {
		t.Fatalf("srv approve sb: %v", err)
	}
	reg := sandboxctrl.NewRegistry(nil)

	srv, err := orchestrator.New(orchestrator.Config{
		Identity:        srvID,
		Peers:           srvPeers,
		Addr:            testListenAddr,
		SandboxRegistry: reg,
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
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
		case err := <-srvErr:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Logf("Serve: %v", err)
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

	return &e2eRig{
		server: srv,
		url:    "https://" + srv.Addr(),
		sbID:   sbID,
		sbPeer: sbPeers,
		reg:    reg,
	}
}

func TestRegisterAndServeRecordsRegistrationAndHeartbeat(t *testing.T) {
	rig := newE2ERig(t)
	client, err := sandboxctrlclient.New(sandboxctrlclient.Config{
		URL:               rig.url,
		Identity:          rig.sbID,
		Peers:             rig.sbPeer,
		Version:           testFixtureVer,
		HeartbeatInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	advertised := sandboxctrl.Resources{CPUs: 8, MemoryBytes: 16 << 30}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- client.RegisterAndServe(ctx, advertised)
	}()

	// Wait up to a second for the registration to appear with at
	// least one heartbeat refresh visible (LastSeenAt > RegisteredAt).
	fp := rig.sbID.Fingerprint()
	deadline := time.Now().Add(time.Second)
	var entry sandboxctrl.Entry
	var heartbeatObserved bool
	//daft:lint:disable=forloopbound reason="exits on deadline timeout or once heartbeat refresh is observed; bounded by 1s wall clock"
	for time.Now().Before(deadline) {
		if e, ok := rig.reg.Lookup(fp); ok {
			entry = e
			if e.LastSeenAt.After(e.RegisteredAt) {
				heartbeatObserved = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if entry.Version != testFixtureVer {
		t.Errorf("registry Version = %q, want %q", entry.Version, testFixtureVer)
	}
	if entry.Resources != advertised {
		t.Errorf("registry Resources = %+v, want %+v", entry.Resources, advertised)
	}
	if !heartbeatObserved {
		t.Errorf("no heartbeat observed within 1s; LastSeenAt = %v RegisteredAt = %v",
			entry.LastSeenAt, entry.RegisteredAt)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("RegisterAndServe returned %v, want nil on ctx cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RegisterAndServe did not return after ctx cancel")
	}

	// Entry should be released once the server-side handler
	// observes the closed stream. The client-side ctx cancel is
	// asynchronous with respect to the server's request ctx, so
	// poll briefly.
	cleanupDeadline := time.Now().Add(2 * time.Second)
	//daft:lint:disable=forloopbound reason="exits when the registry entry is released or on the 2s wall-clock deadline"
	for time.Now().Before(cleanupDeadline) {
		if _, ok := rig.reg.Lookup(fp); !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("registry still has entry 2s after RegisterAndServe returned")
}

// TestRegisterAndServeFailsOnUnreachableOrchestrator covers the
// dial-fail branch of RegisterAndServe (RegisterSandbox returns an
// error before any stream is opened).
func TestRegisterAndServeFailsOnUnreachableOrchestrator(t *testing.T) {
	id, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	peer, err := identity.Generate(identity.RoleOrchestrator)
	if err != nil {
		t.Fatalf("Generate peer: %v", err)
	}
	peers, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	if err := peers.Add(identity.Peer{Fingerprint: peer.Fingerprint()}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	client, err := sandboxctrlclient.New(sandboxctrlclient.Config{
		URL:      "https://127.0.0.1:1", // closed port
		Identity: id,
		Peers:    peers,
		Version:  testFixtureVer,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.RegisterAndServe(ctx, sandboxctrl.Resources{}); err == nil {
		t.Fatal("RegisterAndServe succeeded against unreachable URL")
	}
}

// TestRegisterAndServePropagatesHeartbeatError covers the sendHeartbeat
// error branch: forcibly evict the registration from the orchestrator's
// registry mid-run; the next Heartbeat tick will return
// FailedPrecondition and RegisterAndServe must surface it.
func TestRegisterAndServePropagatesHeartbeatError(t *testing.T) {
	rig := newE2ERig(t)
	client, err := sandboxctrlclient.New(sandboxctrlclient.Config{
		URL:               rig.url,
		Identity:          rig.sbID,
		Peers:             rig.sbPeer,
		Version:           testFixtureVer,
		HeartbeatInterval: 30 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	served := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		served <- client.RegisterAndServe(ctx, sandboxctrl.Resources{CPUs: 1})
	}()

	// Wait for the registration to appear, then forcibly Forget so
	// the next Heartbeat tick gets FailedPrecondition.
	fp := rig.sbID.Fingerprint()
	regDeadline := time.Now().Add(time.Second)
	//daft:lint:disable=forloopbound reason="waits for registration or 1s timeout"
	for time.Now().Before(regDeadline) {
		if _, ok := rig.reg.Lookup(fp); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rig.reg.Forget(fp)

	select {
	case err := <-served:
		if err == nil {
			t.Error("RegisterAndServe returned nil, want heartbeat error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RegisterAndServe did not return within 2s after Forget")
	}
}

// TestRegisterAndServeDeliversDispatchToHandler exercises the
// orchestrator → sandboxd Dispatch path end-to-end: orchestrator
// pushes Dispatch on the stream; the sandboxctrlclient receives it
// and invokes the configured DispatchHandler with the spec.
func TestRegisterAndServeDeliversDispatchToHandler(t *testing.T) {
	rig := newE2ERig(t)

	got := make(chan *sandboxctrlv1.Dispatch, 1)
	handler := dispatchFn(func(_ context.Context, d *sandboxctrlv1.Dispatch) error {
		got <- d
		return nil
	})

	client, err := sandboxctrlclient.New(sandboxctrlclient.Config{
		URL:               rig.url,
		Identity:          rig.sbID,
		Peers:             rig.sbPeer,
		Version:           testFixtureVer,
		HeartbeatInterval: 5 * time.Second,
		Dispatch:          handler,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- client.RegisterAndServe(ctx, sandboxctrl.Resources{CPUs: 1})
	}()

	// Wait for registration to land before pushing — Dispatch
	// before attachStream returns ErrSandboxNotConnected.
	fp := rig.sbID.Fingerprint()
	deadline := time.Now().Add(time.Second)
	want := &sandboxctrlv1.SandboxSpec{ImageRef: "alpine:3", WorkspaceUuid: "ws-1"}
	//daft:lint:disable=forloopbound reason="poll until the orchestrator sees the stream attach (≤1s)"
	for time.Now().Before(deadline) {
		err := rig.server.SandboxControl().Dispatch(fp, "d-1", want)
		if err == nil {
			break
		}
		if !errors.Is(err, orchestrator.ErrSandboxNotConnected) {
			t.Fatalf("Dispatch: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case d := <-got:
		if d.GetDispatchId() != "d-1" {
			t.Errorf("dispatch_id = %q, want d-1", d.GetDispatchId())
		}
		if d.GetSpec().GetImageRef() != "alpine:3" {
			t.Errorf("image_ref = %q, want alpine:3", d.GetSpec().GetImageRef())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler not invoked within 2s")
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("RegisterAndServe = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RegisterAndServe did not return after ctx cancel")
	}
}

// TestRegisterAndServeSurfacesDispatchError covers the failure
// branch of consumeStream's handler call: a non-nil return tears
// the registration down.
func TestRegisterAndServeSurfacesDispatchError(t *testing.T) {
	rig := newE2ERig(t)
	want := errors.New("synthetic handler failure")
	handler := dispatchFn(func(context.Context, *sandboxctrlv1.Dispatch) error {
		return want
	})

	client, err := sandboxctrlclient.New(sandboxctrlclient.Config{
		URL:               rig.url,
		Identity:          rig.sbID,
		Peers:             rig.sbPeer,
		Version:           testFixtureVer,
		HeartbeatInterval: 5 * time.Second,
		Dispatch:          handler,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- client.RegisterAndServe(ctx, sandboxctrl.Resources{CPUs: 1})
	}()

	fp := rig.sbID.Fingerprint()
	deadline := time.Now().Add(time.Second)
	//daft:lint:disable=forloopbound reason="poll until the stream attaches"
	for time.Now().Before(deadline) {
		err := rig.server.SandboxControl().Dispatch(fp, "d-bad", &sandboxctrlv1.SandboxSpec{})
		if err == nil {
			break
		}
		if !errors.Is(err, orchestrator.ErrSandboxNotConnected) {
			t.Fatalf("Dispatch: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case err := <-served:
		if err == nil {
			t.Error("RegisterAndServe returned nil, want handler error to propagate")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RegisterAndServe did not return after handler error")
	}
}

func TestRegisterAndServeRejectsUnapprovedPeer(t *testing.T) {
	rig := newE2ERig(t)
	// Generate a fresh sandbox identity that the orchestrator has
	// NOT approved; the TLS handshake must fail.
	stranger, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	client, err := sandboxctrlclient.New(sandboxctrlclient.Config{
		URL:      rig.url,
		Identity: stranger,
		Peers:    rig.sbPeer,
		Version:  testFixtureVer,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.RegisterAndServe(ctx, sandboxctrl.Resources{}); err == nil {
		t.Fatal("RegisterAndServe succeeded for unapproved peer; want TLS/auth error")
	}
}
