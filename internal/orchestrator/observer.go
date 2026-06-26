package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"connectrpc.com/connect"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/oops"
)

// DefaultOpenObserverHandshakeTimeout caps how long OpenObserverBridge
// waits for the sandboxd to dial ObserverStream after the
// ObserverRequest event has been pushed. Mirrors the attach timeout.
const DefaultOpenObserverHandshakeTimeout = 10 * time.Second

// observerCopyBufSize matches the attach/forward copy buffers: large
// enough to bypass bufio's small-buffer path, small enough that a
// stalled tail doesn't accumulate.
const observerCopyBufSize = 32 * 1024

var (
	// ErrObserverHandleMissing is returned by ObserverTail when the
	// request carries no workspace handle.
	ErrObserverHandleMissing = errors.New("orchestrator: ObserverTail requires a workspace handle")
	// ErrObserverWorkspaceNotRunning is returned by ObserverTail when
	// the addressed workspace has no current_sandbox.
	ErrObserverWorkspaceNotRunning = errors.New("orchestrator: workspace has no current sandbox; not RUNNING")
	// ErrUnknownObserverSession is returned by ObserverStream when the
	// Hello frame's session_id matches no pending bridge request.
	ErrUnknownObserverSession = errors.New("orchestrator: unknown Observer session")
	// ErrObserverHelloMissingSession is returned by ObserverStream when
	// the first frame isn't a Hello with a non-empty session_id.
	ErrObserverHelloMissingSession = errors.New("orchestrator: ObserverStream first frame must be Hello with session_id")
)

// WorkspaceObserverBridger is the seam ObserverTail uses to open a
// read-only observer-log stream from the Sandbox bound to a
// Workspace. The production implementation is *NodeControlHandler
// (its OpenObserverBridge method); tests inject fakes returning a
// canned io.ReadCloser.
type WorkspaceObserverBridger interface {
	OpenObserverBridge(ctx context.Context, fp identity.Fingerprint, sandboxUID string, source nodectrlv1.ObserverSource, follow bool) (io.ReadCloser, error)
}

// SetObserverBridger wires the WorkspaceObserverBridger ObserverTail
// calls route to. Nil ⇒ ObserverTail returns Unimplemented.
func (h *WorkspaceCatalogHandler) SetObserverBridger(b WorkspaceObserverBridger) {
	h.observerBridger = b
}

// observerChunkSink is the subset of the server-stream surface
// ObserverTail writes to. Isolated for tests.
type observerChunkSink interface {
	Send(*workspacev1.ObserverChunk) error
}

var _ observerChunkSink = (*connect.ServerStream[workspacev1.ObserverChunk])(nil)

// ObserverTail is the orchestrator-side handler for
// WorkspaceCatalogService.ObserverTail. It resolves the workspace +
// RUNNING gate, opens a sandboxd-side ObserverStream via
// OpenObserverBridge, and forwards the log chunks to the client until
// the stream ends (dump) or the client disconnects (follow).
func (h *WorkspaceCatalogHandler) ObserverTail(
	ctx context.Context,
	req *connect.Request[workspacev1.ObserverTailRequest],
	stream *connect.ServerStream[workspacev1.ObserverChunk],
) error {
	fp, ok := PeerFingerprintFromRequestHeader(req.Header())
	if !ok {
		return errUnauthenticated
	}
	return h.runObserverTail(ctx, fp, req.Msg, stream)
}

// runObserverTail is the testable core, parameterised over the narrow
// chunk sink + caller fingerprint.
func (h *WorkspaceCatalogHandler) runObserverTail(
	ctx context.Context,
	callerFP identity.Fingerprint,
	req *workspacev1.ObserverTailRequest,
	sink observerChunkSink,
) error {
	if h.observerBridger == nil {
		return connect.NewError(connect.CodeUnimplemented,
			errors.New("orchestrator: ObserverTail: no ObserverBridger configured"))
	}
	if req.GetWorkspace() == nil {
		return connect.NewError(connect.CodeInvalidArgument, ErrObserverHandleMissing)
	}
	uid, nodeFP, err := h.resolveRunningSandbox(callerFP, req.GetWorkspace())
	if err != nil {
		return err
	}
	reader, err := h.observerBridger.OpenObserverBridge(ctx, nodeFP, uid,
		nodectrlv1.ObserverSource_OBSERVER_SOURCE_LOG, req.GetFollow())
	if err != nil {
		return mapCatalogErr(err)
	}
	defer func() { oops.Skip("orchestrator: close observer reader", reader.Close(), net.ErrClosed) }()
	return pumpObserverToClient(reader, sink)
}

// resolveRunningSandbox resolves a workspace handle for the caller and
// returns its current sandbox's UID + node fingerprint, or a connect
// error: NotFound when the handle doesn't resolve, FailedPrecondition
// when the workspace isn't RUNNING (no current_sandbox). Shared by
// ObserverTail and ObserveSummary, which both target the live sandbox
// behind a workspace. Callers reject a nil handle first (the
// InvalidArgument sentinel is per-RPC).
func (h *WorkspaceCatalogHandler) resolveRunningSandbox(
	callerFP identity.Fingerprint,
	handle *workspacev1.WorkspaceHandle,
) (sandboxUID string, nodeFP identity.Fingerprint, err error) {
	ws, err := h.resolveHandle(callerFP, handle)
	if err != nil {
		return "", identity.Fingerprint{}, err
	}
	if ws.Status.CurrentSandbox == nil || ws.Status.CurrentSandbox.UID == "" {
		return "", identity.Fingerprint{}, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%w: workspace=%s", ErrObserverWorkspaceNotRunning, ws.Metadata.UUID))
	}
	return ws.Status.CurrentSandbox.UID, ws.Status.CurrentSandbox.NodeFingerprint, nil
}

// pumpObserverToClient reads observer-log bytes from the node-side
// reader and forwards each chunk as an ObserverChunk on the client
// stream. The per-Read append-copy is required because protobuf
// marshals []byte by reference. EOF (and expected pump exits) end the
// stream cleanly.
func pumpObserverToClient(reader io.Reader, sink observerChunkSink) error {
	buf := make([]byte, observerCopyBufSize)
	//daft:lint:disable=forloopbound reason="exits when reader.Read returns err/EOF (node CloseRequest or cross-cancel close)"
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			if sendErr := sink.Send(&workspacev1.ObserverChunk{Data: append([]byte(nil), buf[:n]...)}); sendErr != nil {
				return sendErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) || oops.IsExpectedPumpExit(err) {
				return nil
			}
			return err
		}
	}
}

// pendingObserver is the rendezvous slot OpenObserverBridge parks on
// while waiting for the matching sandboxd-initiated ObserverStream.
type pendingObserver struct {
	arrived chan observerStreamArrival
}

type observerStreamArrival struct {
	stream  serverObserverStream
	release chan struct{}
}

// serverObserverStream is the subset of the server-side connect bidi
// stream this package consumes. Isolated for tests.
type serverObserverStream interface {
	Send(*nodectrlv1.ObserverStreamMessage) error
	Receive() (*nodectrlv1.ObserverStreamMessage, error)
}

var _ serverObserverStream = (*connect.BidiStream[nodectrlv1.ObserverStreamMessage, nodectrlv1.ObserverStreamMessage])(nil)

// OpenObserverBridge mints a session, pushes an ObserverRequest event
// on the addressed Node's RegisterNode stream, waits for the sandboxd
// to dial ObserverStream with the matching session_id, and returns an
// io.ReadCloser yielding the observer-log bytes. Closing it tears the
// ObserverStream down (stops a follow tail on the Node).
func (h *NodeControlHandler) OpenObserverBridge(
	ctx context.Context,
	fp identity.Fingerprint,
	sandboxUID string,
	source nodectrlv1.ObserverSource,
	follow bool,
) (io.ReadCloser, error) {
	if sandboxUID == "" {
		return nil, errors.New("orchestrator: OpenObserverBridge: empty sandboxUID")
	}
	sessionID, err := mintSessionID()
	if err != nil {
		return nil, fmt.Errorf("orchestrator: mint session id: %w", err)
	}
	pending := &pendingObserver{arrived: make(chan observerStreamArrival, 1)}
	h.mu.Lock()
	h.pendingObservers[sessionID] = pending
	h.mu.Unlock()
	handedOff := false
	defer func() {
		if !handedOff {
			h.mu.Lock()
			delete(h.pendingObservers, sessionID)
			h.mu.Unlock()
		}
	}()

	if err := h.pushObserverRequest(fp, sessionID, sandboxUID, source, follow); err != nil {
		return nil, err
	}

	waitCtx, cancel := waitContextWithFallback(ctx, DefaultOpenObserverHandshakeTimeout)
	defer cancel()
	select {
	case arrival := <-pending.arrived:
		handedOff = true
		return newServerObserverReader(arrival.stream, arrival.release), nil
	case <-waitCtx.Done():
		return nil, fmt.Errorf("orchestrator: wait for ObserverStream session=%s: %w", sessionID, waitCtx.Err())
	}
}

// pushObserverRequest emits the ObserverRequest event on the Node's
// RegisterNode stream channel. Same backlog semantics as
// pushAttachRequest.
func (h *NodeControlHandler) pushObserverRequest(
	fp identity.Fingerprint,
	sessionID, sandboxUID string,
	source nodectrlv1.ObserverSource,
	follow bool,
) error {
	h.mu.Lock()
	ch, ok := h.streams[fp]
	h.mu.Unlock()
	if !ok {
		return ErrNodeNotConnected
	}
	ev := &nodectrlv1.NodeEvent{
		Event: &nodectrlv1.NodeEvent_Observer{
			Observer: &nodectrlv1.ObserverRequest{
				SessionId:  sessionID,
				SandboxUid: sandboxUID,
				Follow:     follow,
				Source:     source,
			},
		},
	}
	select {
	case ch <- ev:
		return nil
	default:
		return ErrSandboxEventBacklog
	}
}

// ObserverStream is the orchestrator-side handler for the bidi RPC the
// sandboxd dials in response to an ObserverRequest event. Mirrors
// AttachStream: read Hello → look up the pending rendezvous → hand off
// → block on release → return.
func (h *NodeControlHandler) ObserverStream(
	ctx context.Context,
	stream *connect.BidiStream[nodectrlv1.ObserverStreamMessage, nodectrlv1.ObserverStreamMessage],
) error {
	return h.runObserverStream(ctx, stream)
}

//nolint:dupl // structurally parallel to runAttachStream/runOpenPortStream; the proto types differ (ObserverStreamMessage) and merging would need generics that obscure type checking. Side-by-side review is the intent.
func (h *NodeControlHandler) runObserverStream(ctx context.Context, stream serverObserverStream) error {
	msg, err := stream.Receive()
	if err != nil {
		return fmt.Errorf("orchestrator: ObserverStream first Receive: %w", err)
	}
	hello := msg.GetHello()
	if hello == nil || hello.GetSessionId() == "" {
		return connect.NewError(connect.CodeInvalidArgument, ErrObserverHelloMissingSession)
	}
	sessionID := hello.GetSessionId()
	h.mu.Lock()
	pending, ok := h.pendingObservers[sessionID]
	if ok {
		delete(h.pendingObservers, sessionID)
	}
	h.mu.Unlock()
	if !ok {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%w: %s", ErrUnknownObserverSession, sessionID))
	}
	release := make(chan struct{})
	pending.arrived <- observerStreamArrival{stream: stream, release: release}
	select {
	case <-release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// serverObserverReader adapts a serverObserverStream as an
// io.ReadCloser: Read pulls data frames from the node stream; Close
// signals the ObserverStream handler to return (stopping a follow
// tail on the Node). Read-only — there is no Write half.
type serverObserverReader struct {
	stream  serverObserverStream
	release chan struct{}

	closeOnce sync.Once
	readMu    sync.Mutex
	readBuf   []byte
}

func newServerObserverReader(stream serverObserverStream, release chan struct{}) *serverObserverReader {
	return &serverObserverReader{stream: stream, release: release}
}

func (r *serverObserverReader) Read(p []byte) (int, error) {
	r.readMu.Lock()
	defer r.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.readBuf) == 0 {
		msg, err := r.stream.Receive()
		if err != nil {
			return 0, err
		}
		data := msg.GetData()
		if len(data) == 0 {
			continue
		}
		r.readBuf = data
	}
	n := copy(p, r.readBuf)
	r.readBuf = r.readBuf[n:]
	return n, nil
}

func (r *serverObserverReader) Close() error {
	r.closeOnce.Do(func() { close(r.release) })
	return nil
}
