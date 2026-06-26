package orchestrator

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

const testObserverSandboxUID = "sb-observer-1"

// ---- fakes ----

type fakeObserverBridger struct {
	rc         io.ReadCloser
	err        error
	lastUID    string
	lastFollow bool
	lastSource nodectrlv1.ObserverSource
}

func (b *fakeObserverBridger) OpenObserverBridge(_ context.Context, _ identity.Fingerprint, uid string, source nodectrlv1.ObserverSource, follow bool) (io.ReadCloser, error) {
	b.lastUID = uid
	b.lastFollow = follow
	b.lastSource = source
	if b.err != nil {
		return nil, b.err
	}
	return b.rc, nil
}

type fakeChunkSink struct {
	mu  sync.Mutex
	buf strings.Builder
	err error
}

func (s *fakeChunkSink) Send(c *workspacev1.ObserverChunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.buf.Write(c.GetData())
	return nil
}

func (s *fakeChunkSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// stringReadCloser wraps a strings.Reader with a closed flag.
type stringReadCloser struct {
	r      *strings.Reader
	closed bool
}

func newStringReadCloser(s string) *stringReadCloser {
	return &stringReadCloser{r: strings.NewReader(s)}
}
func (c *stringReadCloser) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *stringReadCloser) Close() error               { c.closed = true; return nil }

type fakeObserverServerStream struct {
	mu       sync.Mutex
	incoming chan *nodectrlv1.ObserverStreamMessage
	outgoing []*nodectrlv1.ObserverStreamMessage
	recvErr  error
}

func newFakeObserverServerStream() *fakeObserverServerStream {
	return &fakeObserverServerStream{incoming: make(chan *nodectrlv1.ObserverStreamMessage, 8)}
}

func (s *fakeObserverServerStream) Send(m *nodectrlv1.ObserverStreamMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outgoing = append(s.outgoing, m)
	return nil
}

func (s *fakeObserverServerStream) Receive() (*nodectrlv1.ObserverStreamMessage, error) {
	s.mu.Lock()
	err := s.recvErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	m, ok := <-s.incoming
	if !ok {
		return nil, io.EOF
	}
	return m, nil
}

func (s *fakeObserverServerStream) push(m *nodectrlv1.ObserverStreamMessage) { s.incoming <- m }

func observerHelloMsg(sessionID string) *nodectrlv1.ObserverStreamMessage {
	return &nodectrlv1.ObserverStreamMessage{
		Body: &nodectrlv1.ObserverStreamMessage_Hello{Hello: &nodectrlv1.ObserverStreamHello{SessionId: sessionID}},
	}
}

func observerDataMsg(data string) *nodectrlv1.ObserverStreamMessage {
	return &nodectrlv1.ObserverStreamMessage{
		Body: &nodectrlv1.ObserverStreamMessage_Data{Data: []byte(data)},
	}
}

func newObserverTestHandler(t *testing.T) (*NodeControlHandler, identity.Fingerprint, chan *nodectrlv1.NodeEvent) {
	t.Helper()
	reg := nodectrl.NewRegistry(time.Now)
	h, err := NewNodeControlHandler(reg)
	if err != nil {
		t.Fatalf("NewNodeControlHandler: %v", err)
	}
	fp := identity.Fingerprint{0x11, 0x22}
	events := h.attachStream(fp)
	t.Cleanup(func() { h.detachStream(fp) })
	return h, fp, events
}

func newObserverTestHandlerWithWorkspace(t *testing.T, br WorkspaceObserverBridger) (*WorkspaceCatalogHandler, string, identity.Fingerprint) {
	t.Helper()
	cat := workspacecatalog.NewStore(nil)
	h, err := NewWorkspaceCatalogHandler(cat, nil)
	if err != nil {
		t.Fatalf("NewWorkspaceCatalogHandler: %v", err)
	}
	h.SetObserverBridger(br)
	owner := identity.Fingerprint{0xaa, 0xbb}
	uid := "ws-uuid-observer"
	if _, err := cat.Create(workspacecatalog.CreateInput{
		UUID: uid, Name: "observer-ws", OwnerFingerprint: owner,
	}); err != nil {
		t.Fatalf("catalog.Create: %v", err)
	}
	if _, err := cat.Update(uid, workspacecatalog.MutateStatus,
		func(ws *workspacecatalog.Workspace) error {
			ws.Status.CurrentSandbox = &workspacecatalog.SandboxRef{
				UID:             testObserverSandboxUID,
				NodeFingerprint: identity.Fingerprint{0xcc},
			}
			return nil
		}); err != nil {
		t.Fatalf("catalog.Update: %v", err)
	}
	return h, uid, owner
}

func uuidHandleReq(uuid string, follow bool) *workspacev1.ObserverTailRequest {
	return &workspacev1.ObserverTailRequest{
		Workspace: &workspacev1.WorkspaceHandle{Handle: &workspacev1.WorkspaceHandle_Uuid{Uuid: uuid}},
		Follow:    follow,
	}
}

// ---- WorkspaceCatalogHandler.runObserverTail ----

func TestRunObserverTailStreamsChunks(t *testing.T) {
	rc := newStringReadCloser("line-a\nline-b\n")
	br := &fakeObserverBridger{rc: rc}
	h, uid, owner := newObserverTestHandlerWithWorkspace(t, br)
	sink := &fakeChunkSink{}
	if err := h.runObserverTail(context.Background(), owner, uuidHandleReq(uid, true), sink); err != nil {
		t.Fatalf("runObserverTail: %v", err)
	}
	if sink.String() != "line-a\nline-b\n" {
		t.Errorf("chunks = %q, want the full log", sink.String())
	}
	if br.lastUID != testObserverSandboxUID || !br.lastFollow {
		t.Errorf("bridger args uid=%q follow=%v", br.lastUID, br.lastFollow)
	}
	if !rc.closed {
		t.Error("reader was not closed")
	}
}

func TestRunObserverTailNoBridger(t *testing.T) {
	cat := workspacecatalog.NewStore(nil)
	h, _ := NewWorkspaceCatalogHandler(cat, nil)
	err := h.runObserverTail(context.Background(), identity.Fingerprint{}, uuidHandleReq("x", false), &fakeChunkSink{})
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("err code = %v, want Unimplemented", connect.CodeOf(err))
	}
}

func TestRunObserverTailMissingHandle(t *testing.T) {
	h, _, owner := newObserverTestHandlerWithWorkspace(t, &fakeObserverBridger{})
	err := h.runObserverTail(context.Background(), owner, &workspacev1.ObserverTailRequest{}, &fakeChunkSink{})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("err code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestRunObserverTailNotRunning(t *testing.T) {
	cat := workspacecatalog.NewStore(nil)
	h, _ := NewWorkspaceCatalogHandler(cat, nil)
	h.SetObserverBridger(&fakeObserverBridger{})
	owner := identity.Fingerprint{0xaa}
	uid := "ws-idle"
	if _, err := cat.Create(workspacecatalog.CreateInput{UUID: uid, Name: "idle", OwnerFingerprint: owner}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	err := h.runObserverTail(context.Background(), owner, uuidHandleReq(uid, false), &fakeChunkSink{})
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("err code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}

func TestRunObserverTailBridgerError(t *testing.T) {
	br := &fakeObserverBridger{err: errors.New("synthetic bridge fail")}
	h, uid, owner := newObserverTestHandlerWithWorkspace(t, br)
	if err := h.runObserverTail(context.Background(), owner, uuidHandleReq(uid, false), &fakeChunkSink{}); err == nil {
		t.Fatal("expected bridger error")
	}
}

func TestPumpObserverToClientSendError(t *testing.T) {
	sink := &fakeChunkSink{err: errors.New("send broke")}
	err := pumpObserverToClient(strings.NewReader("data"), sink)
	if err == nil {
		t.Fatal("expected send error to propagate")
	}
}

// TestPumpObserverToClientExpectedExit covers the clean-stop branch: a
// reader closing with an expected pump-exit error ends the stream
// without surfacing an error.
func TestPumpObserverToClientExpectedExit(t *testing.T) {
	if err := pumpObserverToClient(errReader{io.ErrClosedPipe}, &fakeChunkSink{}); err != nil {
		t.Errorf("pumpObserverToClient = %v, want nil for an expected pump exit", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// TestRunObserverTailUnknownWorkspace covers the resolveHandle error
// branch: an unknown handle surfaces NotFound before any bridge opens.
func TestRunObserverTailUnknownWorkspace(t *testing.T) {
	h, _, owner := newObserverTestHandlerWithWorkspace(t, &fakeObserverBridger{})
	err := h.runObserverTail(context.Background(), owner, uuidHandleReq("no-such-uuid", false), &fakeChunkSink{})
	if err == nil {
		t.Fatal("expected an error resolving an unknown workspace")
	}
}

// ---- NodeControlHandler rendezvous ----

func TestOpenObserverBridgePushesEventThenStreams(t *testing.T) {
	h, fp, events := newObserverTestHandler(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	type res struct {
		rc  io.ReadCloser
		err error
	}
	out := make(chan res, 1)
	go func() {
		rc, err := h.OpenObserverBridge(ctx, fp, testObserverSandboxUID,
			nodectrlv1.ObserverSource_OBSERVER_SOURCE_LOG, true)
		out <- res{rc, err}
	}()

	var sessionID string
	select {
	case ev := <-events:
		req := ev.GetObserver()
		if req == nil {
			t.Fatalf("event is not Observer: %+v", ev)
		}
		if req.GetSandboxUid() != testObserverSandboxUID || !req.GetFollow() {
			t.Errorf("event uid=%q follow=%v", req.GetSandboxUid(), req.GetFollow())
		}
		sessionID = req.GetSessionId()
		if sessionID == "" {
			t.Fatal("empty session_id")
		}
	case <-time.After(time.Second):
		t.Fatal("observer event not pushed")
	}

	stream := newFakeObserverServerStream()
	stream.push(observerHelloMsg(sessionID))
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	streamDone := make(chan error, 1)
	go func() { streamDone <- h.runObserverStream(streamCtx, stream) }()

	r := <-out
	if r.err != nil {
		t.Fatalf("OpenObserverBridge: %v", r.err)
	}
	stream.push(observerDataMsg("from-vm"))
	got := make([]byte, len("from-vm"))
	if _, err := io.ReadFull(r.rc, got); err != nil {
		t.Fatalf("reader.Read: %v", err)
	}
	if string(got) != "from-vm" {
		t.Errorf("read = %q, want from-vm", got)
	}
	if err := r.rc.Close(); err != nil {
		t.Fatalf("reader.Close: %v", err)
	}
	select {
	case err := <-streamDone:
		if err != nil {
			t.Errorf("runObserverStream: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runObserverStream did not return after Close")
	}
}

func TestOpenObserverBridgeRejectsEmptySandbox(t *testing.T) {
	h, fp, _ := newObserverTestHandler(t)
	if _, err := h.OpenObserverBridge(context.Background(), fp, "",
		nodectrlv1.ObserverSource_OBSERVER_SOURCE_LOG, false); err == nil {
		t.Error("expected error for empty sandboxUID")
	}
}

func TestOpenObserverBridgeNodeNotConnected(t *testing.T) {
	reg := nodectrl.NewRegistry(time.Now)
	h, _ := NewNodeControlHandler(reg)
	_, err := h.OpenObserverBridge(context.Background(), identity.Fingerprint{0xff}, "sb",
		nodectrlv1.ObserverSource_OBSERVER_SOURCE_LOG, false)
	if !errors.Is(err, ErrNodeNotConnected) {
		t.Errorf("err = %v, want ErrNodeNotConnected", err)
	}
}

func TestOpenObserverBridgeTimesOut(t *testing.T) {
	h, fp, _ := newObserverTestHandler(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	// No node dials ObserverStream, so the bridge times out on ctx.
	if _, err := h.OpenObserverBridge(ctx, fp, "sb",
		nodectrlv1.ObserverSource_OBSERVER_SOURCE_LOG, false); err == nil {
		t.Error("expected timeout error")
	}
}

func TestRunObserverStreamRejectsBadHello(t *testing.T) {
	h, _, _ := newObserverTestHandler(t)
	stream := newFakeObserverServerStream()
	stream.push(observerDataMsg("not-a-hello"))
	err := h.runObserverStream(context.Background(), stream)
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("err code = %v, want InvalidArgument", connect.CodeOf(err))
	}
}

func TestRunObserverStreamUnknownSession(t *testing.T) {
	h, _, _ := newObserverTestHandler(t)
	stream := newFakeObserverServerStream()
	stream.push(observerHelloMsg("nonexistent"))
	err := h.runObserverStream(context.Background(), stream)
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("err code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}

// TestObserverTailWrapperUnauthenticated covers the connect-handler
// auth gate: a request with no peer fingerprint is rejected before the
// stream is touched.
func TestObserverTailWrapperUnauthenticated(t *testing.T) {
	h, _ := NewWorkspaceCatalogHandler(workspacecatalog.NewStore(nil), nil)
	err := h.ObserverTail(context.Background(),
		connect.NewRequest(uuidHandleReq("ws", false)), nil)
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("err code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}

// TestObserverTailWrapperReachesHandler covers the auth-pass delegate:
// with a valid peer header but no bridger configured, the handler
// surfaces Unimplemented (before any stream use, so a nil sink is
// safe).
func TestObserverTailWrapperReachesHandler(t *testing.T) {
	h, _ := NewWorkspaceCatalogHandler(workspacecatalog.NewStore(nil), nil)
	req := connect.NewRequest(uuidHandleReq("ws", false))
	req.Header().Set(peerFingerprintHeader, (identity.Fingerprint{0x42}).String())
	if err := h.ObserverTail(context.Background(), req, nil); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("err code = %v, want Unimplemented", connect.CodeOf(err))
	}
}

func TestOpenObserverBridgeBacklog(t *testing.T) {
	h, fp, events := newObserverTestHandler(t)
	for range cap(events) {
		events <- &nodectrlv1.NodeEvent{}
	}
	_, err := h.OpenObserverBridge(context.Background(), fp, "sb",
		nodectrlv1.ObserverSource_OBSERVER_SOURCE_LOG, false)
	if !errors.Is(err, ErrSandboxEventBacklog) {
		t.Errorf("err = %v, want ErrSandboxEventBacklog", err)
	}
}

func TestRunObserverStreamFirstReceiveError(t *testing.T) {
	h, _, _ := newObserverTestHandler(t)
	stream := newFakeObserverServerStream()
	stream.recvErr = errors.New("recv broke")
	if err := h.runObserverStream(context.Background(), stream); err == nil {
		t.Error("expected first-receive error")
	}
}

// ---- serverObserverReader ----

func TestServerObserverReaderSkipsEmptyAndCloses(t *testing.T) {
	stream := newFakeObserverServerStream()
	stream.push(observerDataMsg(""))     // skipped
	stream.push(observerHelloMsg("x"))   // non-data, skipped
	stream.push(observerDataMsg("real")) // delivered
	r := newServerObserverReader(stream, make(chan struct{}))
	got := make([]byte, 4)
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "real" {
		t.Errorf("read = %q, want real", got)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = r.Close() // idempotent
}

func TestServerObserverReaderReceiveError(t *testing.T) {
	stream := newFakeObserverServerStream()
	stream.recvErr = errors.New("boom")
	r := newServerObserverReader(stream, make(chan struct{}))
	if _, err := r.Read(make([]byte, 4)); err == nil {
		t.Error("expected receive error from Read")
	}
}

func TestServerObserverReaderZeroLenRead(t *testing.T) {
	r := newServerObserverReader(newFakeObserverServerStream(), make(chan struct{}))
	if n, err := r.Read(nil); n != 0 || err != nil {
		t.Errorf("Read(nil) = (%d,%v), want (0,nil)", n, err)
	}
}
