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
	"time"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/cas"
	"github.com/richiejp/darkaifactory/internal/casindex"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
)

const testSyncKey = "workspace\x00/workspace"

type fakeWorkspaceDiskController struct {
	status        WorkspaceDiskStatus
	statusErr     error
	unfreezeErr   error
	deleteErr     error
	resizeErr     error
	unfreezeCalls int
	resizeCalls   int
	deleteCalls   int
}

func (f *fakeWorkspaceDiskController) Status(string) (WorkspaceDiskStatus, error) {
	return f.status, f.statusErr
}

func (f *fakeWorkspaceDiskController) Unfreeze(context.Context, string) error {
	f.unfreezeCalls++
	return f.unfreezeErr
}

func (f *fakeWorkspaceDiskController) Resize(context.Context, string, uint64) error {
	f.resizeCalls++
	return f.resizeErr
}

func (f *fakeWorkspaceDiskController) Delete(context.Context, string) error {
	f.deleteCalls++
	return f.deleteErr
}

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

func TestExactAdoptionDeletesOnlyStoppedRetainedDisk(t *testing.T) {
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
	controller := &fakeWorkspaceDiskController{}
	server.SetWorkspaceDiskController(controller)
	state, err := server.publishManifest(testSyncKey, &workspacev1.Manifest{Version: 1}, casindex.OriginSandbox, casindex.RecoverabilityProtected)
	if err != nil {
		t.Fatal(err)
	}
	ack := func(state manifestState) error {
		_, ackErr := server.AcknowledgeManifest(t.Context(), connect.NewRequest(&workspacev1.AcknowledgeManifestRequest{
			Workspace: testSyncKey, ManifestDigest: state.digest, Generation: state.generation,
		}))
		return ackErr
	}
	if err := ack(state); err != nil || controller.deleteCalls != 1 {
		t.Fatalf("stopped adoption = %v, delete calls %d", err, controller.deleteCalls)
	}
	state, err = server.publishManifest(testSyncKey, &workspacev1.Manifest{Version: 1}, casindex.OriginSandbox, casindex.RecoverabilityProtected)
	if err != nil {
		t.Fatal(err)
	}
	controller.status.Running = true
	if err := ack(state); err != nil || controller.deleteCalls != 1 {
		t.Fatalf("running adoption = %v, delete calls %d", err, controller.deleteCalls)
	}
}

func TestExactAdoptionReportsRetainedDiskCleanupFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statusErr error
		deleteErr error
	}{
		{name: "status", statusErr: errors.New("status failed")},
		{name: "delete", deleteErr: errors.New("delete failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			server.SetWorkspaceDiskController(&fakeWorkspaceDiskController{
				statusErr: tc.statusErr, deleteErr: tc.deleteErr,
			})
			state, err := server.publishManifest(testSyncKey, &workspacev1.Manifest{Version: 1}, casindex.OriginSandbox, casindex.RecoverabilityProtected)
			if err != nil {
				t.Fatal(err)
			}
			_, err = server.AcknowledgeManifest(t.Context(), connect.NewRequest(&workspacev1.AcknowledgeManifestRequest{
				Workspace: testSyncKey, ManifestDigest: state.digest, Generation: state.generation,
			}))
			if connect.CodeOf(err) != connect.CodeInternal {
				t.Fatalf("acknowledgement error = %v, want internal", err)
			}
		})
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
	if _, err := server.GetStorageStatus(t.Context(), connect.NewRequest(&workspacev1.GetStorageStatusRequest{})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("storage status RPC on closed index = %v", err)
	}
	if _, err := server.Prune(t.Context(), true); err == nil {
		t.Fatal("closed index dry-run prune succeeded")
	}
	if _, err := server.PruneStorage(t.Context(), connect.NewRequest(&workspacev1.PruneStorageRequest{DryRun: true})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("storage prune RPC on closed index = %v", err)
	}
	if _, err := server.RejectManifest(t.Context(), connect.NewRequest(&workspacev1.RejectManifestRequest{
		Workspace: testSyncKey, ManifestDigest: put.Msg.GetManifestDigest(), Generation: put.Msg.GetGeneration(),
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("manifest rejection on closed index = %v", err)
	}
	if _, err := server.DeleteManifest(t.Context(), connect.NewRequest(&workspacev1.DeleteManifestRequest{
		Workspace: testSyncKey, ManifestDigest: put.Msg.GetManifestDigest(), Generation: put.Msg.GetGeneration(),
	})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("manifest deletion on closed index = %v", err)
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

func TestManifestControlRPCsAreExactAndRestorePriorClientState(t *testing.T) {
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
	server, err := NewIndexedServer(store, index)
	if err != nil {
		t.Fatal(err)
	}
	clientManifest := &workspacev1.Manifest{Version: 1}
	clientState, err := server.publishManifest(testSyncKey, clientManifest, casindex.OriginClientUpload, casindex.RecoverabilityRecoverable)
	if err != nil {
		t.Fatal(err)
	}
	sandboxManifest := &workspacev1.Manifest{Version: 2}
	sandboxState, err := server.publishManifest(testSyncKey, sandboxManifest, casindex.OriginSandbox, casindex.RecoverabilityProtected)
	if err != nil {
		t.Fatal(err)
	}
	controller := &fakeWorkspaceDiskController{deleteErr: errors.New("workspace is running")}
	server.SetWorkspaceDiskController(controller)
	if _, err := server.RejectManifest(t.Context(), connect.NewRequest(&workspacev1.RejectManifestRequest{
		Workspace: testSyncKey, ManifestDigest: clientState.digest, Generation: clientState.generation,
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale rejection = %v", err)
	}
	if _, err := server.RejectManifest(t.Context(), connect.NewRequest(&workspacev1.RejectManifestRequest{
		Workspace: testSyncKey, ManifestDigest: sandboxState.digest, Generation: sandboxState.generation,
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition || controller.deleteCalls != 1 {
		t.Fatalf("running-disk rejection = %v, delete calls %d", err, controller.deleteCalls)
	}
	controller.deleteErr = nil
	rejected, err := server.RejectManifest(t.Context(), connect.NewRequest(&workspacev1.RejectManifestRequest{
		Workspace: testSyncKey, ManifestDigest: sandboxState.digest, Generation: sandboxState.generation,
	}))
	if err != nil || controller.deleteCalls != 2 || !rejected.Msg.GetCurrentManifestPresent() || rejected.Msg.GetManifestDigest() != clientState.digest || rejected.Msg.GetGeneration() != clientState.generation {
		t.Fatalf("rejection response = %#v, %v", rejected, err)
	}
	got, err := server.GetManifest(t.Context(), connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: testSyncKey}))
	if err != nil || got.Msg.GetManifest().GetVersion() != clientManifest.GetVersion() {
		t.Fatalf("restored manifest = %#v, %v", got, err)
	}
	if _, err := server.RejectManifest(t.Context(), connect.NewRequest(&workspacev1.RejectManifestRequest{
		Workspace: testSyncKey, ManifestDigest: clientState.digest, Generation: clientState.generation,
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("client manifest rejection = %v", err)
	}
	if _, err := server.DeleteManifest(t.Context(), connect.NewRequest(&workspacev1.DeleteManifestRequest{
		Workspace: testSyncKey, ManifestDigest: sandboxState.digest, Generation: sandboxState.generation,
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale deletion = %v", err)
	}
	if _, err := server.stageClientManifest(t.Context(), testSyncKey, &workspacev1.Manifest{Version: 3}, []string{strings.Repeat("c", 64)}); err != nil {
		t.Fatal(err)
	}
	deleted, err := server.DeleteManifest(t.Context(), connect.NewRequest(&workspacev1.DeleteManifestRequest{
		Workspace: testSyncKey, ManifestDigest: clientState.digest, Generation: clientState.generation,
	}))
	if err != nil || deleted.Msg.GetCurrentManifestPresent() {
		t.Fatalf("deletion response = %#v, %v", deleted, err)
	}
	if _, err := server.GetManifest(t.Context(), connect.NewRequest(&workspacev1.GetManifestRequest{Workspace: testSyncKey})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("deleted manifest remains visible: %v", err)
	}
	plain, _ := NewServer(store)
	if _, err := plain.DeleteManifest(t.Context(), connect.NewRequest(&workspacev1.DeleteManifestRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("plain manifest deletion = %v", err)
	}
}

func TestRejectManifestWithoutPriorGenerationClearsCurrent(t *testing.T) {
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
	server, err := NewIndexedServer(store, index)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.publishManifest("aaa", &workspacev1.Manifest{Version: 1}, casindex.OriginClientUpload, casindex.RecoverabilityRecoverable); err != nil {
		t.Fatal(err)
	}
	state, err := server.publishManifest("only", &workspacev1.Manifest{Version: 1}, casindex.OriginSandbox, casindex.RecoverabilityProtected)
	if err != nil {
		t.Fatal(err)
	}
	server.SetWorkspaceDiskController(&fakeWorkspaceDiskController{})
	if _, err := server.RejectManifest(t.Context(), connect.NewRequest(&workspacev1.RejectManifestRequest{
		Workspace: "only", ManifestDigest: state.digest, Generation: state.generation,
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid sync-key rejection = %v", err)
	}
	server.SetWorkspaceDiskController(nil)
	response, err := server.RejectManifest(t.Context(), connect.NewRequest(&workspacev1.RejectManifestRequest{
		Workspace: "only", ManifestDigest: state.digest, Generation: state.generation,
	}))
	if err != nil || response.Msg.GetCurrentManifestPresent() {
		t.Fatalf("reject without prior = %#v, %v", response, err)
	}
}

func TestStorageControlRPCsAreBoundedAndDryRunOnly(t *testing.T) {
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
	server, err := NewIndexedServer(store, index)
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	mustWriteFile(t, filepath.Join(source, "candidate-a"), []byte("candidate-a"))
	mustWriteFile(t, filepath.Join(source, "candidate-b"), []byte("candidate-b"))
	manifest, err := BuildManifest(source, store, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.publishManifest("recoverable", manifest, casindex.OriginClientUpload, casindex.RecoverabilityRecoverable); err != nil {
		t.Fatal(err)
	}
	server.cleanup, err = casindex.NewWorker(casindex.WorkerConfig{
		Index: index, Policy: casindex.Policy{HardBytes: 40, SoftBytes: 8, TargetBytes: 1, Expiry: time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := server.GetStorageStatus(t.Context(), connect.NewRequest(&workspacev1.GetStorageStatusRequest{}))
	if err != nil || status.Msg.GetHardBytes() != 40 || status.Msg.GetUsageBytes() == 0 || status.Msg.GetExpirySeconds() <= 0 {
		t.Fatalf("storage status = %#v, %v", status, err)
	}
	if _, err := server.PruneStorage(t.Context(), connect.NewRequest(&workspacev1.PruneStorageRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("destructive public prune = %v", err)
	}
	if _, err := server.PruneStorage(t.Context(), connect.NewRequest(&workspacev1.PruneStorageRequest{
		DryRun: true, MaxCandidates: maxPruneCandidates + 1,
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unbounded prune = %v", err)
	}
	if plan, err := server.PruneStorage(t.Context(), connect.NewRequest(&workspacev1.PruneStorageRequest{DryRun: true})); err != nil || len(plan.Msg.GetCandidates()) != 2 {
		t.Fatalf("default bounded plan = %#v, %v", plan, err)
	}
	plan, err := server.PruneStorage(t.Context(), connect.NewRequest(&workspacev1.PruneStorageRequest{DryRun: true, MaxCandidates: 1}))
	if err != nil || !plan.Msg.GetTruncated() || len(plan.Msg.GetCandidates()) != 1 || plan.Msg.GetCandidates()[0].GetDisposition() != string(casindex.DispositionActive) {
		t.Fatalf("dry-run plan = %#v, %v", plan, err)
	}
}

//nolint:funlen // One indexed fixture proves status, resize, unfreeze, and deletion share exact expectations.
func TestWorkspaceDiskControlRPCsValidateExactManifestSet(t *testing.T) {
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
	server, err := NewIndexedServer(store, index)
	if err != nil {
		t.Fatal(err)
	}
	const uuid = "disk-workspace"
	states := make(map[string]manifestState)
	for _, target := range []string{"/work", "/data"} {
		key := SyncKey(uuid, target)
		state, publishErr := server.publishManifest(key, &workspacev1.Manifest{Version: 1}, casindex.OriginSandbox, casindex.RecoverabilityProtected)
		if publishErr != nil {
			t.Fatal(publishErr)
		}
		states[key] = state
	}
	controller := &fakeWorkspaceDiskController{status: WorkspaceDiskStatus{
		UsedBytes: 9, AvailableBytes: 11, AllowanceBytes: 20, Frozen: true, Running: true,
	}}
	server.SetWorkspaceDiskController(controller)
	status, err := server.GetWorkspaceDiskStatus(t.Context(), connect.NewRequest(&workspacev1.GetWorkspaceDiskStatusRequest{WorkspaceUuid: uuid}))
	if err != nil || status.Msg.GetUsedBytes() != 9 || !status.Msg.GetFrozen() || !status.Msg.GetRunning() || len(status.Msg.GetCurrentManifests()) != 2 {
		t.Fatalf("disk status = %#v, %v", status, err)
	}
	expected := make([]*workspacev1.ManifestExpectation, 0, len(states))
	for key, state := range states {
		expected = append(expected, &workspacev1.ManifestExpectation{Workspace: key, ManifestDigest: state.digest, Generation: state.generation})
	}
	controller.status.AllowanceBytes = 40
	resized, err := server.ResizeWorkspaceDisk(t.Context(), connect.NewRequest(&workspacev1.ResizeWorkspaceDiskRequest{
		WorkspaceUuid: uuid, DiskBytes: 40, ExpectedManifests: expected,
	}))
	if err != nil || resized.Msg.GetAllowanceBytes() != 40 || controller.resizeCalls != 1 {
		t.Fatalf("resize = %#v, %v, calls %d", resized, err, controller.resizeCalls)
	}
	if _, err := server.ResizeWorkspaceDisk(t.Context(), connect.NewRequest(&workspacev1.ResizeWorkspaceDiskRequest{
		WorkspaceUuid: uuid,
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("zero resize = %v", err)
	}
	if _, err := server.ResizeWorkspaceDisk(t.Context(), connect.NewRequest(&workspacev1.ResizeWorkspaceDiskRequest{
		WorkspaceUuid: uuid, DiskBytes: 50, ExpectedManifests: expected[:1],
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition || controller.resizeCalls != 1 {
		t.Fatalf("stale resize = %v, calls %d", err, controller.resizeCalls)
	}
	controller.resizeErr = errors.New("resize failed")
	if _, err := server.ResizeWorkspaceDisk(t.Context(), connect.NewRequest(&workspacev1.ResizeWorkspaceDiskRequest{
		WorkspaceUuid: uuid, DiskBytes: 50, ExpectedManifests: expected,
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition || controller.resizeCalls != 2 {
		t.Fatalf("failed resize = %v, calls %d", err, controller.resizeCalls)
	}
	controller.resizeErr = nil
	controller.statusErr = errors.New("status after resize failed")
	if _, err := server.ResizeWorkspaceDisk(t.Context(), connect.NewRequest(&workspacev1.ResizeWorkspaceDiskRequest{
		WorkspaceUuid: uuid, DiskBytes: 50, ExpectedManifests: expected,
	})); connect.CodeOf(err) != connect.CodeInternal || controller.resizeCalls != 3 {
		t.Fatalf("resize status failure = %v, calls %d", err, controller.resizeCalls)
	}
	controller.statusErr = nil
	if _, err := server.UnfreezeWorkspaceDisk(t.Context(), connect.NewRequest(&workspacev1.UnfreezeWorkspaceDiskRequest{
		WorkspaceUuid: uuid, ExpectedManifests: expected[:1],
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition || controller.unfreezeCalls != 0 {
		t.Fatalf("incomplete expectation unfreeze = %v, calls %d", err, controller.unfreezeCalls)
	}
	controller.status.Frozen = false
	unfrozen, err := server.UnfreezeWorkspaceDisk(t.Context(), connect.NewRequest(&workspacev1.UnfreezeWorkspaceDiskRequest{
		WorkspaceUuid: uuid, ExpectedManifests: expected,
	}))
	if err != nil || unfrozen.Msg.GetFrozen() || controller.unfreezeCalls != 1 {
		t.Fatalf("unfreeze = %#v, %v, calls %d", unfrozen, err, controller.unfreezeCalls)
	}
	controller.deleteErr = errors.New("still running")
	if _, err := server.DeleteWorkspaceData(t.Context(), connect.NewRequest(&workspacev1.DeleteWorkspaceDataRequest{
		WorkspaceUuid: uuid, ExpectedManifests: expected,
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("running delete = %v", err)
	}
	controller.deleteErr = nil
	server.mu.Lock()
	server.pending[expected[0].GetWorkspace()] = pendingManifest{reserved: 3}
	server.reservedBytes = 3
	server.mu.Unlock()
	if _, err := server.DeleteWorkspaceData(t.Context(), connect.NewRequest(&workspacev1.DeleteWorkspaceDataRequest{
		WorkspaceUuid: uuid, ExpectedManifests: expected,
	})); err != nil || controller.deleteCalls != 2 {
		t.Fatalf("delete = %v, calls %d", err, controller.deleteCalls)
	}
	if names := server.Names(); len(names) != 0 {
		t.Fatalf("manifests remain after delete: %v", names)
	}
	if currents, err := index.CurrentManifests(); err != nil || len(currents) != 0 {
		t.Fatalf("indexed current manifests after delete = %#v, %v", currents, err)
	}
	for key, state := range states {
		manifest, err := index.Manifest(key, state.digest, state.generation)
		if err != nil || manifest.Current || manifest.Disposition != casindex.DispositionDeleted {
			t.Fatalf("indexed deleted manifest %q = %#v, %v", key, manifest, err)
		}
	}
}

//nolint:funlen // The shared indexed manifest fixture makes the lifecycle failure matrix race-representative and easier to audit together.
func TestWorkspaceDiskControlRPCsRejectInvalidRequestsAndControllerFailures(t *testing.T) {
	const uuid = "disk"
	plain, _ := mustNewServer(t)
	if _, err := plain.GetWorkspaceDiskStatus(t.Context(), connect.NewRequest(&workspacev1.GetWorkspaceDiskStatusRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("status without controller = %v", err)
	}
	if _, err := plain.UnfreezeWorkspaceDisk(t.Context(), connect.NewRequest(&workspacev1.UnfreezeWorkspaceDiskRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("unfreeze without controller = %v", err)
	}
	if _, err := plain.ResizeWorkspaceDisk(t.Context(), connect.NewRequest(&workspacev1.ResizeWorkspaceDiskRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("resize without controller = %v", err)
	}
	if _, err := plain.DeleteWorkspaceData(t.Context(), connect.NewRequest(&workspacev1.DeleteWorkspaceDataRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("delete without controller = %v", err)
	}
	plain.SetWorkspaceDiskController(&fakeWorkspaceDiskController{})
	if _, err := plain.UnfreezeWorkspaceDisk(t.Context(), connect.NewRequest(&workspacev1.UnfreezeWorkspaceDiskRequest{WorkspaceUuid: uuid})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("unfreeze without durable index = %v", err)
	}
	if _, err := plain.ResizeWorkspaceDisk(t.Context(), connect.NewRequest(&workspacev1.ResizeWorkspaceDiskRequest{WorkspaceUuid: uuid, DiskBytes: 1})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("resize without durable index = %v", err)
	}

	root := filepath.Join(t.TempDir(), "indexed")
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
	controller := &fakeWorkspaceDiskController{}
	server.SetWorkspaceDiskController(controller)
	if _, err := server.GetWorkspaceDiskStatus(t.Context(), connect.NewRequest(&workspacev1.GetWorkspaceDiskStatusRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty UUID status = %v", err)
	}
	controller.statusErr = errors.New("status failed")
	if _, err := server.GetWorkspaceDiskStatus(t.Context(), connect.NewRequest(&workspacev1.GetWorkspaceDiskStatusRequest{WorkspaceUuid: "disk"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("failed status = %v", err)
	}
	controller.statusErr = nil
	if _, err := server.UnfreezeWorkspaceDisk(t.Context(), connect.NewRequest(&workspacev1.UnfreezeWorkspaceDiskRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty UUID unfreeze = %v", err)
	}
	if _, err := server.DeleteWorkspaceData(t.Context(), connect.NewRequest(&workspacev1.DeleteWorkspaceDataRequest{})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty UUID delete = %v", err)
	}

	key := SyncKey(uuid, "/work")
	state, err := server.publishManifest(key, &workspacev1.Manifest{Version: 1}, casindex.OriginSandbox, casindex.RecoverabilityProtected)
	if err != nil {
		t.Fatal(err)
	}
	valid := []*workspacev1.ManifestExpectation{{Workspace: key, ManifestDigest: state.digest, Generation: state.generation}}
	if _, err := server.publishManifest(SyncKey("other", "/work"), &workspacev1.Manifest{Version: 1}, casindex.OriginClientUpload, casindex.RecoverabilityRecoverable); err != nil {
		t.Fatal(err)
	}
	extra := append(append([]*workspacev1.ManifestExpectation{}, valid...), &workspacev1.ManifestExpectation{
		Workspace: SyncKey(uuid, "/missing"), ManifestDigest: "missing", Generation: 1,
	})
	if _, err := server.UnfreezeWorkspaceDisk(t.Context(), connect.NewRequest(&workspacev1.UnfreezeWorkspaceDiskRequest{WorkspaceUuid: uuid, ExpectedManifests: extra})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("extra expectation unfreeze = %v", err)
	}
	invalid := [][]*workspacev1.ManifestExpectation{
		{nil},
		{{Workspace: SyncKey("other", "/work")}},
		{valid[0], valid[0]},
		{{Workspace: key, ManifestDigest: "stale", Generation: state.generation}},
	}
	for _, expected := range invalid {
		if _, callErr := server.UnfreezeWorkspaceDisk(t.Context(), connect.NewRequest(&workspacev1.UnfreezeWorkspaceDiskRequest{WorkspaceUuid: uuid, ExpectedManifests: expected})); callErr == nil {
			t.Fatalf("invalid expectation accepted: %#v", expected)
		}
	}
	controller.unfreezeErr = errors.New("grow incomplete")
	if _, err := server.UnfreezeWorkspaceDisk(t.Context(), connect.NewRequest(&workspacev1.UnfreezeWorkspaceDiskRequest{WorkspaceUuid: uuid, ExpectedManifests: valid})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("controller unfreeze failure = %v", err)
	}
	controller.unfreezeErr = nil
	controller.statusErr = errors.New("post-unfreeze status failed")
	if _, err := server.UnfreezeWorkspaceDisk(t.Context(), connect.NewRequest(&workspacev1.UnfreezeWorkspaceDiskRequest{WorkspaceUuid: uuid, ExpectedManifests: valid})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("post-unfreeze status failure = %v", err)
	}
	controller.statusErr = nil
	controller.deleteErr = errors.New("disk running")
	if _, err := server.DeleteWorkspaceData(t.Context(), connect.NewRequest(&workspacev1.DeleteWorkspaceDataRequest{WorkspaceUuid: uuid, ExpectedManifests: valid})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("controller delete failure = %v", err)
	}
	controller.deleteErr = nil
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := server.DeleteWorkspaceData(t.Context(), connect.NewRequest(&workspacev1.DeleteWorkspaceDataRequest{WorkspaceUuid: uuid, ExpectedManifests: valid})); connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("index delete failure = %v", err)
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

func TestReadMountFileReadsOnlyExactRegularMetadata(t *testing.T) {
	srv, store := mustNewServer(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".devcontainer"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(root, ".devcontainer", "devcontainer.json"), []byte(`{"image":"oven/bun"}`))
	m, err := BuildManifest(root, store, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	const key = "ws\x00/workspace"
	if err := srv.Add(key, m); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, found, err := srv.ReadMountFile(t.Context(), key, ".devcontainer/devcontainer.json")
	if err != nil || !found || string(got) != `{"image":"oven/bun"}` {
		t.Fatalf("ReadMountFile = (%q, %v, %v)", got, found, err)
	}
	if _, found, err := srv.ReadMountFile(t.Context(), key, ".devcontainer"); err == nil || found {
		t.Fatalf("directory ReadMountFile = (%v, %v), want error", found, err)
	}
	if _, found, err := srv.ReadMountFile(t.Context(), key, "missing.json"); err != nil || found {
		t.Fatalf("missing ReadMountFile = (%v, %v)", found, err)
	}
	if _, _, err := srv.ReadMountFile(t.Context(), key, "../escape"); err == nil {
		t.Fatal("escaping metadata path succeeded")
	}
	if _, found, err := srv.ReadMountFile(t.Context(), "unknown", "anything"); err != nil || found {
		t.Fatalf("unknown manifest ReadMountFile = (%v, %v)", found, err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := srv.ReadMountFile(canceled, key, ".devcontainer/devcontainer.json"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ReadMountFile error = %v", err)
	}
	oversized := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{{
		Path: "large.json", Kind: workspacev1.EntryKind_ENTRY_KIND_FILE, Size: maxMetadataFileBytes + 1,
	}}}
	if err := srv.Add("oversized", oversized); err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.ReadMountFile(t.Context(), "oversized", "large.json"); err == nil {
		t.Fatal("oversized metadata read succeeded")
	}
	mismatch := &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{{
		Path: "mismatch.json", Kind: workspacev1.EntryKind_ENTRY_KIND_FILE, Size: 1,
	}}}
	if err := srv.Add("mismatch", mismatch); err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.ReadMountFile(t.Context(), "mismatch", "mismatch.json"); err == nil {
		t.Fatal("metadata size mismatch succeeded")
	}
}

func TestBoundedMetadataWriterRejectsOverflow(t *testing.T) {
	w := &boundedMetadataWriter{}
	w.buf.Grow(maxMetadataFileBytes)
	if _, err := w.buf.Write(make([]byte, maxMetadataFileBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("overflow")); err == nil {
		t.Fatal("bounded metadata writer accepted overflow")
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

func TestGetManifestWaitsForConcurrentPreservation(t *testing.T) {
	srv, _ := mustNewServer(t)
	const key = "concurrent\x00/workspace"
	live := t.TempDir()
	mustWriteFile(t, filepath.Join(live, "final.txt"), []byte("final"))
	srv.SetLiveMount(key, live)
	started := make(chan struct{})
	release := make(chan struct{})
	srv.SetLiveMountRefresher(key, func(context.Context, string) error {
		close(started)
		<-release
		return nil
	})
	preserved := make(chan error, 1)
	go func() { preserved <- srv.PreserveLiveMount(t.Context(), key) }()
	<-started
	manifest := make(chan *workspacev1.GetManifestResponse, 1)
	manifestErr := make(chan error, 1)
	go func() {
		resp, err := srv.GetManifest(t.Context(), connect.NewRequest(
			&workspacev1.GetManifestRequest{Workspace: key},
		))
		if err != nil {
			manifestErr <- err
			return
		}
		manifest <- resp.Msg
	}()
	close(release)
	if err := <-preserved; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-manifestErr:
		t.Fatal(err)
	case got := <-manifest:
		if !manifestHasPath(got.GetManifest(), "final.txt") {
			t.Fatalf("manifest = %v, want final.txt", got.GetManifest())
		}
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
