package sandboxdcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSandboxdDirHonoursOverride(t *testing.T) {
	got, err := resolveSandboxdDir("/tmp/explicit-sandboxd")
	if err != nil {
		t.Fatalf("resolveSandboxdDir: %v", err)
	}
	if got != "/tmp/explicit-sandboxd" {
		t.Errorf("got %q, want override", got)
	}
}

func TestResolveSandboxdDirRejectsRelativeOverride(t *testing.T) {
	if _, err := resolveSandboxdDir("relative/config"); err == nil {
		t.Fatal("expected relative sandboxd config dir to be rejected")
	}
}

func TestResolveSandboxdDirFallsBackToUserConfig(t *testing.T) {
	got, err := resolveSandboxdDir("")
	if err != nil {
		t.Fatalf("resolveSandboxdDir: %v", err)
	}
	if !strings.HasSuffix(got, sandboxdConfigSubdir) {
		t.Errorf("got %q, want suffix %q", got, sandboxdConfigSubdir)
	}
}

func TestResolveSandboxdDirReportsConfigDirError(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, err := resolveSandboxdDir(""); err == nil {
		t.Fatal("expected error when UserConfigDir cannot resolve")
	}
}

func TestLoadOrGenerateSandboxdRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id1, err := loadOrGenerateSandboxd(dir)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	id2, err := loadOrGenerateSandboxd(dir)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if id1.Fingerprint() != id2.Fingerprint() {
		t.Errorf("identity changed across loads")
	}
}

func TestOpenSandboxdPeerStoreEmptyDir(t *testing.T) {
	dir := t.TempDir()
	store, err := openSandboxdPeerStore(dir)
	if err != nil {
		t.Fatalf("openSandboxdPeerStore: %v", err)
	}
	if store.Path() != filepath.Join(dir, peersFilename) {
		t.Errorf("Path = %q, want suffix %q", store.Path(), peersFilename)
	}
}

func TestOpenSandboxdPeerStoreCorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, peersFilename), []byte("not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := openSandboxdPeerStore(dir); err == nil {
		t.Fatal("expected error for corrupt peer store")
	}
}
