package workspace

import (
	"context"
	"errors"
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

type rejectingAcknowledgeClient struct {
	workspacev1connect.WorkspaceServiceClient
}

func (c rejectingAcknowledgeClient) AcknowledgeManifest(
	context.Context,
	*connect.Request[workspacev1.AcknowledgeManifestRequest],
) (*connect.Response[workspacev1.AcknowledgeManifestResponse], error) {
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("ack unavailable"))
}

type rejectingPutChunkClient struct {
	workspacev1connect.WorkspaceServiceClient
}

func (c rejectingPutChunkClient) PutChunk(
	context.Context,
	*connect.Request[workspacev1.PutChunkRequest],
) (*connect.Response[workspacev1.PutChunkResponse], error) {
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("upload unavailable"))
}

type compatibilityManifestClient struct {
	workspacev1connect.WorkspaceServiceClient
	manifest *workspacev1.Manifest
}

func (c compatibilityManifestClient) GetManifest(
	context.Context,
	*connect.Request[workspacev1.GetManifestRequest],
) (*connect.Response[workspacev1.GetManifestResponse], error) {
	return connect.NewResponse(&workspacev1.GetManifestResponse{Manifest: c.manifest}), nil
}

func TestSyncKeyDistinguishesTargets(t *testing.T) {
	a := SyncKey("ws", "/workspace/src")
	b := SyncKey("ws", "/workspace/data")
	if a == b {
		t.Fatalf("SyncKey collided: %q == %q", a, b)
	}
	if got := SyncKey("ws", "/x"); got != "ws\x00/x" {
		t.Fatalf("SyncKey = %q, want %q", got, "ws\x00/x")
	}
}

// TestPushPullRoundTrip drives the full data plane: build a manifest
// from a source tree into a client-side store, Push it to a server
// backed by its own (initially empty) store, then Pull it into a
// fresh destination using a third store and verify the bytes match.
func TestPushPullRoundTrip(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "a.txt"), []byte(testAlphaBody))
	mustMkdir(t, filepath.Join(src, "sub"))
	mustWriteFile(t, filepath.Join(src, "sub", "b.txt"), []byte("beta\n"))

	pushStore := mustOpenStore(t)
	m, err := BuildManifest(src, pushStore, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}

	srv, _ := mustNewServer(t)
	c := startTestServer(t, srv)

	key := SyncKey("ws", "/workspace")
	if err := Push(context.Background(), c, pushStore, key, m); err != nil {
		t.Fatalf("Push: %v", err)
	}

	dest := t.TempDir()
	pullStore := mustOpenStore(t)
	if err := Pull(context.Background(), c, pullStore, key, dest, ApplyOptions{}); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	for _, tc := range []struct{ rel, want string }{
		{"a.txt", testAlphaBody},
		{filepath.Join("sub", "b.txt"), "beta\n"},
	} {
		got, err := os.ReadFile(filepath.Join(dest, tc.rel)) //nolint:gosec // dest is t.TempDir().
		if err != nil {
			t.Fatalf("read %s: %v", tc.rel, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s = %q, want %q", tc.rel, got, tc.want)
		}
	}
}

// TestPushSkipsChunksServerAlreadyHas verifies the missing-digests
// negotiation: a second Push of the same manifest uploads nothing
// because PutManifest reports no missing chunks.
func TestPushSkipsChunksServerAlreadyHas(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "a.txt"), []byte(testAlphaBody))
	pushStore := mustOpenStore(t)
	m, err := BuildManifest(src, pushStore, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}

	srv, _ := mustNewServer(t)
	c := startTestServer(t, srv)
	key := SyncKey("ws", "/workspace")

	// First PutManifest: every chunk is missing.
	resp, err := c.PutManifest(context.Background(), connect.NewRequest(
		&workspacev1.PutManifestRequest{Workspace: key, Manifest: m},
	))
	if err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	if len(resp.Msg.GetMissingDigests()) == 0 {
		t.Fatal("expected missing digests on first put")
	}
	// Complete the push, then re-put: nothing should be missing now.
	if err := Push(context.Background(), c, pushStore, key, m); err != nil {
		t.Fatalf("Push: %v", err)
	}
	resp2, err := c.PutManifest(context.Background(), connect.NewRequest(
		&workspacev1.PutManifestRequest{Workspace: key, Manifest: m},
	))
	if err != nil {
		t.Fatalf("PutManifest 2: %v", err)
	}
	if got := resp2.Msg.GetMissingDigests(); len(got) != 0 {
		t.Errorf("missing after full push = %v, want none", got)
	}
}

func TestPutManifestDedupsRepeatedChunks(t *testing.T) {
	// Two byte-identical files share one chunk digest; missingDigests
	// must report it once, not once per referencing entry.
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "a.txt"), []byte(testAlphaBody))
	mustWriteFile(t, filepath.Join(src, "b.txt"), []byte(testAlphaBody))
	store := mustOpenStore(t)
	m, err := BuildManifest(src, store, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	srv, _ := mustNewServer(t)
	c := startTestServer(t, srv)
	resp, err := c.PutManifest(context.Background(), connect.NewRequest(
		&workspacev1.PutManifestRequest{Workspace: SyncKey("ws", "/x"), Manifest: m},
	))
	if err != nil {
		t.Fatalf("PutManifest: %v", err)
	}
	if got := len(resp.Msg.GetMissingDigests()); got != 1 {
		t.Errorf("missing digests = %d, want 1 (deduped)", got)
	}
}

func TestPutManifestRejectsEmptyName(t *testing.T) {
	srv, _ := mustNewServer(t)
	c := startTestServer(t, srv)
	_, err := c.PutManifest(context.Background(), connect.NewRequest(
		&workspacev1.PutManifestRequest{Workspace: "", Manifest: &workspacev1.Manifest{}},
	))
	var connErr *connect.Error
	if !errors.As(err, &connErr) || connErr.Code() != connect.CodeInvalidArgument {
		t.Errorf("err = %v, want InvalidArgument", err)
	}
}

func TestPutChunkRejectsCorruptPayload(t *testing.T) {
	srv, _ := mustNewServer(t)
	c := startTestServer(t, srv)
	// Well-formed zstd bytes, but uploaded under a digest they don't
	// hash to — PutCompressed must reject it as corrupt rather than
	// store it under a lying digest.
	local := mustOpenStore(t)
	realDigest, _, err := local.Put(strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
	compressed, err := local.GetCompressed(realDigest)
	if err != nil {
		t.Fatalf("GetCompressed: %v", err)
	}
	wrongDigest := "0000000000000000000000000000000000000000000000000000000000000000"
	_, err = c.PutChunk(context.Background(), connect.NewRequest(
		&workspacev1.PutChunkRequest{Digest: wrongDigest, Compressed: compressed},
	))
	var connErr *connect.Error
	if !errors.As(err, &connErr) || connErr.Code() != connect.CodeInvalidArgument {
		t.Errorf("err = %v, want InvalidArgument", err)
	}
}

func TestPushPullNilArgs(t *testing.T) {
	srv, store := mustNewServer(t)
	c := startTestServer(t, srv)
	ctx := context.Background()
	if err := Push(ctx, nil, store, "k", &workspacev1.Manifest{}); err == nil {
		t.Error("Push: expected nil-client error")
	}
	if err := Push(ctx, c, nil, "k", &workspacev1.Manifest{}); err == nil {
		t.Error("Push: expected nil-store error")
	}
	if err := Push(ctx, c, store, "k", nil); err == nil {
		t.Error("Push: expected nil-manifest error")
	}
	if err := Pull(ctx, nil, store, "k", t.TempDir(), ApplyOptions{}); err == nil {
		t.Error("Pull: expected nil-client error")
	}
}

func TestPushPropagatesPutManifestError(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "a.txt"), []byte(testAlphaBody))
	store := mustOpenStore(t)
	m, err := BuildManifest(src, store, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	srv, _ := mustNewServer(t)
	c := startTestServer(t, srv)
	// Empty key makes the server reject PutManifest, exercising
	// Push's manifest-upload error path.
	if err := Push(context.Background(), c, store, "", m); err == nil {
		t.Fatal("expected error from PutManifest with empty key")
	}
}

func TestPushFailsWhenChunkMissingLocally(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "a.txt"), []byte(testAlphaBody))
	buildStore := mustOpenStore(t)
	m, err := BuildManifest(src, buildStore, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	srv, _ := mustNewServer(t)
	c := startTestServer(t, srv)
	// Push with a *different* empty store: the server reports every
	// chunk missing, but pushChunk can't read them locally.
	emptyStore := mustOpenStore(t)
	if err := Push(context.Background(), c, emptyStore, SyncKey("ws", "/x"), m); err == nil {
		t.Fatal("expected error: chunk absent from local store")
	}
}

func TestPutChunkRejectsMalformedCompression(t *testing.T) {
	srv, _ := mustNewServer(t)
	c := startTestServer(t, srv)
	digest := "0000000000000000000000000000000000000000000000000000000000000000"
	_, err := c.PutChunk(context.Background(), connect.NewRequest(
		&workspacev1.PutChunkRequest{Digest: digest, Compressed: []byte("not zstd at all")},
	))
	var connErr *connect.Error
	if !errors.As(err, &connErr) || connErr.Code() != connect.CodeInternal {
		t.Errorf("err = %v, want Internal", err)
	}
}

func TestPullUnknownWorkspaceFails(t *testing.T) {
	srv, _ := mustNewServer(t)
	c := startTestServer(t, srv)
	err := Pull(context.Background(), c, mustOpenStore(t), "missing", t.TempDir(), ApplyOptions{})
	if err == nil {
		t.Fatal("expected error pulling unknown workspace")
	}
}

func TestPullReportsAcknowledgeFailureAfterMaterialization(t *testing.T) {
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
	mustWriteFile(t, filepath.Join(source, "data"), []byte("payload"))
	manifest, err := BuildManifest(source, store, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.publishManifest("key", manifest, casindex.OriginSandbox, casindex.RecoverabilityProtected); err != nil {
		t.Fatal(err)
	}
	client := rejectingAcknowledgeClient{WorkspaceServiceClient: startTestServer(t, server)}
	dest := t.TempDir()
	if err := Pull(t.Context(), client, mustOpenStore(t), "key", dest, ApplyOptions{}); err == nil || !strings.Contains(err.Error(), "acknowledge manifest") {
		t.Fatalf("Pull acknowledgement error = %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dest, "data")); err != nil || string(data) != "payload" { //nolint:gosec // dest is t.TempDir().
		t.Fatalf("materialized data = %q, %v", data, err)
	}
}

func TestSyncReportsTransferAndApplyFailures(t *testing.T) {
	server, remoteStore := mustNewServer(t)
	client := startTestServer(t, server)
	source := t.TempDir()
	mustWriteFile(t, filepath.Join(source, "data"), []byte("payload"))
	localStore := mustOpenStore(t)
	manifest, err := BuildManifest(source, localStore, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := Push(t.Context(), rejectingPutChunkClient{WorkspaceServiceClient: client}, localStore, "upload", manifest); err == nil || !strings.Contains(err.Error(), "put chunk") {
		t.Fatalf("Push transfer error = %v", err)
	}

	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	compat := compatibilityManifestClient{WorkspaceServiceClient: client, manifest: &workspacev1.Manifest{}}
	if err := Pull(t.Context(), compat, remoteStore, "compat", t.TempDir(), ApplyOptions{}); err != nil {
		t.Fatalf("compatibility Pull: %v", err)
	}
	failingApply := compatibilityManifestClient{WorkspaceServiceClient: client, manifest: manifest}
	if err := Pull(t.Context(), failingApply, remoteStore, "apply-error", blocked, ApplyOptions{}); err == nil {
		t.Fatal("Pull applied into a file")
	}
}
