package cli

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewImageCmdHasPush(t *testing.T) {
	cmd := newImageCmd()
	var found bool
	for _, c := range cmd.Commands() {
		if c.Name() == "push" {
			found = true
		}
	}
	if !found {
		t.Error("daft image is missing the push subcommand")
	}
}

// TestRunImagePushMissingTarball drives runImagePush far enough to build
// the transport, then fails at the image load (no network needed): the
// orchestrator flag short-circuits resolution and a fresh config dir
// generates an identity.
func TestRunImagePushMissingTarball(t *testing.T) {
	opts := imagePushOpts{
		orchestrator: testOrchURL,
		configDir:    t.TempDir(),
		repo:         "test/missing",
		tag:          "v1",
	}
	err := runImagePush(context.Background(), io.Discard, filepath.Join(t.TempDir(), "nope.tar"), opts)
	if err == nil {
		t.Fatal("expected an error pushing a nonexistent tarball")
	}
}

// TestRunImagePushPrintsRef covers the happy path with an injected push:
// the command resolves the orchestrator + identity, builds the transport,
// and prints the ref the push returns.
func TestRunImagePushPrintsRef(t *testing.T) {
	var gotTar, gotRepo, gotTag string
	push := func(tarPath, repo, tag string, _ http.RoundTripper) (string, error) {
		gotTar, gotRepo, gotTag = tarPath, repo, tag
		return "daft-registry.internal/" + repo + ":" + tag, nil
	}
	opts := imagePushOpts{orchestrator: testOrchURL, configDir: t.TempDir(), repo: "team/app", tag: "v9"}
	var out bytes.Buffer
	if err := runImagePushWith(context.Background(), &out, "/img.tar", opts, push); err != nil {
		t.Fatalf("runImagePushWith: %v", err)
	}
	if gotTar != "/img.tar" || gotRepo != "team/app" || gotTag != "v9" {
		t.Errorf("push args = %q %q %q", gotTar, gotRepo, gotTag)
	}
	if strings.TrimSpace(out.String()) != "daft-registry.internal/team/app:v9" {
		t.Errorf("printed ref = %q", out.String())
	}
}

// TestRunImagePushBadConfigDir covers the identity-open error branch: a
// config dir that is actually a file makes openClientIdentity fail before
// any push.
func TestRunImagePushBadConfigDir(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	opts := imagePushOpts{orchestrator: testOrchURL, configDir: notADir, repo: "r", tag: "v1"}
	push := func(string, string, string, http.RoundTripper) (string, error) {
		t.Fatal("push should not run when the config dir is invalid")
		return "", nil
	}
	if err := runImagePushWith(context.Background(), io.Discard, "/img.tar", opts, push); err == nil {
		t.Fatal("expected an error opening an invalid client config dir")
	}
}

// TestImagePushCmdExecutes exercises the cobra RunE + flag wiring; it
// fails at the missing tarball, which still runs the default deps.
func TestImagePushCmdExecutes(t *testing.T) {
	cmd := newImagePushCmd()
	cmd.SetArgs([]string{
		filepath.Join(t.TempDir(), "nope.tar"),
		orchestratorArg, testOrchURL,
		configDirArg, t.TempDir(),
	})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	if err := cmd.ExecuteContext(context.Background()); err == nil {
		t.Fatal("expected an error for a nonexistent tarball")
	}
}
