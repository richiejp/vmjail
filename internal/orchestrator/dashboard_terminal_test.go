package orchestrator

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

type fakeTerminalSocket struct {
	mu       sync.Mutex
	incoming [][]byte
	sent     [][]byte
	err      error
	closed   bool
	deadline time.Time
}

type blockingTerminalSocket struct {
	fakeTerminalSocket
	closedOnce sync.Once
	done       chan struct{}
}

type dataAndErrorReader struct{ used bool }

func (r *dataAndErrorReader) Read(buffer []byte) (int, error) {
	if r.used {
		return 0, io.EOF
	}
	r.used = true
	return copy(buffer, "last"), io.ErrUnexpectedEOF
}

func (s *blockingTerminalSocket) Receive(*[]byte) error {
	<-s.done
	return io.EOF
}

func (s *blockingTerminalSocket) Close() error {
	s.closedOnce.Do(func() { close(s.done) })
	return s.fakeTerminalSocket.Close()
}

func (s *fakeTerminalSocket) Receive(frame *[]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.incoming) == 0 {
		if s.err != nil {
			return s.err
		}
		return io.EOF
	}
	*frame = append((*frame)[:0], s.incoming[0]...)
	s.incoming = s.incoming[1:]
	return nil
}

func (s *fakeTerminalSocket) Send(frame []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, append([]byte(nil), frame...))
	return nil
}

func (s *fakeTerminalSocket) SetReadDeadline(deadline time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadline = deadline
	return nil
}

func (s *fakeTerminalSocket) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func TestTerminalDimensions(t *testing.T) {
	request := httptest.NewRequest("GET", "/?cols=132&rows=43", http.NoBody)
	cols, rows, err := terminalDimensions(request)
	if err != nil || cols != 132 || rows != 43 {
		t.Fatalf("dimensions = %d,%d,%v", cols, rows, err)
	}
	request = httptest.NewRequest("GET", "/", http.NoBody)
	cols, rows, err = terminalDimensions(request)
	if err != nil || cols != 80 || rows != 24 {
		t.Fatalf("defaults = %d,%d,%v", cols, rows, err)
	}
	for _, query := range []string{"cols=0", "rows=nope", "cols=65536"} {
		if _, _, err := terminalDimensions(httptest.NewRequest("GET", "/?"+query, http.NoBody)); err == nil {
			t.Fatalf("%q accepted", query)
		}
	}
}

func TestDashboardTerminalResolveLifecycle(t *testing.T) {
	store := workspacecatalog.NewStore(nil)
	createDashboardWorkspace(t, store, "inactive", "inactive", dashboardFingerprint(t, 'b'), false)
	terminal := &dashboardTerminal{workspaces: store}
	if _, err := terminal.resolve("missing"); err == nil {
		t.Fatal("missing workspace resolved")
	}
	if _, err := terminal.resolve("inactive"); !errors.Is(err, ErrAttachWorkspaceNotRunning) {
		t.Fatalf("inactive error = %v", err)
	}
	createDashboardWorkspace(t, store, "active", "active", dashboardFingerprint(t, 'c'), true)
	_, err := store.Update("active", workspacecatalog.MutateStatus, func(value *workspacecatalog.Workspace) error {
		value.Status.ObservedGeneration = value.Metadata.Generation
		value.Status.CurrentSandbox.WorkspaceGeneration = value.Metadata.Generation
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := terminal.resolve("active")
	if err != nil || workspace.Metadata.UUID != "active" {
		t.Fatalf("active resolve = %#v, %v", workspace, err)
	}
	_, err = store.Update("active", workspacecatalog.MutateSpec, func(*workspacecatalog.Workspace) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := terminal.resolve("active"); !errors.Is(err, ErrWorkspaceGenerationPending) {
		t.Fatalf("stale generation error = %v", err)
	}
}

func TestDashboardTerminalHTTPGuardsAndBridgeFailure(t *testing.T) {
	store := workspacecatalog.NewStore(nil)
	node := dashboardFingerprint(t, 'e')
	createDashboardWorkspace(t, store, "inactive-guard", "inactive-guard", node, false)
	createDashboardWorkspace(t, store, "active-guard", "active-guard", node, true)
	_, err := store.Update("active-guard", workspacecatalog.MutateStatus, func(value *workspacecatalog.Workspace) error {
		value.Status.ObservedGeneration = value.Metadata.Generation
		value.Status.CurrentSandbox.WorkspaceGeneration = value.Metadata.Generation
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	registerDashboardTerminalRoutes(mux, nil)
	registerDashboardTerminalRoutes(mux, &dashboardTerminal{workspaces: store})
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest("GET", "/terminal/active-guard", http.NoBody))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("disabled terminal status = %d", recorder.Code)
	}

	terminal := &dashboardTerminal{workspaces: store, bridger: &fakeAttachBridger{err: errors.New("node unavailable")}}
	mux = http.NewServeMux()
	registerDashboardTerminalRoutes(mux, terminal)
	for path, want := range map[string]int{
		"/terminal/inactive-guard":             http.StatusPreconditionFailed,
		"/terminal/active-guard/socket?cols=0": http.StatusBadRequest,
	} {
		recorder = httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest("GET", path, http.NoBody))
		if recorder.Code != want {
			t.Errorf("%s status=%d want=%d", path, recorder.Code, want)
		}
	}
	workspace, err := store.Get("active-guard")
	if err != nil {
		t.Fatal(err)
	}
	socket := &fakeTerminalSocket{}
	terminal.serveWebSocket(context.Background(), socket, workspace, 80, 24)
	if len(socket.sent) != 1 || socket.sent[0][0] != terminalFrameRetry || !strings.Contains(string(socket.sent[0][1:]), "node unavailable") {
		t.Fatalf("bridge failure frames = %q", socket.sent)
	}
}

func TestBrowserToAttachDataResizeHeartbeatAndValidation(t *testing.T) {
	local, remote := net.Pipe()
	defer func() { _ = local.Close() }()
	defer func() { _ = remote.Close() }()
	resize := make([]byte, 9)
	resize[0] = terminalFrameResize
	binary.BigEndian.PutUint32(resize[1:5], 120)
	binary.BigEndian.PutUint32(resize[5:9], 40)
	socket := &fakeTerminalSocket{incoming: [][]byte{{terminalFrameHeartbeat}, resize, append([]byte{terminalFrameData}, []byte("input")...)}}
	session := &fakeAttachSession{Conn: local}
	read := make(chan string, 1)
	go func() {
		buffer := make([]byte, 5)
		_, _ = io.ReadFull(remote, buffer)
		read <- string(buffer)
	}()
	err := browserToAttach(socket, session)
	if !errors.Is(err, io.EOF) || <-read != "input" {
		t.Fatalf("relay = %v", err)
	}
	resizes := session.sentResizes()
	if len(resizes) != 1 || resizes[0].cols != 120 || resizes[0].rows != 40 || socket.deadline.IsZero() {
		t.Fatalf("resizes=%v deadline=%v", resizes, socket.deadline)
	}

	zeroResize := make([]byte, 9)
	zeroResize[0] = terminalFrameResize
	badFrames := [][]byte{{}, {9}, {terminalFrameResize, 1}, zeroResize, {terminalFrameHeartbeat, 1}}
	for _, frame := range badFrames {
		if err := browserToAttach(&fakeTerminalSocket{incoming: [][]byte{frame}}, session); err == nil {
			t.Fatalf("bad frame %v accepted", frame)
		}
	}
	resizeSocket := &fakeTerminalSocket{incoming: [][]byte{resize}}
	if err := browserToAttach(resizeSocket, &fakeAttachSession{Conn: local, rerr: errors.New("resize failed")}); err == nil {
		t.Fatal("resize failure ignored")
	}
	if err := browserToAttach(&fakeTerminalSocket{incoming: [][]byte{{terminalFrameData}}}, session); !errors.Is(err, io.EOF) {
		t.Fatalf("empty data frame result = %v", err)
	}
	closedLocal, closedRemote := net.Pipe()
	_ = closedRemote.Close()
	defer func() { _ = closedLocal.Close() }()
	if err := browserToAttach(&fakeTerminalSocket{incoming: [][]byte{{terminalFrameData, 'x'}}}, &fakeAttachSession{Conn: closedLocal}); err == nil {
		t.Fatal("PTY write failure ignored")
	}
}

func TestAttachToBrowserAndExitFrame(t *testing.T) {
	socket := &fakeTerminalSocket{}
	if err := attachToBrowser(socket, strings.NewReader("output")); !errors.Is(err, io.EOF) {
		t.Fatalf("attachToBrowser = %v", err)
	}
	if len(socket.sent) != 1 || socket.sent[0][0] != terminalFrameData || string(socket.sent[0][1:]) != "output" {
		t.Fatalf("sent = %q", socket.sent)
	}
	if err := sendTerminalExit(socket, 17, strings.Repeat("x", 2048)); err != nil {
		t.Fatal(err)
	}
	exit := socket.sent[1]
	if exit[0] != terminalFrameExit || binary.BigEndian.Uint32(exit[1:5]) != 17 || len(exit) != 5+1024 {
		t.Fatalf("exit frame len=%d code=%d", len(exit), binary.BigEndian.Uint32(exit[1:5]))
	}
	socket = &fakeTerminalSocket{}
	err := attachToBrowser(socket, &terminalAttachSession{exit: &sandboxgateway.AttachExitError{Code: 9, Reason: "terminal failure"}})
	if !errors.Is(err, io.EOF) || len(socket.sent) != 1 || socket.sent[0][0] != terminalFrameExit || binary.BigEndian.Uint32(socket.sent[0][1:5]) != 9 {
		t.Fatalf("terminal exit relay err=%v frames=%v", err, socket.sent)
	}
	socket = &fakeTerminalSocket{}
	if err := attachToBrowser(socket, &dataAndErrorReader{}); !errors.Is(err, io.ErrUnexpectedEOF) || len(socket.sent) != 1 || string(socket.sent[0][1:]) != "last" {
		t.Fatalf("data plus error relay err=%v frames=%v", err, socket.sent)
	}
	sendFailure := errors.New("browser send failed")
	if err := attachToBrowser(&fakeTerminalSocket{err: sendFailure}, strings.NewReader("output")); !errors.Is(err, sendFailure) {
		t.Fatalf("browser send failure = %v", err)
	}
	if err := sendTerminalExit(&fakeTerminalSocket{err: sendFailure}, 1, "reason"); !errors.Is(err, sendFailure) {
		t.Fatalf("exit send failure = %v", err)
	}
	if err := sendTerminalRetry(&fakeTerminalSocket{err: sendFailure}, strings.Repeat("reason", 300)); !errors.Is(err, sendFailure) {
		t.Fatalf("retry send failure = %v", err)
	}
}

func TestServeWebSocketPreservesGuestExit(t *testing.T) {
	store := workspacecatalog.NewStore(nil)
	node := dashboardFingerprint(t, 'f')
	createDashboardWorkspace(t, store, "exit", "exit", node, true)
	_, err := store.Update("exit", workspacecatalog.MutateStatus, func(value *workspacecatalog.Workspace) error {
		value.Status.ObservedGeneration = value.Metadata.Generation
		value.Status.CurrentSandbox.WorkspaceGeneration = value.Metadata.Generation
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.Get("exit")
	if err != nil {
		t.Fatal(err)
	}
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = local.Close() })
	t.Cleanup(func() { _ = remote.Close() })
	session := &terminalAttachSession{Conn: local, exit: &sandboxgateway.AttachExitError{Code: 23, Reason: "guest exited"}}
	terminal := &dashboardTerminal{workspaces: store, bridger: &fakeAttachBridger{sessions: []sandboxgateway.AttachSession{session}}}
	socket := &blockingTerminalSocket{done: make(chan struct{})}
	terminal.serveWebSocket(context.Background(), socket, workspace, 80, 24)
	if len(socket.sent) != 1 || socket.sent[0][0] != terminalFrameExit || binary.BigEndian.Uint32(socket.sent[0][1:5]) != 23 || !socket.closed {
		t.Fatalf("guest exit frames=%v closed=%v", socket.sent, socket.closed)
	}
}

func TestTerminalOriginAndSecurityHeaders(t *testing.T) {
	for _, origin := range []string{"null", "https://dashboard.example"} {
		request := httptest.NewRequest("GET", "https://dashboard.example/terminal/ws/socket", http.NoBody)
		request.Header.Set("Origin", origin)
		if err := validateTerminalWebSocketOrigin(nil, request); err != nil {
			t.Fatalf("origin %q rejected: %v", origin, err)
		}
	}
	request := httptest.NewRequest("GET", "https://dashboard.example/terminal/ws/socket", http.NoBody)
	request.Header.Set("Origin", "https://attacker.example")
	if err := validateTerminalWebSocketOrigin(nil, request); err == nil {
		t.Fatal("cross-origin websocket accepted")
	}

	handler := withDashboardSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/", http.NoBody))
	for _, header := range []string{"Content-Security-Policy", "Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy", "Permissions-Policy", "Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options"} {
		if recorder.Header().Get(header) == "" {
			t.Errorf("missing %s", header)
		}
	}
}

//nolint:funlen // one bounded round trip keeps page, assets, handshake, PTY bytes, and bridge metadata in a single acceptance proof.
func TestDashboardTerminalHTTPWebSocketRoundTrip(t *testing.T) {
	store := workspacecatalog.NewStore(nil)
	node := dashboardFingerprint(t, 'd')
	createDashboardWorkspace(t, store, "browser", "browser", node, true)
	_, err := store.Update("browser", workspacecatalog.MutateStatus, func(value *workspacecatalog.Workspace) error {
		value.Status.ObservedGeneration = value.Metadata.Generation
		value.Status.CurrentSandbox.WorkspaceGeneration = value.Metadata.Generation
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = local.Close() })
	t.Cleanup(func() { _ = remote.Close() })
	bridger := &fakeAttachBridger{sessions: []sandboxgateway.AttachSession{&fakeAttachSession{Conn: local}}}
	handler := dashboardHandler(nodectrl.NewRegistry(nil), store, nil, &dashboardTerminal{workspaces: store, bridger: bridger})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	response, err := http.Get(server.URL + "/terminal/browser") //nolint:noctx // httptest server is local and closed by cleanup.
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Security-Policy") == "" || response.Header.Get("X-Frame-Options") != "" {
		t.Fatalf("terminal page status=%d headers=%v", response.StatusCode, response.Header)
	}
	csp := response.Header.Get("Content-Security-Policy")
	for _, want := range []string{server.URL + "/assets/xterm.js", server.URL + "/assets/terminal.js", "ws://" + strings.TrimPrefix(server.URL, "http://") + "/terminal/browser/socket"} {
		if !strings.Contains(csp, want) {
			t.Fatalf("terminal CSP %q does not contain %q", csp, want)
		}
	}
	asset, err := http.Get(server.URL + "/assets/xterm.css") //nolint:noctx // bounded local httptest request.
	if err != nil {
		t.Fatal(err)
	}
	_ = asset.Body.Close()
	if asset.Header.Get("Cross-Origin-Resource-Policy") != "cross-origin" {
		t.Fatalf("asset headers = %v", asset.Header)
	}
	adapter, err := http.Get(server.URL + "/assets/terminal.js") //nolint:noctx // bounded local httptest request.
	if err != nil {
		t.Fatal(err)
	}
	adapterBody, readErr := io.ReadAll(adapter.Body)
	_ = adapter.Body.Close()
	if readErr != nil || !bytes.Contains(adapterBody, []byte("setTimeout(connect, delay)")) ||
		!bytes.Contains(adapterBody, []byte("terminalEnded = true")) {
		t.Fatalf("terminal retry adapter missing bounded reconnect/final-exit guards: %v", readErr)
	}

	serverURL, _ := url.Parse(server.URL)
	ws, err := websocket.Dial("ws://"+serverURL.Host+"/terminal/browser/socket?cols=100&rows=30", "", server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	want := []byte("browser-input")
	readDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, len(want))
		_, readErr := io.ReadFull(remote, buffer)
		if readErr == nil && !bytes.Equal(buffer, want) {
			readErr = errors.New("unexpected terminal input")
		}
		if readErr == nil {
			_, readErr = remote.Write([]byte("guest-output"))
		}
		readDone <- readErr
	}()
	if err := websocket.Message.Send(ws, append([]byte{terminalFrameData}, want...)); err != nil {
		t.Fatal(err)
	}
	var frame []byte
	if err := websocket.Message.Receive(ws, &frame); err != nil {
		t.Fatal(err)
	}
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if len(frame) == 0 || frame[0] != terminalFrameData || string(frame[1:]) != "guest-output" {
		t.Fatalf("terminal output = %q", frame)
	}
	if bridger.calls != 1 || bridger.lastReq.SandboxUID != "sandbox-browser" || !bridger.lastReq.TTY || bridger.lastReq.Cols != 100 || bridger.lastReq.Rows != 30 {
		t.Fatalf("bridger calls=%d request=%+v", bridger.calls, bridger.lastReq)
	}
}
