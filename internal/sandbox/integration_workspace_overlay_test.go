//go:build linux && integration

package sandbox

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/agent"
	"github.com/richiejp/darkaifactory/internal/imagestore"
)

// TestWorkspaceOverlayE2E boots a real VM with the workspace share
// wrapped in a tmpfs-backed overlayfs (Slice 5b), runs a real
// container that writes /workspace/out.txt, and asserts:
//
//   - The container's write succeeded (the agent's
//     daft-hello-container exits 2 on write failure; we'd see that
//     in the console). The container banner being printed is the
//     positive signal.
//   - The underlying workspace dir on the sandbox-host is
//     untouched: in.txt is still the original bytes, out.txt does
//     NOT appear on the host because writes landed in the in-VM
//     tmpfs upper layer and were torn down with the VM.
//
// That second assertion is the whole point of overlay mode: it
// isolates container writes from the source directory so the
// user-host gets to decide when (or whether) those changes flow
// back.
func TestWorkspaceOverlayE2E(t *testing.T) {
	cfg := resolveArtifacts(t)

	workspaceDir := t.TempDir()
	const inputBody = "overlay-isolated-via-real-vm"
	mustWriteFile(t, filepath.Join(workspaceDir, "in.txt"), []byte(inputBody))
	if err := os.Chmod(workspaceDir, 0o777); err != nil { //nolint:gosec // see TestWorkspaceRoundTripE2E.
		t.Fatalf("Chmod workspaceDir: %v", err)
	}
	if err := os.Chmod(filepath.Join(workspaceDir, "in.txt"), 0o666); err != nil { //nolint:gosec // see TestWorkspaceRoundTripE2E.
		t.Fatalf("Chmod in.txt: %v", err)
	}

	_, ref := pushHelloIntegrationImage(t)
	store := newImagestore(t)
	pulled, err := store.Pull(t.Context(), ref)
	if err != nil {
		t.Fatalf("imagestore.Pull: %v", err)
	}
	bundle, err := store.Bundle(t.Context(), pulled.Digest, imagestore.BundleOptions{
		ExtraMounts: []imagestore.BundleMount{{
			Destination: "/workspace",
			Type:        "bind",
			Source:      "/workspace",
			Options:     []string{"rbind", "rw"},
		}},
	})
	if err != nil {
		t.Fatalf("imagestore.Bundle: %v", err)
	}

	cfg.Shares = []*ShareConfig{
		{
			HostDir:    filepath.Dir(bundle.Path),
			Tag:        "bundles",
			Mountpoint: "/bundles",
		},
		{
			HostDir:    workspaceDir,
			Tag:        "workspace",
			Mountpoint: "/workspace",
			Overlay:    true,
		},
	}
	cfg.Bundle = filepath.Join("/bundles", filepath.Base(bundle.Path))

	var console bytes.Buffer
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("--- console ---\n%s", console.String())
		}
	})
	if err := Hello(traceCtx(t), &console, cfg); err != nil {
		t.Fatalf("Hello: %v", err)
	}

	// The container banner is the only positive signal we have
	// that crun executed AND the in-VM write didn't trip
	// daft-hello-container's error path.
	if !strings.Contains(console.String(),
		agent.ContainerReportPrefix+agent.ContainerBanner) {
		t.Errorf("container banner missing — overlay setup likely broke container")
	}
	if strings.Contains(console.String(), "workspace round-trip error") {
		t.Errorf("container reported a workspace error")
	}

	outPath := filepath.Join(workspaceDir, "out.txt")
	if _, err := os.Stat(outPath); !os.IsNotExist(err) {
		t.Errorf("out.txt leaked to host workspace dir (err=%v) — overlay didn't isolate writes", err)
	}
	if got := mustRead(t, filepath.Join(workspaceDir, "in.txt")); got != inputBody {
		t.Errorf("in.txt mutated by container: got %q, want %q", got, inputBody)
	}
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

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // t.TempDir-rooted.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(b)
}
