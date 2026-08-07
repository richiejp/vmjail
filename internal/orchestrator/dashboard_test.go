package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/richiejp/darkaifactory/internal/agentruncatalog"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

func TestDashboardGroupsActiveInactiveDisconnectedAndUnassigned(t *testing.T) {
	connected := dashboardFingerprint(t, '1')
	disconnected := dashboardFingerprint(t, '2')
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	nodes := nodectrl.NewRegistry(func() time.Time { return now })
	nodes.Register(connected, "node-a", "0.1.0", nodectrl.Resources{CPUs: 16, MemoryBytes: 64 << 30})
	workspaces := workspacecatalog.NewStore(func() time.Time { return now })
	createDashboardWorkspace(t, workspaces, "a", "active", connected, true)
	createDashboardWorkspace(t, workspaces, "b", "inactive", connected, false)
	createDashboardWorkspace(t, workspaces, "c", "offline", disconnected, false)
	createDashboardWorkspace(t, workspaces, "d", "unassigned", identity.Fingerprint{}, false)

	status := buildDashboardStatus(nodes, workspaces, now)
	if status.NodeCount != 1 || status.WorkspaceCount != 4 || len(status.Nodes) != 2 || len(status.Unassigned) != 1 {
		t.Fatalf("status counts = %+v", status)
	}
	if !status.Nodes[0].Connected || status.Nodes[0].Hostname != "node-a" || len(status.Nodes[0].Workspaces) != 2 {
		t.Fatalf("connected node = %+v", status.Nodes[0])
	}
	if !status.Nodes[0].Workspaces[0].Active || status.Nodes[0].Workspaces[1].Active {
		t.Fatalf("workspace activity = %+v", status.Nodes[0].Workspaces)
	}
	if status.Nodes[1].Connected || status.Nodes[1].Fingerprint != disconnected.String() {
		t.Fatalf("disconnected node = %+v", status.Nodes[1])
	}
}

func TestDashboardHTTPJSONHTMLAndHealth(t *testing.T) {
	nodes := nodectrl.NewRegistry(nil)
	workspaces := workspacecatalog.NewStore(nil)
	handler := dashboardHandler(nodes, workspaces)

	jsonResponse := httptest.NewRecorder()
	handler.ServeHTTP(jsonResponse, httptest.NewRequest(http.MethodGet, "/v1/status", http.NoBody))
	if jsonResponse.Code != http.StatusOK || jsonResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("JSON response = %d %v", jsonResponse.Code, jsonResponse.Header())
	}
	var status dashboardStatus
	if err := json.Unmarshal(jsonResponse.Body.Bytes(), &status); err != nil || status.Status != "ok" {
		t.Fatalf("decode status = %+v, %v", status, err)
	}

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", http.NoBody))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "darkaifactory") || !strings.Contains(page.Body.String(), "No nodes are connected") {
		t.Fatalf("page = %d %q", page.Code, page.Body.String())
	}
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"status":"ok"`) {
		t.Fatalf("health = %d %q", health.Code, health.Body.String())
	}
}

func TestDashboardWorkspaceNodeSandboxDetails(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	fingerprint := dashboardFingerprint(t, '3')
	nodes := nodectrl.NewRegistry(func() time.Time { return now })
	nodes.Register(fingerprint, "node-one", "1.2.3", nodectrl.Resources{CPUs: 8, MemoryBytes: 16 << 30})
	workspaces := workspacecatalog.NewStore(func() time.Time { return now })
	createDashboardWorkspace(t, workspaces, "workspace-one", "project", fingerprint, true)
	_, err := workspaces.Update("workspace-one", workspacecatalog.MutateStatus, func(workspace *workspacecatalog.Workspace) error {
		workspace.Status.DiskUsedBytes = 2 << 30
		workspace.Status.DiskAllowanceBytes = 20 << 30
		workspace.Status.ProtectedBytes = 1 << 30
		workspace.Status.ManifestDigest = "sha256:manifest"
		workspace.Status.ManifestGeneration = 4
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := dashboardHandler(nodes, workspaces)
	for path, contains := range map[string]string{
		"/workspaces/workspace-one":      "sha256:manifest",
		"/nodes/" + fingerprint.String(): "node-one",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, http.NoBody))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), contains) {
			t.Fatalf("GET %s = %d %q", path, response.Code, response.Body.String())
		}
	}
	redirect := httptest.NewRecorder()
	handler.ServeHTTP(redirect, httptest.NewRequest(http.MethodGet, "/sandboxes/sandbox-workspace-one", http.NoBody))
	if redirect.Code != http.StatusTemporaryRedirect || redirect.Header().Get("Location") != "/workspaces/workspace-one#sandbox" {
		t.Fatalf("sandbox redirect = %d %v", redirect.Code, redirect.Header())
	}
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/workspaces/missing", http.NoBody))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing workspace = %d", missing.Code)
	}
	jsonResponse := httptest.NewRecorder()
	handler.ServeHTTP(jsonResponse, httptest.NewRequest(http.MethodGet, "/v1/workspaces/workspace-one", http.NoBody))
	if jsonResponse.Code != http.StatusOK || !strings.Contains(jsonResponse.Body.String(), `"manifest_digest":"sha256:manifest"`) {
		t.Fatalf("workspace JSON = %d %q", jsonResponse.Code, jsonResponse.Body.String())
	}
	for _, path := range []string{"/v1/workspaces/missing", "/sandboxes/missing", "/nodes/missing"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, http.NoBody))
		if response.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d", path, response.Code)
		}
	}
}

func TestDashboardDetailsIncludeSafeAgentRunAndDisconnectedNode(t *testing.T) {
	now := time.Date(2026, 8, 7, 13, 0, 0, 0, time.UTC)
	fingerprint := dashboardFingerprint(t, '5')
	workspaces := workspacecatalog.NewStore(func() time.Time { return now })
	createDashboardWorkspace(t, workspaces, "workspace-two", "project-two", fingerprint, false)
	db, err := badger.Open(badger.DefaultOptions(t.TempDir()).WithLogger(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close badger: %v", closeErr)
		}
	})
	runs, err := agentruncatalog.Open(db, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	owner := dashboardFingerprint(t, 'a')
	_, err = runs.Create(agentruncatalog.CreateInput{UUID: "run-one", WorkspaceUUID: "workspace-two", ProfileName: "maintenance", ProfileDigest: "sha256:profile", Prompt: "must not render", Command: []string{"dashboard-agent"}, OwnerFingerprint: owner})
	if err != nil {
		t.Fatal(err)
	}
	handler := dashboardHandler(nodectrl.NewRegistry(nil), workspaces, runs)
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/workspaces/workspace-two", http.NoBody))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "run-one") || strings.Contains(page.Body.String(), "must not render") {
		t.Fatalf("agent detail = %d %q", page.Code, page.Body.String())
	}
	node := httptest.NewRecorder()
	handler.ServeHTTP(node, httptest.NewRequest(http.MethodGet, "/nodes/"+fingerprint.String(), http.NoBody))
	if node.Code != http.StatusOK || !strings.Contains(node.Body.String(), "disconnected") {
		t.Fatalf("disconnected node = %d %q", node.Code, node.Body.String())
	}
}

func TestDashboardEventsReadyAndChange(t *testing.T) {
	nodes := nodectrl.NewRegistry(nil)
	workspaces := workspacecatalog.NewStore(nil)
	handler := dashboardHandler(nodes, workspaces)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := newDashboardEventWriter()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/events", http.NoBody).WithContext(ctx))
		close(done)
	}()
	writer.waitFor(t, "event: ready")
	nodes.Register(dashboardFingerprint(t, '4'), "changed", "1", nodectrl.Resources{})
	writer.waitFor(t, "event: change")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("events handler did not stop after cancellation")
	}
	if got := writer.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}
}

func TestDashboardEventsRejectUnsupportedWriterAndStopCanceledRequest(t *testing.T) {
	handler := dashboardHandler(nodectrl.NewRegistry(nil), workspacecatalog.NewStore(nil))
	unsupported := &dashboardFailingWriter{header: make(http.Header)}
	handler.ServeHTTP(unsupported, httptest.NewRequest(http.MethodGet, "/events", http.NoBody))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/events", http.NoBody).WithContext(ctx))
	if !strings.Contains(response.Body.String(), "event: ready") {
		t.Fatalf("canceled events = %q", response.Body.String())
	}
}

func TestDashboardEventsReportAgentRunChange(t *testing.T) {
	db, err := badger.Open(badger.DefaultOptions(t.TempDir()).WithLogger(nil))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close badger: %v", closeErr)
		}
	})
	runs, err := agentruncatalog.Open(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler := dashboardHandler(nodectrl.NewRegistry(nil), workspacecatalog.NewStore(nil), runs)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := newDashboardEventWriter()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/events", http.NoBody).WithContext(ctx))
		close(done)
	}()
	writer.waitFor(t, "event: ready")
	_, err = runs.Create(agentruncatalog.CreateInput{UUID: "run-change", WorkspaceUUID: "dashboard-workspace", ProfileName: "profile", ProfileDigest: "digest", Command: []string{"dashboard-agent"}, OwnerFingerprint: dashboardFingerprint(t, '6')})
	if err != nil {
		t.Fatal(err)
	}
	writer.waitFor(t, "event: change")
	cancel()
	<-done
}

func TestDashboardDetailWriteFailure(t *testing.T) {
	writeDashboardPage(&dashboardFailingWriter{header: make(http.Header)}, newDashboardNodePage(), dashboardNodeDetail{})
}

type dashboardEventWriter struct {
	mu      sync.Mutex
	header  http.Header
	body    strings.Builder
	changed chan struct{}
}

func newDashboardEventWriter() *dashboardEventWriter {
	return &dashboardEventWriter{header: make(http.Header), changed: make(chan struct{}, 8)}
}

func (w *dashboardEventWriter) Header() http.Header { return w.header }
func (*dashboardEventWriter) WriteHeader(int)       {}
func (w *dashboardEventWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	_, _ = w.body.Write(data)
	w.mu.Unlock()
	select {
	case w.changed <- struct{}{}:
	default:
	}
	return len(data), nil
}
func (*dashboardEventWriter) Flush() {}
func (w *dashboardEventWriter) waitFor(t *testing.T, value string) {
	t.Helper()
	deadline := time.After(time.Second)
	//daft:lint:disable=forloopbound reason="the one-second deadline bounds waiting for the asynchronous SSE write"
	for {
		w.mu.Lock()
		found := strings.Contains(w.body.String(), value)
		w.mu.Unlock()
		if found {
			return
		}
		select {
		case <-w.changed:
		case <-deadline:
			t.Fatalf("event stream missing %q", value)
		}
	}
}

func TestDashboardJSONEncodingFailure(t *testing.T) {
	response := httptest.NewRecorder()
	writeDashboardJSON(response, make(chan int))
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "encode dashboard response") {
		t.Fatalf("encoding failure = %d %q", response.Code, response.Body.String())
	}
}

func TestDashboardRenderingHandlesZeroTimestampAndWriteFailure(t *testing.T) {
	status := dashboardStatus{Status: "ok", Nodes: []dashboardNode{{Connected: true, LastSeenAt: time.Time{}}}}
	if err := newDashboardPage().Execute(io.Discard, status); err != nil {
		t.Fatalf("render zero timestamp: %v", err)
	}
	dashboardHandler(nodectrl.NewRegistry(nil), workspacecatalog.NewStore(nil)).ServeHTTP(
		&dashboardFailingWriter{header: make(http.Header)},
		httptest.NewRequest(http.MethodGet, "/", http.NoBody),
	)
}

type dashboardFailingWriter struct{ header http.Header }

func (w *dashboardFailingWriter) Header() http.Header     { return w.header }
func (*dashboardFailingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }
func (*dashboardFailingWriter) WriteHeader(int)           {}

func createDashboardWorkspace(t *testing.T, store *workspacecatalog.Store, uuid, name string, target identity.Fingerprint, active bool) {
	t.Helper()
	workspace, err := store.Create(workspacecatalog.CreateInput{UUID: uuid, Name: name, OwnerFingerprint: dashboardFingerprint(t, 'a')})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = store.Update(workspace.Metadata.UUID, workspacecatalog.MutateSpec, func(value *workspacecatalog.Workspace) error {
		if !target.IsZero() {
			value.Spec.TargetSandboxFingerprint = target.String()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !active {
		return
	}
	_, err = store.Update(workspace.Metadata.UUID, workspacecatalog.MutateStatus, func(value *workspacecatalog.Workspace) error {
		value.Status.CurrentSandbox = &workspacecatalog.SandboxRef{UID: "sandbox-" + uuid, NodeFingerprint: target}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func dashboardFingerprint(t *testing.T, digit byte) identity.Fingerprint {
	t.Helper()
	fingerprint, err := identity.ParseFingerprint("sha256:" + strings.Repeat(string(digit), 64))
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

func TestFormatGiB(t *testing.T) {
	if got := formatGiB(3 << 29); got != "1.5" {
		t.Fatalf("formatGiB = %q", got)
	}
}
