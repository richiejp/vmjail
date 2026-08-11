//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/richiejp/darkaifactory/internal/testdaemons"
	"github.com/richiejp/darkaifactory/internal/testtmux"
)

// TestClusterWorkspaceBuildahSpike is the #101 slice-3a spike: prove
// buildah can build a Dockerfile and export an image tarball from inside
// a daft sandbox VM — the foundation for build-based devcontainers.
//
// It is deliberately minimal to isolate the buildah-in-sandbox plumbing
// from the harder bits:
//   - FROM scratch + COPY only (NO RUN), so buildah never has to exec or
//     chroot — it just assembles a rootfs and commits, exercising the
//     build→commit→export path with the least privilege.
//   - storage under /tmp (tmpfs): daft mounts the container rootfs
//     READ-ONLY, so buildah's graphroot/runroot can't live at the image's
//     /var/lib/containers; --root/--runroot point them at the writable
//     tmpfs instead.
//   - vfs driver + chroot isolation (baked into daft-build-image), no
//     overlay/fuse, no base-image pull (FROM scratch needs no network).
//
// The built tarball lands on a writable output mount the host reads back
// and validates with go-containerregistry. RUN steps (need exec caps /
// isolation) and a real base image (need egress) are follow-up spikes.
func TestClusterWorkspaceBuildahSpike(t *testing.T) {
	imgPath := requireEnvPath(t, "DAFT_BUILD_IMAGE",
		"nix build .#daft-build-image -o bin/daft-build-image")

	const (
		okSentinel   = "BUILDAH-SPIKE-OK"
		failSentinel = "BUILDAH-SPIKE-FAIL"
	)
	// The entrypoint runs the build, exports a docker-archive to the rw
	// output mount, prints OK/FAIL (which reaches sandboxd stdout via the
	// agent), then sleeps so the VM stays up. buildah --version on the
	// FAIL path leaves a breadcrumb in the captured log.
	script := fmt.Sprintf(
		"set -e; export HOME=/tmp; mkdir -p /tmp/cstore /tmp/crun; cd /build/ctx; "+
			"if buildah --root /tmp/cstore --runroot /tmp/crun build "+
			"--isolation chroot --storage-driver vfs -t spike -f Dockerfile . && "+
			"buildah --root /tmp/cstore --runroot /tmp/crun push spike "+
			"docker-archive:/build/out/image.tar; then echo %s; else echo %s; "+
			"buildah --version; fi; exec sleep infinity",
		okSentinel, failSentinel,
	)

	tarPath := decompressDockerTarball(t, imgPath)
	base, err := tarball.ImageFromPath(tarPath, nil)
	if err != nil {
		t.Fatalf("load build image %s: %v", tarPath, err)
	}
	img := withUserAndEntrypoint(t, base, "", []string{"/bin/dash", "-c", script})
	ref := pushImageToRegistry(t, img, "test/d101-buildah:v1")

	// Build context: a FROM-scratch Dockerfile that just copies a file in.
	ctx := t.TempDir()
	writeFile(t, filepath.Join(ctx, "Dockerfile"), "FROM scratch\nCOPY hello.txt /hello.txt\n")
	writeFile(t, filepath.Join(ctx, "hello.txt"), "d101-buildah-spike\n")
	out := t.TempDir() // rw, host-owned

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	wsName := "d101-buildah"
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D101B", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf(
			"%s workspace create --name %s %s", prefix, wsName, orch,
		)},
		{Label: "image", Cmd: fmt.Sprintf(
			"%s workspace image %s --preserve-command --name %s %s", prefix, ref, wsName, orch,
		)},
		{Label: "ctx", Cmd: fmt.Sprintf(
			"%s workspace mount add /build/ctx --source %s --read-only --name %s %s",
			prefix, ctx, wsName, orch,
		)},
		{Label: "out", Cmd: fmt.Sprintf(
			"%s workspace mount add /build/out --source %s --name %s %s",
			prefix, out, wsName, orch,
		)},
		// The build capability grant: buildah's copier chroots even for
		// COPY (CAP_SYS_CHROOT), which daft's zero-cap default withholds.
		{Label: "build-caps", Cmd: fmt.Sprintf(
			"%s workspace privileged-build --enable --name %s %s", prefix, wsName, orch,
		)},
		{Label: "target", Cmd: fmt.Sprintf(
			"%s workspace target %s --name %s %s", prefix, c.SandboxdFP, wsName, orch,
		)},
		{Label: "launch", Cmd: fmt.Sprintf(
			"%s workspace launch --name %s %s", prefix, wsName, orch,
		)},
	}, 30*time.Second)

	c.WaitForSandboxdOutput(t, okSentinel, 120*time.Second)

	// Host-side: the exported tarball must load as a valid image carrying
	// the COPY'd file's layer.
	imgTar := filepath.Join(out, "image.tar")
	if _, err := os.Stat(imgTar); err != nil {
		t.Fatalf("build output %s absent: %v", imgTar, err)
	}
	built, err := tarball.ImageFromPath(imgTar, nil)
	if err != nil {
		t.Fatalf("load built image %s: %v", imgTar, err)
	}
	layers, err := built.Layers()
	if err != nil || len(layers) == 0 {
		t.Fatalf("built image layers = %d, err=%v; want >=1", len(layers), err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
