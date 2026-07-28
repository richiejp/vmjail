package workspace

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/cas"
	"github.com/richiejp/darkaifactory/internal/casindex"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
)

const testSyncKey = "workspace\x00/workspace"

func mustNewServer(t *testing.T) (*Server, *cas.Store) {
	t.Helper()
	store := mustOpenStore(t)
	srv, err := NewServer(store)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, store
}

func TestIndexedServerPublishesOnlyCompleteManifestAndRestores(t *testing.T) {
	root := filepath.Join(t.TempDir(), "node-cas")
	store, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	index, err := casindex.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewIndexedServer(store, index)
	if err != nil {
		t.Fatal(err)
	}

	source := t.TempDir()
	mustWriteFile(t, filepath.Join(source, "data"), []byte("indexed payload"))
	clientStore := mustOpenStore(t)
	manifest, err := BuildManifest(source, clientStore, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	put, err := server.PutManifest(t.Context(), connect.NewRequest(&workspacev1.PutManifestRequest{
		Workspace: testSyncKey, Manifest: manifest,
	}))
	if err != nil || len(put.Msg.GetMissingDigests()) == 0 || put.Msg.GetManifestDigest() == "" || put.Msg.GetGeneration() != 1 {
		t.Fatalf("PutManifest = %#v, %v", put, err)
	}
	if _, err := server.GetManifest(t.Context(), connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: testSyncKey})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("partial manifest became visible: %v", err)
	}
	for _, digest := range put.Msg.GetMissingDigests() {
		compressed, err := clientStore.GetCompressed(digest)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := server.PutChunk(t.Context(), connect.NewRequest(&workspacev1.PutChunkRequest{Digest: digest, Compressed: compressed})); err != nil {
			t.Fatal(err)
		}
	}
	got, err := server.GetManifest(t.Context(), connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: testSyncKey}))
	if err != nil || got.Msg.GetManifestDigest() != put.Msg.GetManifestDigest() || got.Msg.GetGeneration() != put.Msg.GetGeneration() {
		t.Fatalf("published manifest = %#v, %v", got, err)
	}
	if _, err := server.AcknowledgeManifest(t.Context(), connect.NewRequest(&workspacev1.AcknowledgeManifestRequest{
		Workspace: testSyncKey, ManifestDigest: put.Msg.GetManifestDigest(), Generation: put.Msg.GetGeneration() + 1,
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale acknowledgement = %v", err)
	}
	if _, err := server.AcknowledgeManifest(t.Context(), connect.NewRequest(&workspacev1.AcknowledgeManifestRequest{
		Workspace: testSyncKey, ManifestDigest: put.Msg.GetManifestDigest(), Generation: put.Msg.GetGeneration(),
	})); err != nil {
		t.Fatal(err)
	}

	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := casindex.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restored, err := NewIndexedServer(store, reopened)
	if err != nil {
		t.Fatal(err)
	}
	got, err = restored.GetManifest(t.Context(), connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: testSyncKey}))
	if err != nil || got.Msg.GetManifestDigest() != put.Msg.GetManifestDigest() || !manifestHasPath(got.Msg.GetManifest(), "data") {
		t.Fatalf("restored manifest = %#v, %v", got, err)
	}
}

func TestIndexedLiveSnapshotRequiresExactAdoption(t *testing.T) {
	root := filepath.Join(t.TempDir(), "node-cas")
	store, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	index, err := casindex.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	server, err := NewIndexedServer(store, index)
	if err != nil {
		t.Fatal(err)
	}
	live := t.TempDir()
	mustWriteFile(t, filepath.Join(live, "first"), []byte("first"))
	server.SetLiveMount(testSyncKey, live)
	first, err := server.GetManifest(t.Context(), connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: testSyncKey}))
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(live, "second"), []byte("second"))
	second, err := server.GetManifest(t.Context(), connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: testSyncKey}))
	if err != nil || second.Msg.GetGeneration() <= first.Msg.GetGeneration() {
		t.Fatalf("new live generation = %#v, %v", second, err)
	}
	if _, err := server.AcknowledgeManifest(t.Context(), connect.NewRequest(&workspacev1.AcknowledgeManifestRequest{
		Workspace: testSyncKey, ManifestDigest: first.Msg.GetManifestDigest(), Generation: first.Msg.GetGeneration(),
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale live acknowledgement = %v", err)
	}
	client := startTestServer(t, server)
	destination := t.TempDir()
	if err := Pull(t.Context(), client, mustOpenStore(t), testSyncKey, destination, ApplyOptions{}); err != nil {
		t.Fatalf("Pull with exact adoption: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "second")); err != nil {
		t.Fatalf("pulled live file: %v", err)
	}
	for _, ref := range manifestChunkRefs(second.Msg.GetManifest()) {
		chunk, err := index.Chunk(ref.Digest)
		if err != nil || chunk.Recoverability != casindex.RecoverabilityRecoverable {
			t.Fatalf("adopted live chunk = %#v, %v", chunk, err)
		}
	}
}

func TestIndexedServerStorageControlsAndValidation(t *testing.T) {
	store := mustOpenStore(t)
	if _, err := NewIndexedServer(store, nil); err == nil {
		t.Fatal("nil index accepted")
	}
	plain, err := NewServer(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := plain.RunCleanup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := plain.StorageStatus(); err == nil {
		t.Fatal("plain server returned indexed status")
	}
	if _, err := plain.Prune(t.Context(), true); err == nil {
		t.Fatal("plain server pruned without an index")
	}
	if _, _, err := encodeManifest(nil); err == nil {
		t.Fatal("nil manifest encoded")
	}
	if got := saturatingWorkspaceBytes(^uint64(0)-1, 10); got != ^uint64(0) {
		t.Fatalf("saturating bytes = %d", got)
	}

	root := filepath.Join(t.TempDir(), "cas")
	indexedStore, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	index, err := casindex.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	indexed, err := NewIndexedServer(indexedStore, index)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := indexed.StorageStatus(); err != nil || status.Policy != casindex.DefaultPolicy() {
		t.Fatalf("indexed status = %#v, %v", status, err)
	}
	if plan, err := indexed.Prune(t.Context(), true); err != nil || len(plan.Candidates) != 0 {
		t.Fatalf("indexed dry prune = %#v, %v", plan, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := indexed.RunCleanup(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cleanup = %v", err)
	}
}

func TestIndexedServerRejectsCorruptRestoredPayload(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cas")
	store, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	index, err := casindex.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = index.Close() })
	if err := index.CommitManifest(casindex.CommitRequest{
		SyncKey: testSyncKey, Digest: strings.Repeat("a", 64), Generation: 1,
		Origin: casindex.OriginClientUpload, Recoverability: casindex.RecoverabilityRecoverable,
		Payload: []byte("not protobuf"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewIndexedServer(store, index); err == nil {
		t.Fatal("corrupt restored manifest accepted")
	}
}

func TestIndexedServerReportsClosedIndexErrors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cas")
	store, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	index, err := casindex.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewIndexedServer(store, index)
	if err != nil {
		t.Fatal(err)
	}
	manifest := &workspacev1.Manifest{}
	put, err := server.PutManifest(t.Context(), connect.NewRequest(&workspacev1.PutManifestRequest{
		Workspace: testSyncKey, Manifest: manifest,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewIndexedServer(store, index); err == nil {
		t.Fatal("server restored from closed index")
	}
	if _, err := server.AcknowledgeManifest(t.Context(), connect.NewRequest(&workspacev1.AcknowledgeManifestRequest{
		Workspace: testSyncKey, ManifestDigest: put.Msg.GetManifestDigest(), Generation: put.Msg.GetGeneration(),
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("acknowledge with closed index = %v", err)
	}
	if _, err := server.PutManifest(t.Context(), connect.NewRequest(&workspacev1.PutManifestRequest{
		Workspace: "another", Manifest: manifest,
	})); err == nil {
		t.Fatal("manifest committed to closed index")
	}
	if _, err := server.StorageStatus(); err == nil {
		t.Fatal("storage status read from closed index")
	}
	if _, err := server.Prune(t.Context(), true); err == nil {
		t.Fatal("closed index dry-run prune succeeded")
	}
	if _, err := server.stageClientManifest(t.Context(), "pending", &workspacev1.Manifest{}, []string{strings.Repeat("c", 64)}); err == nil {
		t.Fatal("pending manifest reserved through closed cleanup index")
	}
	live := t.TempDir()
	mustWriteFile(t, filepath.Join(live, "change"), []byte("protected"))
	server.SetLiveMount("live", live)
	if err := server.PreserveLiveMount(t.Context(), "live"); err == nil {
		t.Fatal("live mount preserved through closed index")
	}
	if _, err := server.GetManifest(t.Context(), connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: "live"})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("live manifest through closed index = %v", err)
	}
}

func TestMaterializeMountReportsBackingDirectoryErrors(t *testing.T) {
	server, _ := mustNewServer(t)
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := server.MaterializeMount(t.Context(), "missing", filepath.Join(blocked, "child")); err == nil {
		t.Fatal("missing workspace materialized below a file")
	}
	if err := server.Add("present", &workspacev1.Manifest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.MaterializeMount(t.Context(), "present", filepath.Join(blocked, "child")); err == nil {
		t.Fatal("present workspace materialized below a file")
	}
}

func TestIndexedServerPendingManifestBranches(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cas")
	store, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	index, err := casindex.Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewIndexedServer(store, index)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.stageClientManifest(t.Context(), "", nil, nil); err == nil {
		t.Fatal("empty manifest identity accepted")
	}
	digestA := strings.Repeat("a", 64)
	digestB := strings.Repeat("b", 64)
	manifest := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{{Chunks: []*workspacev1.ChunkRef{
		{Digest: digestA, Size: 3}, {Digest: digestA, Size: 3}, {Digest: digestB, Size: 4},
	}}}}
	if missing, err := server.missingDigests(manifest); err != nil || len(missing) != 2 {
		t.Fatalf("deduplicated missing digests = %#v, %v", missing, err)
	}
	first, err := server.stageClientManifest(t.Context(), "pending", manifest, []string{digestA, digestB})
	if err != nil {
		t.Fatal(err)
	}
	if same, err := server.stageClientManifest(t.Context(), "pending", manifest, []string{digestA, digestB}); err != nil || same != first {
		t.Fatalf("restaged manifest = %#v, %v", same, err)
	}
	replacement := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{{Chunks: []*workspacev1.ChunkRef{{Digest: digestB, Size: 4}}}}}
	if _, err := server.stageClientManifest(t.Context(), "pending", replacement, []string{digestB}); err != nil {
		t.Fatal(err)
	}
	if err := server.completePendingManifests(digestA); err != nil {
		t.Fatal(err)
	}
	if _, err := server.publishManifest("stable", &workspacev1.Manifest{}, casindex.OriginClientUpload, casindex.RecoverabilityRecoverable); err != nil {
		t.Fatal(err)
	}
	if _, err := server.publishManifest("stable", &workspacev1.Manifest{}, casindex.OriginClientUpload, casindex.RecoverabilityRecoverable); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := server.PutManifest(t.Context(), connect.NewRequest(&workspacev1.PutManifestRequest{
		Workspace: "invalid", Manifest: &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{{Chunks: []*workspacev1.ChunkRef{{Digest: "bad"}}}}},
	})); err == nil {
		t.Fatal("invalid manifest digest accepted")
	}
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
