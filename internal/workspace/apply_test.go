package workspace

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/cas"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
)

// applyTestHarness wires up a server with a workspace built from
// `srcRoot`, plus a fresh local CAS to receive chunks, plus a
// Connect client pointing at the in-memory test server. The full
// "round-trip a tree from server to client" cycle runs against this
// harness.
type applyTestHarness struct {
	srcRoot  string
	srvCAS   *cas.Store
	dstCAS   *cas.Store
	dstRoot  string
	manifest *workspacev1.Manifest
	server   *Server
	client   workspacev1connect.WorkspaceServiceClient
}

func newApplyHarness(t *testing.T) *applyTestHarness {
	t.Helper()
	srcRoot := t.TempDir()
	srvStore := mustOpenStore(t)
	dstStore := mustOpenStore(t)
	dstRoot := t.TempDir()

	mustWriteFile(t, filepath.Join(srcRoot, "alpha.txt"), []byte("alpha\n"))
	mustMkdir(t, filepath.Join(srcRoot, "sub"))
	mustWriteFile(t, filepath.Join(srcRoot, "sub", "blob.bin"),
		bytes.Repeat([]byte("Z"), 3*1024))
	if runtime.GOOS != osWindows {
		if err := os.Symlink("alpha.txt", filepath.Join(srcRoot, "link")); err != nil {
			t.Fatalf("Symlink: %v", err)
		}
	}

	m, err := BuildManifest(srcRoot, srvStore, BuildOptions{
		Chunker: &FixedSizeChunker{ChunkSize: 1024},
	})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	srv, err := NewServer(srvStore)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Add("ws", m); err != nil {
		t.Fatalf("Add: %v", err)
	}

	mux := http.NewServeMux()
	path, handler := workspacev1connect.NewWorkspaceServiceHandler(srv)
	mux.Handle(path, handler)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	client := workspacev1connect.NewWorkspaceServiceClient(hs.Client(), hs.URL)

	return &applyTestHarness{
		srcRoot:  srcRoot,
		srvCAS:   srvStore,
		dstCAS:   dstStore,
		dstRoot:  dstRoot,
		manifest: m,
		server:   srv,
		client:   client,
	}
}

func TestApplyManifestRoundTripReconstructsTree(t *testing.T) {
	h := newApplyHarness(t)
	if err := ApplyManifest(context.Background(), h.client, h.dstCAS,
		h.manifest, h.dstRoot, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyManifest: %v", err)
	}

	// Verify file contents round-trip.
	got, err := os.ReadFile(filepath.Join(h.dstRoot, "alpha.txt"))
	if err != nil {
		t.Fatalf("read alpha.txt: %v", err)
	}
	if string(got) != "alpha\n" {
		t.Errorf("alpha.txt = %q, want %q", got, "alpha\n")
	}

	gotBlob, err := os.ReadFile(filepath.Join(h.dstRoot, "sub", "blob.bin"))
	if err != nil {
		t.Fatalf("read blob.bin: %v", err)
	}
	if !bytes.Equal(gotBlob, bytes.Repeat([]byte("Z"), 3*1024)) {
		t.Errorf("blob.bin contents differ from source")
	}

	if runtime.GOOS != osWindows {
		target, err := os.Readlink(filepath.Join(h.dstRoot, "link"))
		if err != nil {
			t.Fatalf("readlink: %v", err)
		}
		if target != "alpha.txt" {
			t.Errorf("symlink target = %q, want alpha.txt", target)
		}
	}
}

func TestApplyManifestPreserveModTime(t *testing.T) {
	h := newApplyHarness(t)
	if err := ApplyManifest(context.Background(), h.client, h.dstCAS,
		h.manifest, h.dstRoot, ApplyOptions{PreserveModTime: true}); err != nil {
		t.Fatalf("ApplyManifest: %v", err)
	}

	srcInfo, err := os.Stat(filepath.Join(h.srcRoot, "alpha.txt"))
	if err != nil {
		t.Fatalf("stat src: %v", err)
	}
	dstInfo, err := os.Stat(filepath.Join(h.dstRoot, "alpha.txt"))
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	// Allow 1s slop — some filesystems lose nanosecond precision.
	delta := dstInfo.ModTime().Sub(srcInfo.ModTime()).Abs()
	if delta > time.Second {
		t.Errorf("mtime delta = %v, want < 1s", delta)
	}
}

func TestApplyManifestSecondPassReusesCachedChunks(t *testing.T) {
	h := newApplyHarness(t)
	if err := ApplyManifest(context.Background(), h.client, h.dstCAS,
		h.manifest, h.dstRoot, ApplyOptions{}); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// Replace the server with one whose store is empty — the next
	// apply must succeed entirely from the local CAS cache.
	emptySrv, err := NewServer(mustOpenStore(t))
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := emptySrv.Add("ws", h.manifest); err != nil {
		t.Fatalf("Add: %v", err)
	}
	mux := http.NewServeMux()
	path, handler := workspacev1connect.NewWorkspaceServiceHandler(emptySrv)
	mux.Handle(path, handler)
	hs2 := httptest.NewServer(mux)
	defer hs2.Close()
	emptyClient := workspacev1connect.NewWorkspaceServiceClient(hs2.Client(), hs2.URL)

	dst2 := t.TempDir()
	// Pre-clear dst so this is a "fresh apply" with cached chunks.
	if err := ApplyManifest(context.Background(), emptyClient, h.dstCAS,
		h.manifest, dst2, ApplyOptions{}); err != nil {
		t.Fatalf("second apply: %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(dst2, "alpha.txt")) //nolint:gosec // G304: dst2 is t.TempDir().
	if string(got) != "alpha\n" {
		t.Errorf("alpha.txt = %q, want %q", got, "alpha\n")
	}
}

func TestApplyManifestRejectsBadInputs(t *testing.T) {
	h := newApplyHarness(t)
	cases := map[string]struct {
		client workspacev1connect.WorkspaceServiceClient
		store  *cas.Store
		m      *workspacev1.Manifest
		dest   string
	}{
		"nil client":   {nil, h.dstCAS, h.manifest, h.dstRoot},
		"nil store":    {h.client, nil, h.manifest, h.dstRoot},
		"nil manifest": {h.client, h.dstCAS, nil, h.dstRoot},
		"empty dest":   {h.client, h.dstCAS, h.manifest, ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ApplyManifest(context.Background(), c.client, c.store,
				c.m, c.dest, ApplyOptions{}); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestApplyManifestRejectsUnsafePath(t *testing.T) {
	h := newApplyHarness(t)
	bad := &workspacev1.Manifest{
		Version: ManifestVersion,
		Entries: []*workspacev1.FileEntry{{
			Path: "../escape",
			Kind: workspacev1.EntryKind_ENTRY_KIND_DIR,
		}},
	}
	err := ApplyManifest(context.Background(), h.client, h.dstCAS,
		bad, h.dstRoot, ApplyOptions{})
	if err == nil {
		t.Fatal("expected error for unsafe path")
	}
}

func TestApplyManifestRejectsUnknownKind(t *testing.T) {
	h := newApplyHarness(t)
	bad := &workspacev1.Manifest{
		Version: ManifestVersion,
		Entries: []*workspacev1.FileEntry{{
			Path: "x",
			Kind: workspacev1.EntryKind_ENTRY_KIND_UNSPECIFIED,
		}},
	}
	err := ApplyManifest(context.Background(), h.client, h.dstCAS,
		bad, h.dstRoot, ApplyOptions{})
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestApplyManifestPreserveMtimeMissingFile(t *testing.T) {
	// PreserveModTime path with mtime=0 must skip the chtimes
	// call (covers the conditional false-branch).
	root := t.TempDir()
	store := mustOpenStore(t)
	mustWriteFile(t, filepath.Join(root, "a.txt"), []byte("a"))
	m, err := BuildManifest(root, store, BuildOptions{})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	for _, e := range m.GetEntries() {
		e.MtimeUnixNano = 0
	}
	srv, err := NewServer(store)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Add("ws", m); err != nil {
		t.Fatalf("Add: %v", err)
	}
	mux := http.NewServeMux()
	path, handler := workspacev1connect.NewWorkspaceServiceHandler(srv)
	mux.Handle(path, handler)
	hs := httptest.NewServer(mux)
	defer hs.Close()
	client := workspacev1connect.NewWorkspaceServiceClient(hs.Client(), hs.URL)

	dst := t.TempDir()
	if err := ApplyManifest(context.Background(), client, mustOpenStore(t),
		m, dst, ApplyOptions{PreserveModTime: true}); err != nil {
		t.Fatalf("ApplyManifest: %v", err)
	}
}

// Apply has no chunks but manifest claims a file → write the empty
// file. Exercises the empty-chunk-list path.
func TestApplyManifestEmptyFileEntry(t *testing.T) {
	store := mustOpenStore(t)
	srv, err := NewServer(store)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	m := &workspacev1.Manifest{
		Version: ManifestVersion,
		Entries: []*workspacev1.FileEntry{{
			Path: "empty",
			Kind: workspacev1.EntryKind_ENTRY_KIND_FILE,
			Mode: 0o644,
		}},
	}
	if err := srv.Add("ws", m); err != nil {
		t.Fatalf("Add: %v", err)
	}
	mux := http.NewServeMux()
	path, handler := workspacev1connect.NewWorkspaceServiceHandler(srv)
	mux.Handle(path, handler)
	hs := httptest.NewServer(mux)
	defer hs.Close()
	client := workspacev1connect.NewWorkspaceServiceClient(hs.Client(), hs.URL)

	dst := t.TempDir()
	if err := ApplyManifest(context.Background(), client, mustOpenStore(t),
		m, dst, ApplyOptions{}); err != nil {
		t.Fatalf("ApplyManifest: %v", err)
	}
	stat, err := os.Stat(filepath.Join(dst, "empty"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stat.Size() != 0 {
		t.Errorf("size = %d, want 0", stat.Size())
	}
}

// Apply with a manifest entry whose digest doesn't exist in the
// server's CAS — fetchChunk fails, applyEntry returns the wrapped
// error.
// TestApplyManifestFailsWhenTargetParentIsAFile forces
// writeFileEntry's MkdirAll error branch by pre-planting a regular
// file at the path that should be the parent dir.
func TestApplyManifestFailsWhenTargetParentIsAFile(t *testing.T) {
	h := newApplyHarness(t)
	bad := &workspacev1.Manifest{
		Version: ManifestVersion,
		Entries: []*workspacev1.FileEntry{{
			Path: "blocker/child.txt",
			Kind: workspacev1.EntryKind_ENTRY_KIND_FILE,
			Mode: 0o600,
		}},
	}
	if err := os.WriteFile(filepath.Join(h.dstRoot, "blocker"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ApplyManifest(context.Background(), h.client, h.dstCAS,
		bad, h.dstRoot, ApplyOptions{}); err == nil {
		t.Fatal("expected MkdirAll error")
	}
}

func TestApplyManifestPropagatesMissingChunkError(t *testing.T) {
	store := mustOpenStore(t)
	srv, err := NewServer(store)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	bogusDigest := "0000000000000000000000000000000000000000000000000000000000000000"
	m := &workspacev1.Manifest{
		Version: ManifestVersion,
		Entries: []*workspacev1.FileEntry{{
			Path: "bogus",
			Kind: workspacev1.EntryKind_ENTRY_KIND_FILE,
			Mode: 0o600,
			Chunks: []*workspacev1.ChunkRef{{
				Digest: bogusDigest, Size: 1,
			}},
		}},
	}
	if err := srv.Add("ws", m); err != nil {
		t.Fatalf("Add: %v", err)
	}
	mux := http.NewServeMux()
	path, handler := workspacev1connect.NewWorkspaceServiceHandler(srv)
	mux.Handle(path, handler)
	hs := httptest.NewServer(mux)
	defer hs.Close()
	client := workspacev1connect.NewWorkspaceServiceClient(hs.Client(), hs.URL)

	dst := t.TempDir()
	err = ApplyManifest(context.Background(), client, mustOpenStore(t),
		m, dst, ApplyOptions{})
	if err == nil {
		t.Fatal("expected error for missing chunk")
	}
}

func TestMapStoreErrorMapsInternal(t *testing.T) {
	err := mapStoreError(errors.New("synthetic store I/O"))
	var connErr *connect.Error
	if !errors.As(err, &connErr) || connErr.Code() != connect.CodeInternal {
		t.Errorf("err = %v, want CodeInternal", err)
	}
}

func TestApplyModeDefaultsAndDirFloor(t *testing.T) {
	dirEntry := &workspacev1.FileEntry{
		Kind: workspacev1.EntryKind_ENTRY_KIND_DIR,
		Mode: 0o000,
	}
	if got := applyMode(dirEntry); got&0o700 != 0o700 {
		t.Errorf("dir mode = %v, want 0o700 floor", got)
	}
	fileEntry := &workspacev1.FileEntry{
		Kind: workspacev1.EntryKind_ENTRY_KIND_FILE,
		Mode: 0,
	}
	if got := applyMode(fileEntry); got != 0o600 {
		t.Errorf("zero file mode = %v, want 0o600", got)
	}
}

func TestFetchChunkPropagatesServerError(t *testing.T) {
	store := mustOpenStore(t)
	srv, err := NewServer(store)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	path, handler := workspacev1connect.NewWorkspaceServiceHandler(srv)
	mux.Handle(path, handler)
	hs := httptest.NewServer(mux)
	defer hs.Close()
	client := workspacev1connect.NewWorkspaceServiceClient(hs.Client(), hs.URL)

	missing := "0000000000000000000000000000000000000000000000000000000000000000"
	err = fetchChunk(context.Background(), client, mustOpenStore(t), missing)
	var connErr *connect.Error
	if !errors.As(err, &connErr) || connErr.Code() != connect.CodeNotFound {
		t.Errorf("err = %v, want NotFound", err)
	}
}
