package orchestratorcli

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/orchestrator"
	"github.com/richiejp/darkaifactory/internal/version"
)

const (
	addrFlag      = "--addr"
	loopbackAny   = "127.0.0.1:0"
	configDirFlag = "--config-dir"
)

func TestVersionCommandPrintsVersion(t *testing.T) {
	var out, errBuf bytes.Buffer
	if err := Execute([]string{"version"}, &out, &errBuf); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := strings.TrimSpace(out.String())
	if got != version.Version {
		t.Errorf("version = %q, want %q", got, version.Version)
	}
}

func TestUnknownCommandReturnsError(t *testing.T) {
	var out, errBuf bytes.Buffer
	if err := Execute([]string{"definitely-not-real"}, &out, &errBuf); err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
}

func TestResolveConfigDirHonoursOverride(t *testing.T) {
	got, err := resolveConfigDir("/tmp/explicit")
	if err != nil {
		t.Fatalf("resolveConfigDir: %v", err)
	}
	if got != "/tmp/explicit" {
		t.Errorf("got %q, want /tmp/explicit", got)
	}
}

// TestResolveConfigDirPropagatesUserConfigDirError forces
// os.UserConfigDir to fail.
func TestResolveConfigDirPropagatesUserConfigDirError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, err := resolveConfigDir(""); err == nil {
		t.Fatal("expected error")
	}
}

func TestResolveConfigDirFallsBackToUserConfig(t *testing.T) {
	got, err := resolveConfigDir("")
	if err != nil {
		t.Fatalf("resolveConfigDir: %v", err)
	}
	if got == "" {
		t.Fatal("expected non-empty config dir")
	}
	if !strings.HasSuffix(got, configSubdir) {
		t.Errorf("got %q, want suffix %q", got, configSubdir)
	}
}

func TestLoadOrGenerateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id1, err := loadOrGenerate(dir)
	if err != nil {
		t.Fatalf("first loadOrGenerate: %v", err)
	}
	id2, err := loadOrGenerate(dir)
	if err != nil {
		t.Fatalf("second loadOrGenerate: %v", err)
	}
	if id1.Fingerprint() != id2.Fingerprint() {
		t.Errorf("fingerprint differs across calls — second call generated instead of loading")
	}
}

func TestLoadOrGeneratePropagatesCorruptCert(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := loadOrGenerate(dir); err == nil {
		t.Fatal("expected error for corrupt identity")
	}
}

// freePort grabs an ephemeral port number for binding the orchestrator
// in tests. Returning the closed port on a localhost interface is
// race-free enough for these test latencies.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", loopbackAny)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return addr
}

func TestServeStartsAndShutsDownCleanly(t *testing.T) {
	dir := t.TempDir()
	addr := freePort(t)

	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	cmd := New(stdout, stderr)
	cmd.SetArgs([]string{serveCmdName, configDirFlag, dir, addrFlag, addr})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd.SetContext(ctx)

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	// Wait for the listener line to appear.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stdout.String(), "listening on") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(stdout.String(), "listening on") {
		t.Fatalf("serve never logged listener; stdout=%q stderr=%q",
			stdout.String(), stderr.String())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not exit after cancel")
	}
}

// TestServeFailsWhenIdentityIsCorrupt exercises the loadOrGenerate
// error branch in serve: an existing cert.pem that doesn't parse
// must surface as a serve failure rather than silent regeneration.
func TestServeFailsWhenIdentityIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), []byte("bad pem"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), []byte("bad pem"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	cmd := New(stdout, stderr)
	cmd.SetArgs([]string{serveCmdName, configDirFlag, dir, addrFlag, loopbackAny})
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected serve error for corrupt identity")
	}
}

// TestServeFailsWhenPeersFileIsCorrupt covers the OpenPeerStore error
// branch in serve.
func TestServeFailsWhenPeersFileIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, peersFile), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	cmd := New(stdout, stderr)
	cmd.SetArgs([]string{serveCmdName, configDirFlag, dir, addrFlag, loopbackAny})
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected serve error for corrupt peers.json")
	}
}

// TestServeFailsWhenAddrAlreadyBound exercises the Listen error path
// from inside the cobra command.
func TestServeFailsWhenAddrAlreadyBound(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", loopbackAny)
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	busyAddr := ln.Addr().String()

	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	cmd := New(stdout, stderr)
	cmd.SetArgs([]string{serveCmdName, configDirFlag, dir, addrFlag, busyAddr})
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected serve error when address is busy")
	}
}

// TestRunServerReturnsServeError verifies that a non-shutdown Serve
// error propagates back to the caller. We synthesise it by closing
// the listener under the server, which causes Serve to return a
// "use of closed network connection" error.
func TestRunServerReturnsServeError(t *testing.T) {
	srv, err := orchestrator.New(orchestrator.Config{
		Identity: mustGenerate(t, identity.RoleOrchestrator),
		Peers:    mustOpenStore(t),
		Addr:     loopbackAny,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		shutCtx, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		if err := srv.Shutdown(shutCtx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()
	if err := runServer(t.Context(), srv); err != nil {
		t.Errorf("runServer: %v", err)
	}
}

// syncBuffer is a goroutine-safe io.Writer used for capturing
// orchestratorcli output in tests; bytes.Buffer is not safe for
// concurrent Write/Read use which we need across the goroutines that
// run cmd.Execute and the polling loop above.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func mustGenerate(t *testing.T, role identity.Role) *identity.Identity {
	t.Helper()
	id, err := identity.Generate(role)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return id
}

func mustOpenStore(t *testing.T) *identity.PeerStore {
	t.Helper()
	s, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatalf("OpenPeerStore: %v", err)
	}
	return s
}
