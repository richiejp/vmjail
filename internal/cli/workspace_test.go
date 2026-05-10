package cli

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/mtls"
)

// approveBoth wires up two daft client config dirs (server-side
// and client-side) so each side has approved the other's
// fingerprint. Returns the two dir paths.
func approveBoth(t *testing.T) (serverDir, clientDir string) {
	t.Helper()
	serverDir = t.TempDir()
	clientDir = t.TempDir()

	srvID, err := loadOrGenerateClient(serverDir)
	if err != nil {
		t.Fatalf("server identity: %v", err)
	}
	cliID, err := loadOrGenerateClient(clientDir)
	if err != nil {
		t.Fatalf("client identity: %v", err)
	}

	if _, _, err := runDaft(t, authCmdName, configDirArg, serverDir, "approve",
		cliID.Fingerprint().String()); err != nil {
		t.Fatalf("server approve client: %v", err)
	}
	if _, _, err := runDaft(t, authCmdName, configDirArg, clientDir, "approve",
		srvID.Fingerprint().String()); err != nil {
		t.Fatalf("client approve server: %v", err)
	}
	return serverDir, clientDir
}

// freePort grabs an ephemeral 127.0.0.1 port and immediately closes
// it — the kernel will keep it free long enough for our serve to
// rebind.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestWorkspaceServeAndPullRoundTrip(t *testing.T) {
	serverDir, clientDir := approveBoth(t)

	src := t.TempDir()
	mustWriteFileCLI(t, filepath.Join(src, "alpha.txt"), []byte("alpha\n"))
	mustWriteFileCLI(t, filepath.Join(src, "blob.bin"),
		bytes.Repeat([]byte("Z"), 4096))

	addr := freeAddr(t)

	stdout := &syncBuf{}
	stderr := &syncBuf{}
	cmd := NewRootCmd()
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs([]string{
		workspaceCmdName, serveSubName,
		"--workspace=ws=" + src,
		"--addr=" + addr,
		configDirArg, serverDir,
		"--cas-dir=" + filepath.Join(serverDir, "cas"),
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd.SetContext(ctx)

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

	// Wait for the listener line.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stdout.String(), "serving \"ws\"") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(stdout.String(), "serving \"ws\"") {
		t.Fatalf("server did not start: stdout=%q stderr=%q",
			stdout.String(), stderr.String())
	}

	dest := t.TempDir()
	pullStdout, pullStderr, err := runDaft(t, workspaceCmdName, pullSubName,
		"--from", addr, "--workspace", "ws",
		configDirArg, clientDir,
		"--cas-dir", filepath.Join(clientDir, "cas"),
		dest)
	if err != nil {
		t.Fatalf("pull: %v\nstdout=%q stderr=%q",
			err, pullStdout, pullStderr)
	}
	got, err := os.ReadFile(filepath.Join(dest, "alpha.txt")) //nolint:gosec // G304: dest is t.TempDir().
	if err != nil {
		t.Fatalf("read alpha.txt: %v", err)
	}
	if string(got) != "alpha\n" {
		t.Errorf("alpha.txt = %q, want %q", got, "alpha\n")
	}
	gotBlob, err := os.ReadFile(filepath.Join(dest, "blob.bin")) //nolint:gosec // G304: dest is t.TempDir().
	if err != nil {
		t.Fatalf("read blob.bin: %v", err)
	}
	if !bytes.Equal(gotBlob, bytes.Repeat([]byte("Z"), 4096)) {
		t.Error("blob.bin contents differ from source")
	}
}

func TestParseWorkspaceFlag(t *testing.T) {
	cases := map[string]struct {
		in   string
		name string
		path string
		ok   bool
	}{
		"good":        {"dev=./repo", "dev", "./repo", true},
		"absolute":    {"a=/tmp/x", "a", "/tmp/x", true},
		"missing eq":  {"justname", "", "", false},
		"empty name":  {"=path", "", "", false},
		"empty path":  {"name=", "", "", false},
		"empty input": {"", "", "", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			gotName, gotPath, err := parseWorkspaceFlag(c.in)
			if c.ok && err != nil {
				t.Fatalf("parseWorkspaceFlag(%q) = %v, want nil", c.in, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("parseWorkspaceFlag(%q) = nil, want error", c.in)
			}
			if c.ok {
				if gotName != c.name || gotPath != c.path {
					t.Errorf("(%q, %q), want (%q, %q)",
						gotName, gotPath, c.name, c.path)
				}
			}
		})
	}
}

func TestWorkspaceServeRejectsMissingFlag(t *testing.T) {
	dir := t.TempDir()
	_, _, err := runDaft(t, workspaceCmdName, serveSubName,
		configDirArg, dir, "--addr=127.0.0.1:0")
	if err == nil {
		t.Fatal("expected error when --workspace is missing")
	}
}

func TestWorkspaceServeRejectsBadFlagShape(t *testing.T) {
	dir := t.TempDir()
	_, _, err := runDaft(t, workspaceCmdName, serveSubName,
		"--workspace=missing-equals",
		configDirArg, dir, "--addr=127.0.0.1:0")
	if err == nil {
		t.Fatal("expected error for malformed --workspace")
	}
}

func TestWorkspaceServeFailsForMissingDir(t *testing.T) {
	dir := t.TempDir()
	_, _, err := runDaft(t, workspaceCmdName, serveSubName,
		"--workspace=ws=/definitely/does/not/exist",
		configDirArg, dir, "--addr=127.0.0.1:0")
	if err == nil {
		t.Fatal("expected error for missing src dir")
	}
}

func TestWorkspacePullRejectsMissingFrom(t *testing.T) {
	dir := t.TempDir()
	_, _, err := runDaft(t, workspaceCmdName, pullSubName,
		"--workspace=ws", configDirArg, dir, t.TempDir())
	if err == nil {
		t.Fatal("expected error for missing --from")
	}
}

func TestWorkspacePullRejectsMissingWorkspaceName(t *testing.T) {
	dir := t.TempDir()
	_, _, err := runDaft(t, workspaceCmdName, pullSubName,
		"--from=127.0.0.1:1", configDirArg, dir, t.TempDir())
	if err == nil {
		t.Fatal("expected error for missing --workspace")
	}
}

// TestRunUntilCancelReturnsServeError forces the Serve-error branch
// of runUntilCancel by shutting the server down externally before
// the function gets to see its select{}.
func TestRunUntilCancelReturnsServeError(t *testing.T) {
	id, err := identity.Generate(identity.RoleClient)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	store, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	srv, err2 := mtls.New(mtls.Config{
		Identity: id, Peers: store, Addr: "127.0.0.1:0",
		Handler: http.NewServeMux(),
	})
	if err2 != nil {
		t.Fatalf("mtls.New: %v", err2)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	// Shut the server down in another goroutine; runUntilCancel
	// will see errCh fill with http.ErrServerClosed and return nil.
	go func() {
		time.Sleep(50 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	if err := runUntilCancel(context.Background(), srv); err != nil {
		t.Errorf("runUntilCancel: %v", err)
	}
}

func TestWorkspacePullFailsForCorruptIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, _, err := runDaft(t, workspaceCmdName, pullSubName,
		"--from", "127.0.0.1:1", "--workspace", "ws",
		configDirArg, dir, t.TempDir())
	if err == nil {
		t.Fatal("expected identity-load error")
	}
}

func TestWorkspacePullFailsForCorruptPeers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, peersFilename), []byte("not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, _, err := runDaft(t, workspaceCmdName, pullSubName,
		"--from", "127.0.0.1:1", "--workspace", "ws",
		configDirArg, dir, t.TempDir())
	if err == nil {
		t.Fatal("expected peer-store error")
	}
}

func TestWorkspaceServeFailsForCorruptIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	src := t.TempDir()
	mustWriteFileCLI(t, filepath.Join(src, "x"), []byte("x"))
	_, _, err := runDaft(t, workspaceCmdName, serveSubName,
		"--workspace=ws="+src, configDirArg, dir,
		"--addr=127.0.0.1:0")
	if err == nil {
		t.Fatal("expected identity-load error")
	}
}

func TestWorkspaceServeFailsForCorruptPeers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, peersFilename), []byte("not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	src := t.TempDir()
	mustWriteFileCLI(t, filepath.Join(src, "x"), []byte("x"))
	_, _, err := runDaft(t, workspaceCmdName, serveSubName,
		"--workspace=ws="+src, configDirArg, dir,
		"--addr=127.0.0.1:0")
	if err == nil {
		t.Fatal("expected peer-store error")
	}
}

func TestWorkspaceServeFailsForBadAddr(t *testing.T) {
	dir := t.TempDir()
	src := t.TempDir()
	mustWriteFileCLI(t, filepath.Join(src, "x"), []byte("x"))
	_, _, err := runDaft(t, workspaceCmdName, serveSubName,
		"--workspace=ws="+src, configDirArg, dir,
		"--addr=not-an-addr")
	if err == nil {
		t.Fatal("expected addr-parse error")
	}
}

func TestWorkspacePullFailsAgainstUnknownAddr(t *testing.T) {
	_, clientDir := approveBoth(t)
	_, _, err := runDaft(t, workspaceCmdName, pullSubName,
		"--from", "127.0.0.1:1",
		"--workspace", "ws",
		"--timeout", "500ms",
		configDirArg, clientDir,
		t.TempDir())
	if err == nil {
		t.Fatal("expected error pulling from a closed port")
	}
}

// syncBuf is a goroutine-safe buffer; bytes.Buffer is not safe for
// concurrent Write/Read which we need across the goroutines that
// run cmd.Execute and the polling loop above.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// keep linter happy about identity import for direct generation in
// other tests in this package.
var _ = identity.RoleClient

func mustWriteFileCLI(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
