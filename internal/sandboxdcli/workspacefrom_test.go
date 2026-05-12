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
	testFakeOrchURL  = "https://o"
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
	if !strings.Contains(err.Error(), "requires --workspace-name") {
		t.Errorf("error = %v, want requires-name message", err)
	}
}

// TestRunWorkspaceBrokerValidation covers the three required-flag
// branches and the --workspace-from / --workspace-broker
// mutual-exclusion check.
func TestRunWorkspaceBrokerValidation(t *testing.T) {
	cases := []struct {
		name string
		opts runOptions
		want string
	}{
		{
			name: "missing name",
			opts: runOptions{ref: testImageRef, workspaceBroker: testFakeOrchURL},
			want: "--workspace-broker requires --workspace-name",
		},
		{
			name: "missing serve fp",
			opts: runOptions{ref: testImageRef, workspaceBroker: testFakeOrchURL, workspaceName: "ws"},
			want: "--workspace-broker requires --workspace-serve-fingerprint",
		},
		{
			name: "missing sandbox id",
			opts: runOptions{
				ref: testImageRef, workspaceBroker: testFakeOrchURL, workspaceName: "ws",
				workspaceServeFP: "sha256:" + strings.Repeat("AB:", 31) + "AB",
			},
			want: "--workspace-broker requires --workspace-sandbox-id",
		},
		{
			name: "both modes set",
			opts: runOptions{
				ref: testImageRef, workspaceFrom: testFakeAddr, workspaceBroker: testFakeOrchURL,
				workspaceName: "ws",
			},
			want: "mutually exclusive",
		},
		{
			name: "workspace + broker",
			opts: runOptions{
				ref: testImageRef, workspace: "/some/dir", workspaceBroker: testFakeOrchURL,
				workspaceName: "ws",
			},
			want: "mutually exclusive",
		},
	}
	deps := runDepsForTest(&fakeStore{})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runRun(context.Background(), io.Discard, tc.opts, deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestRunWorkspaceBrokerInvokesDep checks the broker pull dep is
// invoked with the right opts and its result is wired into the
// boot config.
func TestRunWorkspaceBrokerInvokesDep(t *testing.T) {
	tmpdir := t.TempDir()
	cleanupCalled := false
	store := &fakeStore{
		pulledImage: &imagestore.Image{Ref: testImageRef, Digest: testDigest},
		bundle:      &imagestore.Bundle{Digest: testDigest, Path: testBundleDir},
	}
	deps := runDepsForTest(store)
	var got pullWorkspaceBrokerOpts
	deps.pullWorkspaceBroker = func(_ context.Context, opts pullWorkspaceBrokerOpts) (string, func(), error) {
		got = opts
		return tmpdir, func() { cleanupCalled = true }, nil
	}
	bootCfgs := captureBootCfg(&deps)

	const fp = "sha256:" + "AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:" +
		"AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB"
	err := runRun(context.Background(), io.Discard, runOptions{
		ref:                testImageRef,
		workspaceBroker:    "https://orch:9443",
		workspaceName:      "ws",
		workspaceServeFP:   fp,
		workspaceSandboxID: testSandboxID,
	}, deps)
	if err != nil {
		t.Fatalf("runRun: %v", err)
	}
	if got.orchestratorURL != "https://orch:9443" || got.workspace != "ws" ||
		got.sandboxID != testSandboxID || got.serveFP != fp {
		t.Errorf("dep called with %+v", got)
	}
	if !cleanupCalled {
		t.Error("cleanup never called")
	}
	if bootCfgs == nil || len(bootCfgs.Shares) != 2 {
		t.Fatalf("expected boot cfg with bundle + workspace shares, got %+v", bootCfgs)
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
	if !strings.Contains(err.Error(), "--workspace-name requires") {
		t.Errorf("error = %v, want requires-from-or-broker message", err)
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
