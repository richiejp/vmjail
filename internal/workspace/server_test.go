package workspace

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/cas"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
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

// TestMaterializeMountAppliesPushedTree covers the Slice S3
// launch-time path: a tree pushed under a key materialises out of the
// local CAS — no network — into a fresh backing dir with file bodies
// intact.
func TestMaterializeMountAppliesPushedTree(t *testing.T) {
	srv, store := mustNewServer(t)
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "hello.txt"), []byte("materialized"))
	m, err := BuildManifest(root, store, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	const key = "ws-uuid\x00/workspace"
	if err := srv.Add(key, m); err != nil {
		t.Fatalf("Add: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "backing")
	applied, err := srv.MaterializeMount(context.Background(), key, dest)
	if err != nil {
		t.Fatalf("MaterializeMount: %v", err)
	}
	if !applied {
		t.Fatal("applied = false, want true for a pushed mount")
	}
	//nolint:gosec // G304: test-controlled path
	got, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	if string(got) != "materialized" {
		t.Errorf("body = %q, want %q", got, "materialized")
	}
}

// TestMaterializeMountUnpushedCreatesEmptyDir: an unknown key yields
// applied=false and a created-but-empty backing dir, so an unpushed
// mount boots as an empty read-write tree.
func TestMaterializeMountUnpushedCreatesEmptyDir(t *testing.T) {
	srv, _ := mustNewServer(t)
	dest := filepath.Join(t.TempDir(), "empty")
	applied, err := srv.MaterializeMount(context.Background(), "never-pushed", dest)
	if err != nil {
		t.Fatalf("MaterializeMount: %v", err)
	}
	if applied {
		t.Error("applied = true, want false for an unpushed mount")
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("backing dir not empty: %v", entries)
	}
}

func TestMaterializeMountRejectsEmptyDest(t *testing.T) {
	srv, _ := mustNewServer(t)
	if _, err := srv.MaterializeMount(context.Background(), "k", ""); err == nil {
		t.Error("expected error for empty dest")
	}
}

// TestMaterializeMountMissingChunkErrors covers the local-only apply's
// hard-fail on a gap: with the manifest registered but a referenced
// chunk evicted from the CAS, there's no client to fetch it, so
// materialisation must error rather than silently produce a truncated
// file.
func TestMaterializeMountMissingChunkErrors(t *testing.T) {
	srv, store := mustNewServer(t)
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "f.txt"), []byte("payload"))
	m, err := BuildManifest(root, store, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	const key = "k"
	if err := srv.Add(key, m); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Evict every chunk the manifest references.
	for _, e := range m.GetEntries() {
		for _, c := range e.GetChunks() {
			if err := store.Delete(c.GetDigest()); err != nil {
				t.Fatalf("Delete: %v", err)
			}
		}
	}
	if _, err := srv.MaterializeMount(context.Background(), key, filepath.Join(t.TempDir(), "d")); err == nil {
		t.Error("expected error materialising with a missing chunk")
	}
}

// TestGetManifestServesLiveMount covers the Slice S4 path: while a sync
// key is registered live, GetManifest re-builds the manifest from the
// live backing dir (reflecting writes made after the last push) rather
// than the stored snapshot, and clearing the registration falls back to
// the snapshot.
func TestGetManifestServesLiveMount(t *testing.T) {
	srv, store := mustNewServer(t)
	const key = "ws\x00/workspace"

	// Last-pushed snapshot: a single file "pushed.txt".
	pushed := t.TempDir()
	mustWriteFile(t, filepath.Join(pushed, "pushed.txt"), []byte("old"))
	snap, err := BuildManifest(pushed, store, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildManifest snapshot: %v", err)
	}
	if err := srv.Add(key, snap); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Live backing dir the running VM has written a different file into.
	live := t.TempDir()
	mustWriteFile(t, filepath.Join(live, "live.txt"), []byte("fresh"))
	srv.SetLiveMount(key, live)

	c := startTestServer(t, srv)
	resp, err := c.GetManifest(context.Background(), connect.NewRequest(
		&workspacev1.GetManifestRequest{Workspace: key},
	))
	if err != nil {
		t.Fatalf("GetManifest (live): %v", err)
	}
	if !manifestHasPath(resp.Msg.GetManifest(), "live.txt") {
		t.Errorf("live manifest missing live.txt: %v", resp.Msg.GetManifest().GetEntries())
	}
	if manifestHasPath(resp.Msg.GetManifest(), "pushed.txt") {
		t.Error("live manifest should not contain the stale pushed.txt")
	}

	// Clearing the registration falls back to the last-pushed snapshot.
	srv.ClearLiveMount(key)
	resp, err = c.GetManifest(context.Background(), connect.NewRequest(
		&workspacev1.GetManifestRequest{Workspace: key},
	))
	if err != nil {
		t.Fatalf("GetManifest (snapshot): %v", err)
	}
	if !manifestHasPath(resp.Msg.GetManifest(), "pushed.txt") {
		t.Errorf("snapshot manifest missing pushed.txt: %v", resp.Msg.GetManifest().GetEntries())
	}
}

// TestGetManifestLiveMountBuildError surfaces a CodeInternal when the
// live backing dir can't be walked (e.g. it was removed underneath us).
func TestGetManifestLiveMountBuildError(t *testing.T) {
	srv, _ := mustNewServer(t)
	const key = "k"
	srv.SetLiveMount(key, filepath.Join(t.TempDir(), "does-not-exist"))
	c := startTestServer(t, srv)
	_, err := c.GetManifest(context.Background(), connect.NewRequest(
		&workspacev1.GetManifestRequest{Workspace: key},
	))
	var connErr *connect.Error
	if !errors.As(err, &connErr) || connErr.Code() != connect.CodeInternal {
		t.Errorf("err = %v, want Internal", err)
	}
}

func TestPreserveLiveMountMakesFinalWritesDurable(t *testing.T) {
	srv, store := mustNewServer(t)
	const key = "preserve\x00/workspace"
	live := t.TempDir()
	mustWriteFile(t, filepath.Join(live, "agent.txt"), []byte("final write"))
	srv.SetLiveMount(key, live)
	if err := srv.PreserveLiveMount(t.Context(), key); err != nil {
		t.Fatalf("PreserveLiveMount: %v", err)
	}
	if _, ok := srv.liveDir(key); ok {
		t.Fatal("live registration remained after preservation")
	}
	if err := os.RemoveAll(live); err != nil {
		t.Fatal(err)
	}
	resp, err := srv.GetManifest(t.Context(), connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: key}))
	if err != nil || !manifestHasPath(resp.Msg.GetManifest(), "agent.txt") {
		t.Fatalf("preserved manifest = %v, %v", resp, err)
	}
	var digest string
	for _, entry := range resp.Msg.GetManifest().GetEntries() {
		if entry.GetPath() == "agent.txt" && len(entry.GetChunks()) > 0 {
			digest = entry.GetChunks()[0].GetDigest()
		}
	}
	has, err := store.Has(digest)
	if err != nil || digest == "" || !has {
		t.Fatal("preserved manifest chunk was not installed in CAS")
	}
}

func TestPreserveLiveMountFailureKeepsOnlyCopyReachable(t *testing.T) {
	srv, _ := mustNewServer(t)
	const key = "failed"
	dir := filepath.Join(t.TempDir(), "missing")
	srv.SetLiveMount(key, dir)
	if err := srv.PreserveLiveMount(t.Context(), key); err == nil {
		t.Fatal("expected preservation failure")
	}
	if got, ok := srv.liveDir(key); !ok || got != dir {
		t.Fatalf("live registration = %q, %v", got, ok)
	}
	if err := srv.PreserveLiveMount(t.Context(), "absent"); err != nil {
		t.Fatalf("absent preservation = %v", err)
	}
}

func TestPreserveLiveMountCannotReplaceNewRegistration(t *testing.T) {
	srv, store := mustNewServer(t)
	const key = "replaced"
	oldDir := t.TempDir()
	newDir := t.TempDir()
	mustWriteFile(t, filepath.Join(oldDir, "old.txt"), []byte("old"))
	manifest, err := BuildManifest(oldDir, store, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	srv.SetLiveMount(key, newDir)
	if err := srv.commitPreservedLiveMount(key, oldDir, manifest); err == nil {
		t.Fatal("stale preservation replaced a newer live registration")
	}
	if got, ok := srv.liveDir(key); !ok || got != newDir {
		t.Fatalf("live registration = %q, %v; want %q", got, ok, newDir)
	}
	if _, ok := srv.manifests[key]; ok {
		t.Fatal("stale manifest was published")
	}
}

func manifestHasPath(m *workspacev1.Manifest, path string) bool {
	for _, e := range m.GetEntries() {
		if e.GetPath() == path {
			return true
		}
	}
	return false
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

func TestLiveMountRefresherFeedsPullAndPreservation(t *testing.T) {
	srv, _ := mustNewServer(t)
	key := "workspace/live"
	dir := t.TempDir()
	srv.SetLiveMount(key, dir)
	refreshes := 0
	srv.SetLiveMountRefresher(key, func(_ context.Context, destination string) error {
		refreshes++
		return os.WriteFile(filepath.Join(destination, "guest.txt"), []byte("xfs"), 0o600)
	})
	response, err := srv.GetManifest(t.Context(), connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: key}))
	if err != nil || !manifestHasPath(response.Msg.GetManifest(), "guest.txt") {
		t.Fatalf("GetManifest = %+v, %v", response, err)
	}
	if err := srv.PreserveLiveMount(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if refreshes != 2 {
		t.Fatalf("refreshes = %d", refreshes)
	}
	if _, live := srv.liveDir(key); live || !manifestHasPath(srv.manifests[key], "guest.txt") {
		t.Fatal("preserved refresh was not committed and unregistered")
	}
}

func TestLiveMountRefresherFailureIsProtected(t *testing.T) {
	srv, _ := mustNewServer(t)
	key := "workspace/live-error"
	dir := t.TempDir()
	want := errors.New("guest unavailable")
	srv.SetLiveMount(key, dir)
	srv.SetLiveMountRefresher(key, func(context.Context, string) error { return want })
	_, err := srv.GetManifest(t.Context(), connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: key}))
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeUnavailable {
		t.Fatalf("GetManifest error = %v", err)
	}
	if err := srv.PreserveLiveMount(t.Context(), key); !errors.Is(err, want) {
		t.Fatalf("PreserveLiveMount = %v", err)
	}
	if _, live := srv.liveDir(key); !live {
		t.Fatal("failed refresh discarded protected live registration")
	}
	srv.SetLiveMountRefresher("unknown", func(context.Context, string) error { return nil })
}
