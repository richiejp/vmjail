package sandboxdcli

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/imagestore"
)

const (
	testImageRef     = "img:1"
	testFakeAddr     = "127.0.0.1:1"
	testFakeUserHost = "user.local:9443"
)

// TestRunWorkspaceFromAndWorkspaceMutuallyExclusive proves the two
// workspace-source flags can't both be set. Catches the user
// confusing themselves with a stale --workspace path while passing
// --workspace-from.
func TestRunWorkspaceFromAndWorkspaceMutuallyExclusive(t *testing.T) {
	deps := runDepsForTest(&fakeStore{})
	err := runRun(context.Background(), io.Discard, runOptions{
		ref:           testImageRef,
		workspace:     "/some/dir",
		workspaceFrom: testFakeAddr,
		workspaceName: "ws",
	}, deps)
	if err == nil {
		t.Fatal("expected error for mutually-exclusive flags")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v, want mutually-exclusive message", err)
	}
}

func TestRunWorkspaceFromRequiresWorkspaceName(t *testing.T) {
	deps := runDepsForTest(&fakeStore{})
	err := runRun(context.Background(), io.Discard, runOptions{
		ref:           testImageRef,
		workspaceFrom: testFakeAddr,
	}, deps)
	if err == nil {
		t.Fatal("expected error for --workspace-from without --workspace-name")
	}
	if !strings.Contains(err.Error(), "must be set together") {
		t.Errorf("error = %v, want must-be-set-together message", err)
	}
}

func TestRunWorkspaceNameRequiresWorkspaceFrom(t *testing.T) {
	deps := runDepsForTest(&fakeStore{})
	err := runRun(context.Background(), io.Discard, runOptions{
		ref:           testImageRef,
		workspaceName: "ws",
	}, deps)
	if err == nil {
		t.Fatal("expected error for --workspace-name without --workspace-from")
	}
}

// TestRunWorkspaceFromPullsAndMountsWorkspace stubs the
// pullWorkspaceFrom dep and verifies the returned tmpdir is wired
// into the boot config as the /workspace virtiofs share.
func TestRunWorkspaceFromPullsAndMountsWorkspace(t *testing.T) {
	tmpdir := t.TempDir()
	cleanupCalled := false
	store := &fakeStore{
		pulledImage: &imagestore.Image{Ref: testImageRef, Digest: testDigest},
		bundle:      &imagestore.Bundle{Digest: testDigest, Path: testBundleDir},
	}
	deps := runDepsForTest(store)
	deps.pullWorkspaceFrom = func(_ context.Context, opts pullWorkspaceOpts) (string, func(), error) {
		if opts.addr != testFakeUserHost || opts.name != "ws" {
			t.Errorf("unexpected pull opts: %+v", opts)
		}
		return tmpdir, func() { cleanupCalled = true }, nil
	}
	bootCfgs := captureBootCfg(&deps)

	err := runRun(context.Background(), io.Discard, runOptions{
		ref:           testImageRef,
		workspaceFrom: testFakeUserHost,
		workspaceName: "ws",
	}, deps)
	if err != nil {
		t.Fatalf("runRun: %v", err)
	}
	if !cleanupCalled {
		t.Error("workspace tmpdir cleanup was not called")
	}

	cfg := *bootCfgs
	if len(cfg.Shares) != 2 {
		t.Fatalf("Shares = %d, want 2 (bundle + workspace)", len(cfg.Shares))
	}
	wsShare := cfg.Shares[1]
	abs, err := filepath.Abs(tmpdir)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	if wsShare.HostDir != abs {
		t.Errorf("workspace share HostDir = %q, want %q", wsShare.HostDir, abs)
	}
	if wsShare.Mountpoint != workspaceMount {
		t.Errorf("workspace Mountpoint = %q, want %q", wsShare.Mountpoint, workspaceMount)
	}
}

// TestRunCmdRunsViaCobra drives the full cobra command tree with a
// swapped-in defaultRunDepsFn, exercising the RunE closure that
// otherwise stays uncovered. Not an integration test — fakes the
// store, boot, and workspace pull.
func TestRunCmdRunsViaCobra(t *testing.T) {
	store := &fakeStore{
		pulledImage: &imagestore.Image{Ref: testImageRef, Digest: testDigest},
		bundle:      &imagestore.Bundle{Digest: testDigest, Path: testBundleDir},
	}
	deps := runDepsForTest(store)
	prev := defaultRunDepsFn
	defaultRunDepsFn = func() runDeps { return deps }
	t.Cleanup(func() { defaultRunDepsFn = prev })

	cmd := NewRootCmd()
	cmd.SetArgs([]string{"run", testImageRef})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

// TestRunKeepWorkspaceSkipsCleanup proves the --keep-workspace
// branch in resolveWorkspaceDir replaces cleanup with a print-only
// no-op so the tmpdir survives the run.
func TestRunKeepWorkspaceSkipsCleanup(t *testing.T) {
	tmpdir := t.TempDir()
	cleanupCalled := false
	store := &fakeStore{
		pulledImage: &imagestore.Image{Ref: testImageRef, Digest: testDigest},
		bundle:      &imagestore.Bundle{Digest: testDigest, Path: testBundleDir},
	}
	deps := runDepsForTest(store)
	deps.pullWorkspaceFrom = func(_ context.Context, _ pullWorkspaceOpts) (string, func(), error) {
		return tmpdir, func() { cleanupCalled = true }, nil
	}
	if err := runRun(context.Background(), io.Discard, runOptions{
		ref:           testImageRef,
		workspaceFrom: testFakeUserHost,
		workspaceName: "ws",
		keepWorkspace: true,
	}, deps); err != nil {
		t.Fatalf("runRun: %v", err)
	}
	if cleanupCalled {
		t.Error("cleanup should NOT be invoked under --keep-workspace")
	}
}

func TestRunWorkspaceFromPropagatesPullError(t *testing.T) {
	deps := runDepsForTest(&fakeStore{})
	wantErr := errors.New("synthetic pull failure")
	deps.pullWorkspaceFrom = func(_ context.Context, _ pullWorkspaceOpts) (string, func(), error) {
		return "", nil, wantErr
	}
	err := runRun(context.Background(), io.Discard, runOptions{
		ref:           testImageRef,
		workspaceFrom: testFakeAddr,
		workspaceName: "ws",
	}, deps)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wrapping %v", err, wantErr)
	}
}
