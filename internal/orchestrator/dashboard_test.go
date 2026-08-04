package orchestrator

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/nodectrl"
	"github.com/richiejp/darkaifactory/internal/workspacecatalog"
)

func TestDashboardGroupsActiveInactiveDisconnectedAndUnassigned(t *testing.T) {
	connected := dashboardFingerprint(t, '1')
	disconnected := dashboardFingerprint(t, '2')
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	nodes := nodectrl.NewRegistry(func() time.Time { return now })
	nodes.Register(connected, "ledbx", "0.1.0", nodectrl.Resources{CPUs: 16, MemoryBytes: 64 << 30})
	workspaces := workspacecatalog.NewStore(func() time.Time { return now })
	createDashboardWorkspace(t, workspaces, "a", "active", connected, true)
	createDashboardWorkspace(t, workspaces, "b", "inactive", connected, false)
	createDashboardWorkspace(t, workspaces, "c", "offline", disconnected, false)
	createDashboardWorkspace(t, workspaces, "d", "unassigned", identity.Fingerprint{}, false)

	status := buildDashboardStatus(nodes, workspaces, now)
	if status.NodeCount != 1 || status.WorkspaceCount != 4 || len(status.Nodes) != 2 || len(status.Unassigned) != 1 {
		t.Fatalf("status counts = %+v", status)
	}
	if !status.Nodes[0].Connected || status.Nodes[0].Hostname != "ledbx" || len(status.Nodes[0].Workspaces) != 2 {
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
