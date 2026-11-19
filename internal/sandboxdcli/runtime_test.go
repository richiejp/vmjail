package sandboxdcli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richiejp/vmjail/internal/bundled"
	"github.com/richiejp/vmjail/internal/sandbox"
)

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{2048, "2.0 KiB"},
		{16 << 30, "16.0 GiB"},
	}
	for _, tc := range cases {
		if got := formatBytes(tc.in); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestResolveVsockDirDefaultCreatesTempSubdir(t *testing.T) {
	t.Setenv("RUNTIME_DIRECTORY", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	dir, cleanup, err := resolveVsockDir("")
	if err != nil {
		t.Fatalf("resolveVsockDir(\"\"): %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if !info.IsDir() {
		t.Errorf("%s is not a directory", dir)
	}
	cleanup()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("owned runtime dir still exists after cleanup: %v", err)
	}
}

func TestResolveVsockDirPrefersSystemdRuntimeDirectory(t *testing.T) {
	runtimeDir := filepath.Join(os.TempDir(), fmt.Sprintf("vmjail-systemd-%d", os.Getpid()))
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	t.Setenv("RUNTIME_DIRECTORY", runtimeDir)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dir, cleanup, err := resolveVsockDir("")
	if err != nil {
		t.Fatalf("resolveVsockDir: %v", err)
	}
	defer cleanup()
	want := filepath.Join(runtimeDir, "vsock")
	if dir != want {
		t.Errorf("runtime dir = %q, want %q", dir, want)
	}
}

func TestResolveVsockDirUsesXDGRuntimeDirectory(t *testing.T) {
	runtimeDir := filepath.Join(os.TempDir(), fmt.Sprintf("vmjail-xdg-%d", os.Getpid()))
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	t.Setenv("RUNTIME_DIRECTORY", "relative")
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	dir, cleanup, err := resolveVsockDir("")
	if err != nil {
		t.Fatalf("resolveVsockDir: %v", err)
	}
	defer cleanup()
	want := filepath.Join(runtimeDir, "vmjail-vsock")
	if dir != want {
		t.Errorf("runtime dir = %q, want %q", dir, want)
	}
	// A build VM adds a UUID, "-build.sock", and a hybrid-vsock port
	// suffix. Keep the normal Linux user-runtime path inside sockaddr_un.
	worstSocket := filepath.Join(dir, strings.Repeat("a", 36)+"-build.sock_7000")
	if len(worstSocket) >= 108 {
		t.Errorf("default hybrid-vsock path is %d bytes, must be below 108: %q", len(worstSocket), worstSocket)
	}
}

func TestResolveVsockDirRejectsPathTooLongForSockets(t *testing.T) {
	dir := filepath.Join(os.TempDir(), strings.Repeat("x", 80))
	if _, _, err := resolveVsockDir(dir); err == nil {
		t.Fatal("expected error for a vsock directory that leaves no socket-name budget")
	}
}

func TestResolveVsockDirAbsoluteOverride(t *testing.T) {
	override := filepath.Join(os.TempDir(), fmt.Sprintf("vmjail-override-%d", os.Getpid()))
	t.Cleanup(func() { _ = os.RemoveAll(override) })
	got, cleanup, err := resolveVsockDir(override)
	if err != nil {
		t.Fatalf("resolveVsockDir: %v", err)
	}
	if got != override {
		t.Errorf("resolveVsockDir = %q, want %q", got, override)
	}
	if _, err := os.Stat(override); err != nil {
		t.Errorf("dir not created: %v", err)
	}
	cleanup()
	if _, err := os.Stat(override); err != nil {
		t.Errorf("explicit directory was removed: %v", err)
	}
}

func TestResolveVsockDirRejectsRelative(t *testing.T) {
	if _, _, err := resolveVsockDir("relative/path"); err == nil {
		t.Fatal("expected error for relative override")
	}
}

func TestResolveVsockDirMkdirError(t *testing.T) {
	dir := t.TempDir()
	blocker := dir + "/notadir"
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	if _, _, err := resolveVsockDir(blocker + "/sub"); err == nil {
		t.Fatal("expected error when mkdir fails")
	}
}

func TestExtractWithEnvFallbackUsesBaseWhenStubAssets(t *testing.T) {
	base := sandbox.Config{
		CloudHypervisor: "/p/ch",
		Kernel:          "/p/kernel",
		Initramfs:       "/p/initramfs",
		Virtiofsd:       "/p/virtiofsd",
	}
	paths, err := extractWithEnvFallback(base)(t.TempDir())
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if paths.CloudHypervisor != base.CloudHypervisor ||
		paths.Kernel != base.Kernel ||
		paths.Initramfs != base.Initramfs ||
		paths.Virtiofsd != base.Virtiofsd {
		t.Errorf("paths = %+v, want %+v", paths, base)
	}
}

func TestExtractWithEnvFallbackReturnsErrNotBundledWhenBaseEmpty(t *testing.T) {
	_, err := extractWithEnvFallback(sandbox.Config{})(t.TempDir())
	if !errors.Is(err, bundled.ErrNotBundled) {
		t.Errorf("err = %v, want ErrNotBundled", err)
	}
}

func TestObserveSummaryFromEnv(t *testing.T) {
	t.Run("unset enables by default", func(t *testing.T) {
		t.Setenv(envObserveSummary, "")
		if !observeSummaryFromEnv() {
			t.Error("observe summary should be on by default")
		}
	})
	t.Run("falsy opts out", func(t *testing.T) {
		// Exercises trim + lower-casing across the falsy synonyms. Variants
		// ("False"/"OFF"/" off ") are chosen so the exact literals "false"
		// and "off" don't recur package-wide and trip goconst — the egress
		// test already covers the canonical forms via the shared helper.
		for _, v := range []string{"0", "False", "no", "OFF", " off "} {
			t.Setenv(envObserveSummary, v)
			if observeSummaryFromEnv() {
				t.Errorf("%q should disable observe summary", v)
			}
		}
	})
	t.Run("other values stay on", func(t *testing.T) {
		for _, v := range []string{"disabled", "nope", "2", "nonsense"} {
			t.Setenv(envObserveSummary, v)
			if !observeSummaryFromEnv() {
				t.Errorf("%q should leave observe summary on", v)
			}
		}
	})
}
