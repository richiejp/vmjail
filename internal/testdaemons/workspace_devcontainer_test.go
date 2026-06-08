//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/localai-org/darkaifactory/internal/testdaemons"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// TestClusterWorkspaceDevcontainerBoots is the #101 end-to-end proof: a
// workspace with NO explicit image, only a `devcontainer.json` in its
// content, boots the image that devcontainer names and applies its env —
// resolved and overlaid node-side at dispatch (slices 1–2d).
//
// The assertion is deliberately load-bearing on the whole chain. The
// image's entrypoint echoes $DC_MARKER, which is set ONLY by the
// devcontainer's containerEnv — never by the image or any explicit
// workspace env. So the marker line appears only if the node:
//  1. read .devcontainer/devcontainer.json from the materialised content,
//  2. resolved it to the image ref (there is no explicit image_ref, so a
//     failure here boots the bare-Hello fallback and the marker never
//     prints), and
//  3. overlaid the containerEnv into the boot env.
//
// A regression in any of those collapses the marker to empty or absent,
// so this one waited-for line covers the full devcontainer→boot path.
func TestClusterWorkspaceDevcontainerBoots(t *testing.T) {
	imgPath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")

	const dcMarker = "dc-e2e-overlay-7f3a"
	// The image entrypoint echoes the devcontainer-supplied env so the
	// captured sandboxd log proves the overlay end-to-end; the trailing
	// sleep keeps the container alive.
	const script = "echo DC_MARKER=$DC_MARKER; exec sleep infinity"

	tarPath := decompressDockerTarball(t, imgPath)
	base, err := tarball.ImageFromPath(tarPath, nil)
	if err != nil {
		t.Fatalf("load image %s: %v", tarPath, err)
	}
	img := withUserAndEntrypoint(t, base, "", []string{"/bin/dash", "-c", script})
	ref := pushImageToRegistry(t, img, "test/d101-devcontainer:v1")

	// The workspace content: a .devcontainer/devcontainer.json that names
	// the image and sets the marker env. NO image is set on the workspace
	// itself — the devcontainer is the only image source.
	content := t.TempDir()
	dcDir := filepath.Join(content, ".devcontainer")
	if err := os.MkdirAll(dcDir, 0o750); err != nil {
		t.Fatalf("mkdir .devcontainer: %v", err)
	}
	dcJSON := fmt.Sprintf(`{
  "name": "d101-e2e",
  "image": %q,
  "containerEnv": { "DC_MARKER": %q }
}`, ref, dcMarker)
	if err := os.WriteFile(filepath.Join(dcDir, "devcontainer.json"), []byte(dcJSON), 0o600); err != nil {
		t.Fatalf("write devcontainer.json: %v", err)
	}

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	wsName := "d101-devcontainer"
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D101", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf(
			"%s workspace create --name %s %s", prefix, wsName, orch,
		)},
		// Direct host-bind the content dir at /workspace; the
		// devcontainer.json lives under it, so the node finds it there.
		{Label: "mount", Cmd: fmt.Sprintf(
			"%s workspace mount add /workspace --source %s --name %s %s",
			prefix, content, wsName, orch,
		)},
		// The only image source: a content-relative devcontainer path.
		{Label: "devcontainer", Cmd: fmt.Sprintf(
			"%s workspace devcontainer .devcontainer/devcontainer.json --name %s %s",
			prefix, wsName, orch,
		)},
		{Label: "target", Cmd: fmt.Sprintf(
			"%s workspace target %s --name %s %s", prefix, c.SandboxdFP, wsName, orch,
		)},
		{Label: "launch", Cmd: fmt.Sprintf(
			"%s workspace launch --name %s %s", prefix, wsName, orch,
		)},
	}, 30*time.Second)

	// The marker line proves the devcontainer was read, its image booted,
	// and its env was overlaid — all node-side, from content alone.
	c.WaitForSandboxdOutput(t, "DC_MARKER="+dcMarker, 60*time.Second)
}
