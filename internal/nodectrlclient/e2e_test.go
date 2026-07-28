package nodectrlclient_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/nodectrlclient"
	"github.com/richiejp/darkaifactory/internal/nodejob"
	"github.com/richiejp/darkaifactory/internal/orchestrator"
)

// syncBuffer is a thread-safe bytes.Buffer for capturing
// concurrent log output from goroutine-driven event handlers.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const (
	testListenAddr = "127.0.0.1:0"
	testFixtureVer = "test-v0"
	testJobSandbox = "sb-e2e"
)

// publishUntilAttached retries PublishSandboxEvent for up to 1s
// while the orchestrator's RegisterNode handler is still racing to
// attach its per-fingerprint stream. ErrNodeNotConnected is the
// only retryable error.
func publishUntilAttached(
	t *testing.T,
	h *orchestrator.NodeControlHandler,
	fp identity.Fingerprint,
	sandbox *sandboxv1.Sandbox,
) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	//daft:lint:disable=forloopbound reason="wall-clock bounded to 1s; exits on attach or transient retry exhaustion"
	for time.Now().Before(deadline) {
		err := h.PublishSandboxEvent(fp, sandboxv1.EventType_EVENT_TYPE_ADDED, sandbox)
		if err == nil {
			return
		}
		if !errors.Is(err, orchestrator.ErrNodeNotConnected) {
			t.Fatalf("PublishSandboxEvent: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("orchestrator stream did not attach within 1s")
}

// e2eRig is an in-process orchestrator + a sandbox identity that
// has been TOFU-approved on both sides. Tests use it to exercise
// the real RegisterNode/Heartbeat path over mTLS.
type e2eRig struct {
	server *orchestrator.Server
	url    string
	sbID   *identity.Identity
	sbPeer *identity.PeerStore
	reg    *nodectrl.Registry
}

type e2eJobManager struct {
	mu      sync.Mutex
	deleted bool
}

func (m *e2eJobManager) Start(req nodejob.StartRequest) (nodejob.Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return nodejob.Snapshot{ID: "job-e2e", SandboxUID: req.SandboxUID, State: nodejob.StateRunning}, nil
}

func (m *e2eJobManager) Get(id string, _, _ uint64, _ int) (nodejob.Snapshot, error) {
	return nodejob.Snapshot{ID: id, SandboxUID: testJobSandbox, State: nodejob.StateSucceeded, ExitCodeSet: true}, nil
}

func (m *e2eJobManager) Cancel(id string) (nodejob.Snapshot, error) {
	return nodejob.Snapshot{ID: id, SandboxUID: testJobSandbox, State: nodejob.StateCanceled, CancelCalled: true}, nil
}

func (m *e2eJobManager) Delete(string) error {
	m.mu.Lock()
	m.deleted = true
	m.mu.Unlock()
	return nil
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
	if err := srvPeers.Add(identity.Peer{Fingerprint: sbID.Fingerprint(), Role: identity.RoleSandboxHost}); err != nil {
		t.Fatalf("srv approve sb: %v", err)
	}
	reg := nodectrl.NewRegistry(nil)

	srv, err := orchestrator.New(orchestrator.Config{
		Identity:     srvID,
		Peers:        srvPeers,
		Addr:         testListenAddr,
		NodeRegistry: reg,
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
	client, err := nodectrlclient.New(nodectrlclient.Config{
		URL:               rig.url,
		Identity:          rig.sbID,
		Peers:             rig.sbPeer,
		Version:           testFixtureVer,
		HeartbeatInterval: 50 * time.Millisecond,
		WorkspaceStorageSnapshot: func(context.Context) []*nodectrlv1.WorkspaceStorageReport {
			return []*nodectrlv1.WorkspaceStorageReport{{WorkspaceUuid: "heartbeat-workspace"}}
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	advertised := nodectrl.Resources{CPUs: 8, MemoryBytes: 16 << 30}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- client.RegisterAndServe(ctx, advertised)
	}()

	// Wait up to a second for the registration to appear with at
	// least one heartbeat refresh visible (LastSeenAt > RegisteredAt).
	fp := rig.sbID.Fingerprint()
	deadline := time.Now().Add(time.Second)
	var entry nodectrl.Entry
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

func TestRegisterAndServeJobControlRoundTrip(t *testing.T) {
	rig := newE2ERig(t)
	manager := &e2eJobManager{}
	client, err := nodectrlclient.New(nodectrlclient.Config{
		URL: rig.url, Identity: rig.sbID, Peers: rig.sbPeer, Version: testFixtureVer,
		HeartbeatInterval: time.Second, JobManager: manager,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- client.RegisterAndServe(ctx, nodectrl.Resources{}) }()
	deadline := time.Now().Add(time.Second)
	//daft:lint:disable=forloopbound reason="bounded to one second while the real registration stream attaches"
	for time.Now().Before(deadline) {
		if _, ok := rig.reg.Lookup(rig.sbID.Fingerprint()); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	h := rig.server.SandboxControl()
	started, err := h.StartNodeJob(t.Context(), rig.sbID.Fingerprint(), nodejob.StartRequest{
		IdempotencyKey: "run-e2e", SandboxUID: testJobSandbox, Command: []string{"agent"},
	})
	if err != nil || started.ID != "job-e2e" || started.State != nodejob.StateRunning {
		t.Fatalf("StartNodeJob = %+v, %v", started, err)
	}
	got, err := h.GetNodeJob(t.Context(), rig.sbID.Fingerprint(), started.ID, 0, 0, 1024)
	if err != nil || got.State != nodejob.StateSucceeded || !got.ExitCodeSet {
		t.Fatalf("GetNodeJob = %+v, %v", got, err)
	}
	canceled, err := h.CancelNodeJob(t.Context(), rig.sbID.Fingerprint(), started.ID)
	if err != nil || canceled.State != nodejob.StateCanceled || !canceled.CancelCalled {
		t.Fatalf("CancelNodeJob = %+v, %v", canceled, err)
	}
	if err := h.DeleteNodeJob(t.Context(), rig.sbID.Fingerprint(), started.ID); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	deleted := manager.deleted
	manager.mu.Unlock()
	if !deleted {
		t.Fatal("Delete did not reach Node manager")
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatalf("RegisterAndServe: %v", err)
	}
}

// TestRegisterAndServeFailsOnUnreachableOrchestrator covers the
// dial-fail branch of RegisterAndServe (RegisterNode returns an
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
	client, err := nodectrlclient.New(nodectrlclient.Config{
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
	if err := client.RegisterAndServe(ctx, nodectrl.Resources{}); err == nil {
		t.Fatal("RegisterAndServe succeeded against unreachable URL")
	}
}

// TestRegisterAndServePropagatesHeartbeatError covers the sendHeartbeat
// error branch: forcibly evict the registration from the orchestrator's
// registry mid-run; the next Heartbeat tick will return
// FailedPrecondition and RegisterAndServe must surface it.
func TestRegisterAndServePropagatesHeartbeatError(t *testing.T) {
	rig := newE2ERig(t)
	client, err := nodectrlclient.New(nodectrlclient.Config{
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
		served <- client.RegisterAndServe(ctx, nodectrl.Resources{CPUs: 1})
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

// TestRegisterAndServeDeliversSandboxEventToHandler exercises the
// orchestrator → Node SandboxWatchEvent path end-to-end: orchestrator
// pushes ADDED on the stream; the nodectrlclient receives it and
// invokes the configured SandboxEventHandler with the payload.
func TestRegisterAndServeDeliversSandboxEventToHandler(t *testing.T) {
	rig := newE2ERig(t)

	got := make(chan *nodectrlv1.SandboxWatchEvent, 1)
	handler := nodectrlclient.SandboxEventHandlerFunc(func(_ context.Context, evt *nodectrlv1.SandboxWatchEvent) error {
		got <- evt
		return nil
	})

	client, err := nodectrlclient.New(nodectrlclient.Config{
		URL:               rig.url,
		Identity:          rig.sbID,
		Peers:             rig.sbPeer,
		Version:           testFixtureVer,
		HeartbeatInterval: 5 * time.Second,
		SandboxEvents:     handler,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- client.RegisterAndServe(ctx, nodectrl.Resources{CPUs: 1})
	}()

	fp := rig.sbID.Fingerprint()
	want := &sandboxv1.Sandbox{
		Metadata: &sandboxv1.SandboxMetadata{Uid: "sb-1"},
		Spec:     &sandboxv1.SandboxSpec{ImageRef: "alpine:3", WorkspaceUuid: "ws-1"},
	}
	publishUntilAttached(t, rig.server.SandboxControl(), fp, want)

	select {
	case evt := <-got:
		if evt.GetType() != sandboxv1.EventType_EVENT_TYPE_ADDED {
			t.Errorf("type = %s, want ADDED", evt.GetType())
		}
		sb := evt.GetSandbox()
		if sb.GetMetadata().GetUid() != "sb-1" {
			t.Errorf("uid = %q, want sb-1", sb.GetMetadata().GetUid())
		}
		if sb.GetSpec().GetImageRef() != "alpine:3" {
			t.Errorf("image_ref = %q, want alpine:3", sb.GetSpec().GetImageRef())
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

// TestRegisterAndServeLogsSandboxEventError pins consumeStream's
// failure branch for sandbox events: a non-nil return is logged
// to Stderr and the registration stream stays up. Earlier the
// loop tore the stream down on any handler error, but the
// dispatch was switched to a goroutine in D6b.3 so unrelated
// AttachRequest / OpenPort events for other sandboxes keep
// flowing instead of being collateral damage of one sandbox's
// local failure.
func TestRegisterAndServeLogsSandboxEventError(t *testing.T) {
	rig := newE2ERig(t)
	want := errors.New("synthetic handler failure")
	handler := nodectrlclient.SandboxEventHandlerFunc(func(context.Context, *nodectrlv1.SandboxWatchEvent) error {
		return want
	})

	var stderr syncBuffer
	client, err := nodectrlclient.New(nodectrlclient.Config{
		URL:               rig.url,
		Identity:          rig.sbID,
		Peers:             rig.sbPeer,
		Version:           testFixtureVer,
		HeartbeatInterval: 5 * time.Second,
		SandboxEvents:     handler,
		Stderr:            &stderr,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- client.RegisterAndServe(ctx, nodectrl.Resources{CPUs: 1})
	}()

	fp := rig.sbID.Fingerprint()
	publishUntilAttached(t, rig.server.SandboxControl(), fp, &sandboxv1.Sandbox{
		Metadata: &sandboxv1.SandboxMetadata{Uid: "sb-bad"},
		Spec:     &sandboxv1.SandboxSpec{},
	})

	deadline := time.Now().Add(2 * time.Second)
	//daft:lint:disable=forloopbound reason="bounded 2s deadline; exits when stderr contains the error string or via t.Fatalf below"
	for time.Now().Before(deadline) {
		if strings.Contains(stderr.String(), "synthetic handler failure") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(stderr.String(), "synthetic handler failure") {
		t.Fatalf("handler error never logged within 2s; stderr=%q", stderr.String())
	}

	// Stream must NOT have torn down — RegisterAndServe should
	// still be running, ready to dispatch follow-up events.
	select {
	case err := <-served:
		t.Fatalf("RegisterAndServe returned early: %v", err)
	case <-time.After(200 * time.Millisecond):
		// Expected: still running.
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
	client, err := nodectrlclient.New(nodectrlclient.Config{
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
	if err := client.RegisterAndServe(ctx, nodectrl.Resources{}); err == nil {
		t.Fatal("RegisterAndServe succeeded for unapproved peer; want TLS/auth error")
	}
}
