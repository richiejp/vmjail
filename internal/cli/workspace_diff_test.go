package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestWorkspaceDiffEndToEnd serves a workspace from one dir then
// runs `daft workspace diff` against a sibling dir engineered to
// surface one added, one removed, and one modified entry. Hits
// the happy path through GetManifest + DiffManifests + printer.
func TestWorkspaceDiffEndToEnd(t *testing.T) {
	remote := buildDiffFixture(t, fixtureSide{
		"same.txt":    "shared",
		"added.txt":   "only-on-remote",
		"changed.txt": "new",
	})
	local := buildDiffFixture(t, fixtureSide{
		"same.txt":    "shared",
		"removed.txt": "only-local",
		"changed.txt": "old",
	})
	addr, clientDir := startDirectServeAndApprove(t, remote)

	stdout, stderr, err := runDaft(
		t, workspaceCmdName, diffSubName,
		configDirArg, clientDir,
		"--from", addr,
		"--workspace", "ws",
		"--cas-dir", filepath.Join(clientDir, "cas"),
		"--timeout", "20s",
		local,
	)
	if err != nil {
		t.Fatalf("diff: %v\nstderr=%q\nstdout=%q", err, stderr, stdout)
	}
	for _, want := range []string{"+ added.txt", "- removed.txt", "~ changed.txt"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "same.txt") {
		t.Errorf("identical entry should be omitted:\n%s", stdout)
	}
}

func TestWorkspaceDiffDetailMode(t *testing.T) {
	remote := buildDiffFixture(t, fixtureSide{"a.txt": "x"})
	local := buildDiffFixture(t, fixtureSide{"a.txt": "x"})
	// Identical content but flip local's mode (default 0o600
	// from mustWriteFile) to 0o644 to surface ModeChanged.
	if err := os.Chmod(filepath.Join(local, "a.txt"), 0o644); err != nil { //nolint:gosec // test fixture; the mode is the point.
		t.Fatalf("chmod: %v", err)
	}
	addr, clientDir := startDirectServeAndApprove(t, remote)

	stdout, _, err := runDaft(
		t, workspaceCmdName, diffSubName,
		configDirArg, clientDir,
		"--from", addr, "--workspace", "ws",
		"--cas-dir", filepath.Join(clientDir, "cas"),
		"--detail",
		local,
	)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(stdout, "mode 0644 -> 0600") {
		t.Errorf("expected mode detail in:\n%s", stdout)
	}
}

// TestWorkspaceDiffDetailTypeChange exercises the type-transition
// branch of detail mode (e.g. remote dir vs local file at the same
// path).
func TestWorkspaceDiffDetailTypeChange(t *testing.T) {
	// Remote: x is a file. Local: x is a dir. (Local has to
	// have a non-empty dir for BuildManifest to record it; we
	// put a regular file inside.)
	remote := buildDiffFixture(t, fixtureSide{"x": "file-content"})
	local := t.TempDir()
	if err := os.MkdirAll(filepath.Join(local, "x"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustWriteFile(t, filepath.Join(local, "x", "child"), []byte("c"))
	addr, clientDir := startDirectServeAndApprove(t, remote)
	stdout, _, err := runDaft(
		t, workspaceCmdName, diffSubName,
		configDirArg, clientDir,
		"--from", addr, "--workspace", "ws",
		"--cas-dir", filepath.Join(clientDir, "cas"),
		"--detail",
		local,
	)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(stdout, "dir -> file") {
		t.Errorf("expected type-change detail in:\n%s", stdout)
	}
}

func TestWorkspaceDiffBadFlags(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := runDaft(t, workspaceCmdName, diffSubName,
		"--workspace", "ws", dir); err == nil {
		t.Error("expected error for missing --from")
	}
	if _, _, err := runDaft(t, workspaceCmdName, diffSubName,
		"--from", "127.0.0.1:1", dir); err == nil {
		t.Error("expected error for missing --workspace")
	}
}

func TestWorkspaceDiffUnreachable(t *testing.T) {
	clientDir := t.TempDir()
	if _, err := loadOrGenerateClient(clientDir); err != nil {
		t.Fatalf("loadOrGenerateClient: %v", err)
	}
	local := buildDiffFixture(t, fixtureSide{"a": "x"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := runWorkspaceDiff(ctx, &syncBuf{}, local, workspaceDiffOpts{
		from:          "127.0.0.1:1",
		workspaceName: "ws",
		configDir:     clientDir,
		casDir:        filepath.Join(clientDir, "cas"),
		timeout:       1 * time.Second,
	})
	if err == nil {
		t.Error("expected error against unreachable server")
	}
}

// fixtureSide is a {filename → content} map for buildDiffFixture.
type fixtureSide map[string]string

func buildDiffFixture(t *testing.T, files fixtureSide) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		mustWriteFile(t, filepath.Join(dir, name), []byte(content))
	}
	return dir
}

// startDirectServeAndApprove brings up a direct-mode `daft workspace
// serve` against srcDir, mutually approves a fresh client identity,
// and returns (addr, clientConfigDir). Reuses approveBoth +
// freeAddr from workspace_test.go.
func startDirectServeAndApprove(t *testing.T, srcDir string) (addr, clientDir string) {
	t.Helper()
	serverDir, clientDir := approveBoth(t)
	addr = freeAddr(t)
	stdout, stderr := &syncBuf{}, &syncBuf{}
	cmd := NewRootCmd()
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs([]string{
		workspaceCmdName, serveSubName,
		"--workspace=ws=" + srcDir,
		"--addr=" + addr,
		configDirArg, serverDir,
		"--cas-dir=" + filepath.Join(serverDir, "cas"),
	})
	serveCtx, cancel := context.WithCancel(context.Background())
	cmd.SetContext(serveCtx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = cmd.Execute()
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(stdout.String(), `serving "ws"`)
	})
	if !strings.Contains(stdout.String(), `serving "ws"`) {
		t.Fatalf("server did not start: stdout=%q stderr=%q",
			stdout.String(), stderr.String())
	}
	return addr, clientDir
}
