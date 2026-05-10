package sandboxdcli

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/cas"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/mtls"
	"github.com/richiejp/darkaifactory/internal/workspace"
)

// TestPullWorkspaceFromHappyPath drives the full mTLS-dial + pull
// pipeline against an in-process workspace serve. This is the
// closest unit-test approximation of what daft-sandboxd does over
// the wire when --workspace-from is set.
func TestPullWorkspaceFromHappyPath(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "alpha.txt"), []byte("alpha\n"))
	mustWriteFile(t, filepath.Join(src, "subdir", "beta.txt"), []byte("beta\n"))

	serverDir, sandboxdDir := approveBothSandboxdSides(t)
	addr := startWorkspaceServe(t, serverDir, src)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir, cleanup, err := pullWorkspaceFrom(ctx, pullWorkspaceOpts{
		addr: addr, name: "ws", configDir: sandboxdDir,
	})
	if err != nil {
		t.Fatalf("pullWorkspaceFrom: %v", err)
	}
	defer cleanup()

	got, err := os.ReadFile(filepath.Join(dir, "alpha.txt")) //nolint:gosec // dir is t.TempDir().
	if err != nil {
		t.Fatalf("read alpha.txt: %v", err)
	}
	if string(got) != "alpha\n" {
		t.Errorf("alpha.txt = %q, want %q", got, "alpha\n")
	}
	gotBeta, err := os.ReadFile(filepath.Join(dir, "subdir", "beta.txt")) //nolint:gosec // dir is t.TempDir().
	if err != nil {
		t.Fatalf("read beta.txt: %v", err)
	}
	if string(gotBeta) != "beta\n" {
		t.Errorf("beta.txt = %q, want %q", gotBeta, "beta\n")
	}
}

// TestPullWorkspaceFromMkdirTempError forces os.MkdirTemp to fail
// by pointing TMPDIR at a path that doesn't exist. Covers the
// rare-error branch between the GetManifest success and the
// ApplyManifest call.
func TestPullWorkspaceFromMkdirTempError(t *testing.T) {
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "alpha.txt"), []byte("alpha\n"))

	srvDir, sbxDir := approveBothSandboxdSides(t)
	addr := startWorkspaceServe(t, srvDir, src)

	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does", "not", "exist"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := pullWorkspaceFrom(ctx, pullWorkspaceOpts{
		addr: addr, name: "ws", configDir: sbxDir,
	})
	if err == nil {
		t.Fatal("expected error from MkdirTemp under unwritable TMPDIR")
	}
}

// approveBothSandboxdSides sets up server + sandboxd config dirs
// with mutual peer approval. Returns the two dir paths.
func approveBothSandboxdSides(t *testing.T) (serverDir, sandboxdDir string) {
	t.Helper()
	serverDir = t.TempDir()
	sandboxdDir = t.TempDir()
	srvID, err := identity.LoadOrGenerate(serverDir, identity.RoleClient)
	if err != nil {
		t.Fatalf("server identity: %v", err)
	}
	sbxID, err := loadOrGenerateSandboxd(sandboxdDir)
	if err != nil {
		t.Fatalf("sandboxd identity: %v", err)
	}
	srvPeers, err := identity.OpenPeerStore(filepath.Join(serverDir, peersFilename))
	if err != nil {
		t.Fatalf("server peers: %v", err)
	}
	sbxPeers, err := openSandboxdPeerStore(sandboxdDir)
	if err != nil {
		t.Fatalf("sandboxd peers: %v", err)
	}
	if err := srvPeers.Add(identity.Peer{Fingerprint: sbxID.Fingerprint()}); err != nil {
		t.Fatalf("server approve: %v", err)
	}
	if err := sbxPeers.Add(identity.Peer{Fingerprint: srvID.Fingerprint()}); err != nil {
		t.Fatalf("sandboxd approve: %v", err)
	}
	return serverDir, sandboxdDir
}

// startWorkspaceServe boots an in-process workspace.Server backed
// by an mtls.Server. Returns the server's listen address. The
// listener is shut down on test cleanup.
func startWorkspaceServe(t *testing.T, serverDir, src string) string {
	t.Helper()
	srvID, err := identity.LoadOrGenerate(serverDir, identity.RoleClient)
	if err != nil {
		t.Fatalf("server identity: %v", err)
	}
	srvPeers, err := identity.OpenPeerStore(filepath.Join(serverDir, peersFilename))
	if err != nil {
		t.Fatalf("server peers: %v", err)
	}
	store, err := cas.Open(filepath.Join(serverDir, "cas"))
	if err != nil {
		t.Fatalf("cas.Open: %v", err)
	}
	manifest, err := workspace.BuildManifest(src, store, workspace.BuildOptions{})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	wsServer, err := workspace.NewServer(store)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := wsServer.Add("ws", manifest); err != nil {
		t.Fatalf("Add: %v", err)
	}
	mux := http.NewServeMux()
	path, handler := workspacev1connect.NewWorkspaceServiceHandler(wsServer)
	mux.Handle(path, handler)
	srv, err := mtls.New(mtls.Config{
		Identity: srvID, Peers: srvPeers, Addr: freeAddr(t), Handler: mux,
	})
	if err != nil {
		t.Fatalf("mtls.New: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() {
		if err := srv.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv.Addr()
}

// freeAddr is identical to the cli package's helper; duplicated
// here to avoid the cross-package import.
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

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}
