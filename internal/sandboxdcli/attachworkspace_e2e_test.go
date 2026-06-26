package sandboxdcli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/cas"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
)

// badManifestClient returns a manifest whose chunk-sum mismatches
// the declared size, forcing daftfuse.New to error in
// serveDaftFuse's New branch. GetChunk is inherited from
// Unimplemented... and never called by this test path.
type badManifestClient struct {
	workspacev1connect.UnimplementedWorkspaceServiceHandler
}

func (badManifestClient) GetManifest(_ context.Context, _ *connect.Request[workspacev1.GetManifestRequest]) (*connect.Response[workspacev1.GetManifestResponse], error) {
	return connect.NewResponse(&workspacev1.GetManifestResponse{
		Manifest: &workspacev1.Manifest{Entries: []*workspacev1.FileEntry{{
			Path:   "f",
			Kind:   workspacev1.EntryKind_ENTRY_KIND_FILE,
			Chunks: []*workspacev1.ChunkRef{{Digest: "deadbeef", Size: 1}},
			Size:   99,
		}}},
	}), nil
}

func mustOpenCAS(t *testing.T) *cas.Store {
	t.Helper()
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatalf("cas.Open: %v", err)
	}
	return store
}

// TestAttachLazyWorkspaceFromHappyPath drives the full mTLS-dial +
// daft-fuse mount pipeline against an in-process workspace serve.
// Reads through the mountpoint trigger on-demand GetChunk calls so
// the file contents must come back byte-identical to the source.
//
//nolint:dupl // Structurally parallel to TestPullWorkspaceFromHappyPath by design — same rig, different code path under test.
func TestAttachLazyWorkspaceFromHappyPath(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "alpha.txt"), []byte("alpha-lazy\n"))
	mustWriteFile(t, filepath.Join(src, "subdir", "beta.txt"), []byte("beta-lazy\n"))

	serverDir, sandboxdDir := approveBothSandboxdSides(t)
	addr := startWorkspaceServe(t, serverDir, src)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mountpoint, cleanup, err := attachLazyWorkspaceFrom(ctx, pullWorkspaceOpts{
		addr: addr, name: "ws", configDir: sandboxdDir,
	})
	if err != nil {
		t.Fatalf("attachLazyWorkspaceFrom: %v", err)
	}
	defer cleanup()

	got, err := os.ReadFile(filepath.Join(mountpoint, "alpha.txt")) //nolint:gosec // path is t.TempDir-rooted.
	if err != nil {
		t.Fatalf("read alpha.txt: %v", err)
	}
	if string(got) != "alpha-lazy\n" {
		t.Errorf("alpha.txt = %q, want %q", got, "alpha-lazy\n")
	}
	gotBeta, err := os.ReadFile(filepath.Join(mountpoint, "subdir", "beta.txt")) //nolint:gosec // path is t.TempDir-rooted.
	if err != nil {
		t.Fatalf("read beta.txt: %v", err)
	}
	if string(gotBeta) != "beta-lazy\n" {
		t.Errorf("beta.txt = %q, want %q", gotBeta, "beta-lazy\n")
	}
}

// TestAttachLazyWorkspaceFromUnreachable hits the GetManifest error
// branch in serveDaftFuse: the transport builds, but the dial fails
// before any mount happens.
func TestAttachLazyWorkspaceFromUnreachable(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := attachLazyWorkspaceFrom(ctx, pullWorkspaceOpts{
		addr: testFakeAddr, name: "ws", configDir: dir,
	})
	if err == nil {
		t.Fatal("expected error against unreachable addr")
	}
}

//nolint:dupl // Mirrors TestPullWorkspaceFromCorruptIdentity by design — same fixture, different target function.
func TestAttachLazyWorkspaceFromCorruptIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("WriteFile cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("WriteFile key: %v", err)
	}
	if _, _, err := attachLazyWorkspaceFrom(context.Background(), pullWorkspaceOpts{
		addr: testFakeAddr, name: "ws", configDir: dir,
	}); err == nil {
		t.Fatal("expected error for corrupt identity")
	}
}

// TestAttachLazyWorkspaceFromMkdirTempError forces os.MkdirTemp
// to fail by pointing TMPDIR at a path that doesn't exist —
// covers serveDaftFuse's MkdirTemp error branch (and its
// closeTransport call after GetManifest succeeds).
//
//nolint:dupl // Parallel to TestPullWorkspaceFromMkdirTempError by design.
func TestAttachLazyWorkspaceFromMkdirTempError(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "alpha.txt"), []byte("alpha\n"))

	srvDir, sbxDir := approveBothSandboxdSides(t)
	addr := startWorkspaceServe(t, srvDir, src)

	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does", "not", "exist"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := attachLazyWorkspaceFrom(ctx, pullWorkspaceOpts{
		addr: addr, name: "ws", configDir: sbxDir,
	})
	if err == nil {
		t.Fatal("expected error from MkdirTemp under unwritable TMPDIR")
	}
}

// TestServeDaftFuseBadManifest pins serveDaftFuse's
// daftfuse.New-error branch: a manifest with mismatched
// chunk-sum vs declared size makes daftfuse.New return an
// error before MkdirTemp ever runs.
func TestServeDaftFuseBadManifest(t *testing.T) {
	store := mustOpenCAS(t)
	closeCalled := false
	client := &badManifestClient{}
	_, _, err := serveDaftFuse(context.Background(), client, store, "ws", func() {
		closeCalled = true
	})
	if err == nil {
		t.Fatal("expected error from daftfuse.New on bad manifest")
	}
	if !closeCalled {
		t.Error("closeTransport should be called on error path")
	}
}

func TestAttachLazyWorkspaceFromConfigDirError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, _, err := attachLazyWorkspaceFrom(context.Background(), pullWorkspaceOpts{
		addr: testFakeAddr, name: "ws",
	}); err == nil {
		t.Fatal("expected error when config dir is unresolvable")
	}
}

func TestAttachLazyWorkspaceBrokerBadServeFP(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrGenerateSandboxd(dir); err != nil {
		t.Fatalf("loadOrGenerate: %v", err)
	}
	if _, _, err := attachLazyWorkspaceBroker(context.Background(), pullWorkspaceBrokerOpts{
		orchestratorURL: "https://" + testFakeAddr,
		workspace:       "ws",
		sandboxID:       "sb",
		serveFP:         "not-a-fingerprint",
		configDir:       dir,
	}); err == nil {
		t.Fatal("expected error for bad serve fingerprint")
	}
}

func TestAttachLazyWorkspaceBrokerConfigDirError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, _, err := attachLazyWorkspaceBroker(context.Background(), pullWorkspaceBrokerOpts{
		orchestratorURL: "https://" + testFakeAddr,
		workspace:       "ws", sandboxID: "sb",
		serveFP: "sha256:" + repeatColon(32),
	}); err == nil {
		t.Fatal("expected error when config dir is unresolvable")
	}
}

// repeatColon produces a syntactically valid fingerprint hex string
// (32 byte pairs, no actual cryptographic meaning) so ParseFingerprint
// accepts it and the failure has to come from the orchestrator dial.
func repeatColon(byteCount int) string {
	out := make([]byte, 0, byteCount*3-1)
	for i := 0; i < byteCount; i++ {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, 'a', 'a')
	}
	return string(out)
}
