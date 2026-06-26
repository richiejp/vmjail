package sandboxdcli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/imagestore"
)

// TestRunWorkspaceAttachValidation covers the bad-flag combinations
// specific to --workspace-attach (mutex with --workspace and
// --keep-workspace; requires --workspace-from or --workspace-broker).
func TestRunWorkspaceAttachValidation(t *testing.T) {
	cases := []struct {
		name string
		opts runOptions
		want string
	}{
		{
			name: "missing transport",
			opts: runOptions{
				ref: testImageRef, workspaceAttach: true,
			},
			want: "--workspace-attach requires --workspace-from or --workspace-broker",
		},
		{
			name: "keep clash",
			opts: runOptions{
				ref: testImageRef, workspaceAttach: true, keepWorkspace: true,
				workspaceFrom: testFakeAddr, workspaceName: "ws",
			},
			want: "mutually exclusive with --keep-workspace",
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

// TestRunWorkspaceAttachInvokesFromDep: when --workspace-attach is
// paired with --workspace-from, runRun dispatches to
// attachWorkspaceFrom (not pullWorkspaceFrom) and wires the
// returned mountpoint into the boot config as the /workspace share.
func TestRunWorkspaceAttachInvokesFromDep(t *testing.T) {
	tmpdir := t.TempDir()
	cleanupCalled := false
	store := &fakeStore{
		pulledImage: &imagestore.Image{Ref: testImageRef, Digest: testDigest},
		bundle:      &imagestore.Bundle{Digest: testDigest, Path: testBundleDir},
	}
	deps := runDepsForTest(store)
	var fromInvoked bool
	deps.attachWorkspaceFrom = func(_ context.Context, opts pullWorkspaceOpts) (string, func(), error) {
		fromInvoked = true
		if opts.addr != testFakeAddr || opts.name != "ws" {
			t.Errorf("opts = %+v", opts)
		}
		return tmpdir, func() { cleanupCalled = true }, nil
	}
	deps.pullWorkspaceFrom = func(_ context.Context, _ pullWorkspaceOpts) (string, func(), error) {
		t.Fatal("pullWorkspaceFrom should not be called when --workspace-attach is set")
		return "", nil, nil
	}
	cfg := captureBootCfg(&deps)

	if err := runRun(context.Background(), io.Discard, runOptions{
		ref:             testImageRef,
		workspaceAttach: true,
		workspaceFrom:   testFakeAddr,
		workspaceName:   "ws",
	}, deps); err != nil {
		t.Fatalf("runRun: %v", err)
	}
	if !fromInvoked {
		t.Error("attachWorkspaceFrom dep was not invoked")
	}
	if !cleanupCalled {
		t.Error("returned cleanup was not run")
	}
	var hasWorkspaceShare bool
	for _, s := range cfg.Shares {
		if s.Tag == workspaceTag && s.Mountpoint == workspaceMount {
			hasWorkspaceShare = true
			break
		}
	}
	if !hasWorkspaceShare {
		t.Errorf("workspace share missing from boot cfg: %+v", cfg.Shares)
	}
}

// TestRunWorkspaceAttachInvokesBrokerDep mirrors the
// --workspace-attach + --workspace-broker path.
func TestRunWorkspaceAttachInvokesBrokerDep(t *testing.T) {
	tmpdir := t.TempDir()
	store := &fakeStore{
		pulledImage: &imagestore.Image{Ref: testImageRef, Digest: testDigest},
		bundle:      &imagestore.Bundle{Digest: testDigest, Path: testBundleDir},
	}
	deps := runDepsForTest(store)
	var brokerInvoked bool
	deps.attachWorkspaceBroker = func(_ context.Context, opts pullWorkspaceBrokerOpts) (string, func(), error) {
		brokerInvoked = true
		if opts.orchestratorURL != testFakeOrchURL || opts.workspace != "ws" {
			t.Errorf("opts = %+v", opts)
		}
		return tmpdir, func() {}, nil
	}
	deps.pullWorkspaceBroker = func(_ context.Context, _ pullWorkspaceBrokerOpts) (string, func(), error) {
		t.Fatal("pullWorkspaceBroker should not be called when --workspace-attach is set")
		return "", nil, nil
	}

	const fp = "sha256:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:" +
		"AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB:AB"
	if err := runRun(context.Background(), io.Discard, runOptions{
		ref:                testImageRef,
		workspaceAttach:    true,
		workspaceBroker:    testFakeOrchURL,
		workspaceName:      "ws",
		workspaceServeFP:   fp,
		workspaceSandboxID: "sb-1",
	}, deps); err != nil {
		t.Fatalf("runRun: %v", err)
	}
	if !brokerInvoked {
		t.Error("attachWorkspaceBroker dep was not invoked")
	}
}

// TestRunWorkspaceAttachPropagatesDepError pins the
// "attachWorkspaceFrom failed → runRun bails before boot" branch.
func TestRunWorkspaceAttachPropagatesDepError(t *testing.T) {
	deps := runDepsForTest(&fakeStore{})
	want := errors.New("fetcher exploded")
	deps.attachWorkspaceFrom = func(_ context.Context, _ pullWorkspaceOpts) (string, func(), error) {
		return "", nil, want
	}
	err := runRun(context.Background(), io.Discard, runOptions{
		ref:             testImageRef,
		workspaceAttach: true,
		workspaceFrom:   testFakeAddr,
		workspaceName:   "ws",
	}, deps)
	if err == nil || !strings.Contains(err.Error(), want.Error()) {
		t.Errorf("err = %v, want it to wrap %q", err, want.Error())
	}
}
