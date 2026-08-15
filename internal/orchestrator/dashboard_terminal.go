package orchestrator

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/websocket"

	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/sandboxgateway"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

const (
	dashboardTerminalMaxFrame = 32 << 10
	dashboardTerminalIdle     = 45 * time.Second
	terminalFrameData         = byte(0)
	terminalFrameResize       = byte(1)
	terminalFrameHeartbeat    = byte(2)
	terminalFrameExit         = byte(3)
	terminalFrameRetry        = byte(4)
)

type dashboardTerminal struct {
	workspaces *workspacecatalog.Store
	bridger    WorkspaceAttachBridger
}

type terminalSocket interface {
	Receive(*[]byte) error
	Send([]byte) error
	SetReadDeadline(time.Time) error
	Close() error
}

type webSocketTerminal struct{ conn *websocket.Conn }

func (s webSocketTerminal) Receive(frame *[]byte) error {
	return websocket.Message.Receive(s.conn, frame)
}
func (s webSocketTerminal) Send(frame []byte) error { return websocket.Message.Send(s.conn, frame) }
func (s webSocketTerminal) SetReadDeadline(deadline time.Time) error {
	return s.conn.SetReadDeadline(deadline)
}
func (s webSocketTerminal) Close() error { return s.conn.Close() }

// registerDashboardTerminalRoutes exposes a binary WebSocket relay for the
// same container PTY used by `workspace exec`. It deliberately does not call
// itself a virtio-console: that device remains the guest boot console until a
// dedicated workload-console lifecycle is implemented.
func registerDashboardTerminalRoutes(mux *http.ServeMux, terminal *dashboardTerminal) {
	if terminal == nil || terminal.bridger == nil {
		return
	}
	mux.HandleFunc("GET /terminal/{uuid}", terminal.servePage)
	mux.Handle("GET /terminal/{uuid}/socket", terminal)
}

func newDashboardTerminalPage() *template.Template {
	return template.Must(template.New("terminal").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Workspace terminal</title><link rel="stylesheet" href="/assets/xterm.css"><style>html,body,#terminal{height:100%;width:100%;margin:0;background:#070b10}body{display:grid;grid-template-rows:auto 1fr;color:#edf3f8;font:13px system-ui,sans-serif}header{padding:6px 10px;background:#151d27}#terminal{min-height:18rem;padding:4px}</style></head><body><header>Workspace terminal · <span id="terminal-status">connecting</span></header><div id="terminal"></div><script src="/assets/xterm.js"></script><script src="/assets/terminal.js"></script></body></html>`))
}

func (t *dashboardTerminal) servePage(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("uuid")
	if _, err := t.resolve(uuid); err != nil {
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", terminalPageCSP(r, uuid))
	// CSP frame-ancestors is the frame policy. X-Frame-Options SAMEORIGIN
	// rejects this intentionally opaque-origin sandboxed iframe in Chromium.
	w.Header().Del("X-Frame-Options")
	if err := newDashboardTerminalPage().Execute(w, nil); err != nil {
		http.Error(w, "render terminal", http.StatusInternalServerError)
	}
}

func terminalPageCSP(r *http.Request, uuid string) string {
	httpScheme, wsScheme := dashboardHTTPScheme, dashboardWSScheme
	if r.TLS != nil {
		httpScheme, wsScheme = dashboardHTTPSScheme, dashboardWSSScheme
	}
	origin := httpScheme + "://" + r.Host
	socket := wsScheme + "://" + r.Host + "/terminal/" + url.PathEscape(uuid) + "/socket"
	// The iframe intentionally has an opaque origin. Explicit URLs let it load
	// only its immutable assets and its exact Workspace socket; 'self' would
	// match no network URL in an opaque-origin document.
	return "default-src 'none'; script-src " + origin + "/assets/xterm.js " + origin + "/assets/terminal.js; " +
		"style-src 'unsafe-inline' " + origin + "/assets/xterm.css; connect-src " + socket + "; " +
		"img-src 'none'; font-src 'none'; object-src 'none'; base-uri 'none'; frame-ancestors " + origin + "; form-action 'none'"
}

func (t *dashboardTerminal) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	workspace, err := t.resolve(r.PathValue("uuid"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusPreconditionFailed)
		return
	}
	cols, rows, err := terminalDimensions(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	server := websocket.Server{
		Handshake: validateTerminalWebSocketOrigin,
		//nolint:contextcheck // the request context is passed explicitly into the WebSocket handler.
		Handler: func(ws *websocket.Conn) {
			ws.MaxPayloadBytes = dashboardTerminalMaxFrame
			t.serveWebSocket(r.Context(), webSocketTerminal{conn: ws}, workspace, cols, rows)
		},
	}
	server.ServeHTTP(w, r)
}

func validateTerminalWebSocketOrigin(_ *websocket.Config, r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "null" {
		return nil
	}
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != dashboardHTTPScheme && parsed.Scheme != dashboardHTTPSScheme) || !strings.EqualFold(parsed.Host, r.Host) {
		return errors.New("dashboard terminal: websocket origin is not this dashboard")
	}
	return nil
}

func (t *dashboardTerminal) resolve(uuid string) (workspacecatalog.Workspace, error) {
	workspace, err := t.workspaces.Get(uuid)
	if err != nil {
		return workspacecatalog.Workspace{}, fmt.Errorf("terminal workspace: %w", err)
	}
	if workspace.Status.CurrentSandbox == nil || workspace.Status.CurrentSandbox.UID == "" {
		return workspacecatalog.Workspace{}, ErrAttachWorkspaceNotRunning
	}
	if err := requireCurrentWorkspaceGeneration(workspace); err != nil {
		return workspacecatalog.Workspace{}, err
	}
	return workspace, nil
}

func terminalDimensions(r *http.Request) (cols, rows uint32, err error) {
	parse := func(name string, fallback uint32) (uint32, error) {
		raw := r.URL.Query().Get(name)
		if raw == "" {
			return fallback, nil
		}
		value, err := strconv.ParseUint(raw, 10, 16)
		if err != nil || value == 0 {
			return 0, fmt.Errorf("terminal %s must be between 1 and 65535", name)
		}
		return uint32(value), nil
	}
	cols, err = parse("cols", 80)
	if err != nil {
		return 0, 0, err
	}
	rows, err = parse("rows", 24)
	return cols, rows, err
}

func (t *dashboardTerminal) serveWebSocket(ctx context.Context, ws terminalSocket, workspace workspacecatalog.Workspace, cols, rows uint32) {
	_ = ws.SetReadDeadline(time.Now().Add(dashboardTerminalIdle))
	sandbox := workspace.Status.CurrentSandbox
	session, err := t.bridger.OpenAttachBridge(ctx, sandbox.NodeFingerprint, sandboxgateway.AttachRequest{
		SandboxUID: sandbox.UID, TTY: true, Cols: cols, Rows: rows,
	})
	if err != nil {
		_ = sendTerminalRetry(ws, err.Error())
		return
	}
	defer func() { oops.Skip("dashboard terminal: close attach", session.Close(), net.ErrClosed) }()

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			oops.Skip("dashboard terminal: context close", session.Close(), net.ErrClosed)
		case <-done:
		}
	}()
	err = relayBrowserTerminal(ws, session)
	close(done)
	_ = err // The relay has already closed the WebSocket; exit frames are sent by attachToBrowser before closure.
}

func relayBrowserTerminal(ws terminalSocket, session sandboxgateway.AttachSession) error {
	errCh := make(chan error, 2)
	go func() { errCh <- browserToAttach(ws, session) }()
	go func() { errCh <- attachToBrowser(ws, session) }()
	first := <-errCh
	oops.Skip("dashboard terminal: stop relay", session.Close(), net.ErrClosed)
	oops.Skip("dashboard terminal: close websocket", ws.Close(), net.ErrClosed)
	second := <-errCh
	if first != nil && !oops.IsExpectedPumpExit(first) {
		return first
	}
	if second != nil && !oops.IsExpectedPumpExit(second) {
		return second
	}
	return nil
}

func browserToAttach(ws terminalSocket, session sandboxgateway.AttachSession) error {
	//daft:lint:disable=forloopbound reason="the browser heartbeat refreshes a 45s read deadline; EOF, timeout, close, or a malformed bounded frame terminates the interactive session"
	for {
		var frame []byte
		if err := ws.Receive(&frame); err != nil {
			return err
		}
		if len(frame) == 0 {
			return errors.New("dashboard terminal: empty browser frame")
		}
		_ = ws.SetReadDeadline(time.Now().Add(dashboardTerminalIdle))
		switch frame[0] {
		case terminalFrameData:
			if len(frame) == 1 {
				continue
			}
			if _, err := session.Write(frame[1:]); err != nil {
				return err
			}
		case terminalFrameResize:
			if len(frame) != 9 {
				return errors.New("dashboard terminal: resize frame must be 9 bytes")
			}
			cols, rows := binary.BigEndian.Uint32(frame[1:5]), binary.BigEndian.Uint32(frame[5:9])
			if cols == 0 || rows == 0 || cols > 65535 || rows > 65535 {
				return errors.New("dashboard terminal: invalid resize dimensions")
			}
			if err := session.Resize(cols, rows); err != nil {
				return err
			}
		case terminalFrameHeartbeat:
			if len(frame) != 1 {
				return errors.New("dashboard terminal: invalid heartbeat frame")
			}
		default:
			return fmt.Errorf("dashboard terminal: unknown frame type %d", frame[0])
		}
	}
}

func attachToBrowser(ws terminalSocket, session io.Reader) error {
	buffer := make([]byte, dashboardTerminalMaxFrame-1)
	//daft:lint:disable=forloopbound reason="the guest PTY read terminates on process exit or when the peer/context closes the attach session"
	for {
		n, err := session.Read(buffer)
		if n > 0 {
			frame := make([]byte, n+1)
			frame[0] = terminalFrameData
			copy(frame[1:], buffer[:n])
			if sendErr := ws.Send(frame); sendErr != nil {
				return sendErr
			}
		}
		if err != nil {
			var exit *sandboxgateway.AttachExitError
			if errors.As(err, &exit) {
				if sendErr := sendTerminalExit(ws, exit.Code, exit.Reason); sendErr != nil {
					return sendErr
				}
			}
			return err
		}
	}
}

func sendTerminalExit(ws terminalSocket, code int32, reason string) error {
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	frame := make([]byte, 5+len(reason))
	frame[0] = terminalFrameExit
	binary.BigEndian.PutUint32(frame[1:5], uint32(code)) //nolint:gosec // wire format preserves signed exit-code bits.
	copy(frame[5:], reason)
	return ws.Send(frame)
}

func sendTerminalRetry(ws terminalSocket, reason string) error {
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	return ws.Send(append([]byte{terminalFrameRetry}, []byte(reason)...))
}

var _ http.Handler = (*dashboardTerminal)(nil)
