package workspace

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	"github.com/localai-org/darkaifactory/internal/cas"
	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
)

func mustNewServer(t *testing.T) (*Server, *cas.Store) {
	t.Helper()
	store := mustOpenStore(t)
	srv, err := NewServer(store)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, store
}

// startTestServer spins up an in-memory httptest.Server hosting the
// workspace handler. mTLS is intentionally skipped here — the
// auth wiring is exercised by the orchestrator e2e tests; this
// test focuses on the WorkspaceService request/response shape.
func startTestServer(t *testing.T, srv *Server) workspacev1connect.WorkspaceServiceClient {
	t.Helper()
	mux := http.NewServeMux()
	path, handler := workspacev1connect.NewWorkspaceServiceHandler(srv)
	mux.Handle(path, handler)

	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)

	return workspacev1connect.NewWorkspaceServiceClient(hs.Client(), hs.URL)
}

func TestNewServerRejectsNilStore(t *testing.T) {
	if _, err := NewServer(nil); err == nil {
		t.Fatal("expected error for nil store")
	}
}

func TestServerAddRejectsBadInputs(t *testing.T) {
	srv, _ := mustNewServer(t)
	if err := srv.Add("", &workspacev1.Manifest{}); err == nil {
		t.Error("expected error for empty name")
	}
	if err := srv.Add("ok", nil); err == nil {
		t.Error("expected error for nil manifest")
	}
}

func TestGetManifestRejectsEmptyName(t *testing.T) {
	srv, _ := mustNewServer(t)
	c := startTestServer(t, srv)
	_, err := c.GetManifest(context.Background(), connect.NewRequest(
		&workspacev1.GetManifestRequest{Workspace: ""},
	))
	if err == nil {
		t.Fatal("expected error")
	}
	var connErr *connect.Error
	if !errors.As(err, &connErr) || connErr.Code() != connect.CodeInvalidArgument {
		t.Errorf("err = %v (code %v), want InvalidArgument", err, connect.Code(0))
	}
}

func TestGetManifestUnknownWorkspaceReturnsNotFound(t *testing.T) {
	srv, _ := mustNewServer(t)
	c := startTestServer(t, srv)
	_, err := c.GetManifest(context.Background(), connect.NewRequest(
		&workspacev1.GetManifestRequest{Workspace: "missing"},
	))
	if err == nil {
		t.Fatal("expected error")
	}
	var connErr *connect.Error
	if !errors.As(err, &connErr) || connErr.Code() != connect.CodeNotFound {
		t.Errorf("err = %v, want NotFound", err)
	}
}

func TestGetManifestRoundTrip(t *testing.T) {
	srv, store := mustNewServer(t)
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "hello.txt"), []byte("hi"))
	m, err := BuildManifest(root, store, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if err := srv.Add("ws1", m); err != nil {
		t.Fatalf("Add: %v", err)
	}

	c := startTestServer(t, srv)
	resp, err := c.GetManifest(context.Background(), connect.NewRequest(
		&workspacev1.GetManifestRequest{Workspace: "ws1"},
	))
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	got := resp.Msg.GetManifest()
	if len(got.GetEntries()) != len(m.GetEntries()) {
		t.Errorf("entries len = %d, want %d", len(got.GetEntries()), len(m.GetEntries()))
	}
}

func TestServerNamesListsAddedManifests(t *testing.T) {
	srv, _ := mustNewServer(t)
	if err := srv.Add("a", &workspacev1.Manifest{}); err != nil {
		t.Fatalf("Add a: %v", err)
	}
	if err := srv.Add("b", &workspacev1.Manifest{}); err != nil {
		t.Fatalf("Add b: %v", err)
	}
	got := srv.Names()
	if len(got) != 2 {
		t.Errorf("Names = %v, want length 2", got)
	}
}

func TestGetChunkRejectsBadDigest(t *testing.T) {
	srv, _ := mustNewServer(t)
	c := startTestServer(t, srv)
	_, err := c.GetChunk(context.Background(), connect.NewRequest(
		&workspacev1.GetChunkRequest{Digest: "not-a-digest"},
	))
	var connErr *connect.Error
	if !errors.As(err, &connErr) || connErr.Code() != connect.CodeInvalidArgument {
		t.Errorf("err = %v, want InvalidArgument", err)
	}
}

func TestGetChunkUnknownReturnsNotFound(t *testing.T) {
	srv, _ := mustNewServer(t)
	c := startTestServer(t, srv)
	missing := "0000000000000000000000000000000000000000000000000000000000000000"
	_, err := c.GetChunk(context.Background(), connect.NewRequest(
		&workspacev1.GetChunkRequest{Digest: missing},
	))
	var connErr *connect.Error
	if !errors.As(err, &connErr) || connErr.Code() != connect.CodeNotFound {
		t.Errorf("err = %v, want NotFound", err)
	}
}
