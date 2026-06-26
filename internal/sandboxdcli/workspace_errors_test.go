package sandboxdcli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/mtls"
)

func TestWorkspaceServeBadConfigDir(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	err := runWorkspaceServe(context.Background(), nopWriter{},
		t.TempDir(), "ws", "127.0.0.1:0", "")
	if err == nil {
		t.Fatal("expected error for unresolvable config dir")
	}
}

func TestWorkspaceServeCorruptIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	err := runWorkspaceServe(context.Background(), nopWriter{},
		t.TempDir(), "ws", "127.0.0.1:0", dir)
	if err == nil {
		t.Fatal("expected error for corrupt identity")
	}
}

func TestWorkspaceServeCorruptPeerStore(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadOrGenerateSandboxd(dir); err != nil {
		t.Fatalf("loadOrGenerate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, peersFilename), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write peers: %v", err)
	}
	err := runWorkspaceServe(context.Background(), nopWriter{},
		t.TempDir(), "ws", "127.0.0.1:0", dir)
	if err == nil {
		t.Fatal("expected error for corrupt peer store")
	}
}

func TestWorkspaceServeBuildManifestErrorMissingPath(t *testing.T) {
	dir := t.TempDir()
	err := runWorkspaceServe(context.Background(), nopWriter{},
		"/definitely/does/not/exist", "ws", "127.0.0.1:0", dir)
	if err == nil {
		t.Fatal("expected error for missing src dir")
	}
}

func TestWorkspaceServeBadListenAddr(t *testing.T) {
	dir := t.TempDir()
	src := t.TempDir()
	mustWriteFile(t, filepath.Join(src, "a"), []byte("a"))
	err := runWorkspaceServe(context.Background(), nopWriter{},
		src, "ws", "not-an-addr", dir)
	if err == nil {
		t.Fatal("expected error for malformed addr")
	}
}

// TestRunUntilCancelServeError forces the Serve-error branch of
// runUntilCancel by shutting the server externally before
// runUntilCancel sees its select{}. Mirrors the equivalent test in
// the cli package, kept here to avoid the cross-package import.
func TestRunUntilCancelServeError(t *testing.T) {
	id, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	store, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	srv, err := mtls.New(mtls.Config{
		Identity: id, Peers: store, Addr: "127.0.0.1:0", Handler: http.NewServeMux(),
	})
	if err != nil {
		t.Fatalf("mtls.New: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()
	if err := runUntilCancel(context.Background(), srv); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("runUntilCancel: unexpected non-Closed error: %v", err)
	}
}
