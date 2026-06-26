//go:build linux && integration

package testdaemons_test

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/richiejp/darkaifactory/internal/testdaemons"
	"github.com/richiejp/darkaifactory/internal/testtmux"
)

// TestClusterWorkspaceBuildahRunStep is the #101 slice-3a follow-up
// spike: prove a Dockerfile RUN step executes inside a daft sandbox —
// the part of a real build the FROM-scratch+COPY spike deliberately
// avoided. RUN is the riskiest Dockerfile instruction here: under
// buildah's chroot isolation it mounts /proc//dev//sys into the build
// rootfs and execs the command there, exercising the privileged-build
// grant's CAP_SYS_ADMIN (mounts) on top of the CAP_SYS_CHROOT the COPY
// path already proved.
//
// The base-image question is deliberately decoupled from network egress:
// RUN needs a rootfs with a shell, but pulling FROM a registry needs
// egress — a separate spike. Instead the build seeds buildah's storage
// from a LOCAL docker-archive (the nix-shell image the harness already
// ships, mounted read-only), tags it localhost/base:v1, and builds FROM
// that. The RUN uses exec form with /bin/dash because the nix image
// ships no /bin/sh (the same gotcha the #100 test hit).
//
// The proof is end-to-end: the host loads the exported image and
// extracts the RUN-created file from its layers — a sentinel alone can't
// fake that the layer really carries the file.
func TestClusterWorkspaceBuildahRunStep(t *testing.T) {
	buildImgPath := requireEnvPath(t, "DAFT_BUILD_IMAGE",
		"nix build .#daft-build-image -o bin/daft-build-image")
	baseImgPath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")

	const (
		okSentinel   = "BUILDAH-RUN-OK"
		failSentinel = "BUILDAH-RUN-FAIL"
		runProof     = "d101-run-proof"
	)
	// pull(local archive) → tag → build(RUN) → push. Storage on the
	// writable tmpfs (the rootfs is read-only); OK/FAIL reaches sandboxd
	// stdout via the agent; --version is the FAIL-path breadcrumb.
	script := fmt.Sprintf(
		"set -e; export HOME=/tmp; mkdir -p /tmp/cstore /tmp/crun; cd /build/ctx; "+
			"if base=$(buildah --root /tmp/cstore --runroot /tmp/crun pull docker-archive:/build/base/base.tar) && "+
			"buildah --root /tmp/cstore --runroot /tmp/crun tag \"$base\" localhost/base:v1 && "+
			"buildah --root /tmp/cstore --runroot /tmp/crun build --isolation chroot --storage-driver vfs -t runspike -f Dockerfile . && "+
			"buildah --root /tmp/cstore --runroot /tmp/crun push runspike docker-archive:/build/out/image.tar; "+
			"then echo %s; else echo %s; buildah --version; fi; exec sleep infinity",
		okSentinel, failSentinel,
	)

	buildTar := decompressDockerTarball(t, buildImgPath)
	builder, err := tarball.ImageFromPath(buildTar, nil)
	if err != nil {
		t.Fatalf("load build image %s: %v", buildTar, err)
	}
	img := withUserAndEntrypoint(t, builder, "", []string{"/bin/dash", "-c", script})
	ref := pushImageToRegistry(t, img, "test/d101-buildah-run:v1")

	// The base image the in-sandbox build FROMs, delivered as a local
	// docker-archive on a read-only mount.
	baseDir := t.TempDir()
	copyFile(t, decompressDockerTarball(t, baseImgPath), filepath.Join(baseDir, "base.tar"))

	// Build context: FROM the seeded base + an exec-form RUN that writes
	// a file only a really-executed command can produce.
	ctx := t.TempDir()
	writeFile(t, filepath.Join(ctx, "Dockerfile"), fmt.Sprintf(
		"FROM localhost/base:v1\nRUN [\"/bin/dash\", \"-c\", \"echo %s > /run-proof.txt\"]\n",
		runProof,
	))
	out := t.TempDir() // rw, host-owned

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	wsName := "d101-buildah-run"
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D101R", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf(
			"%s workspace create --name %s %s", prefix, wsName, orch,
		)},
		{Label: "image", Cmd: fmt.Sprintf(
			"%s workspace image %s --name %s %s", prefix, ref, wsName, orch,
		)},
		{Label: "ctx", Cmd: fmt.Sprintf(
			"%s workspace mount add /build/ctx --source %s --read-only --name %s %s",
			prefix, ctx, wsName, orch,
		)},
		{Label: "base", Cmd: fmt.Sprintf(
			"%s workspace mount add /build/base --source %s --read-only --name %s %s",
			prefix, baseDir, wsName, orch,
		)},
		{Label: "out", Cmd: fmt.Sprintf(
			"%s workspace mount add /build/out --source %s --name %s %s",
			prefix, out, wsName, orch,
		)},
		{Label: "build-caps", Cmd: fmt.Sprintf(
			"%s workspace privileged-build --enable --name %s %s", prefix, wsName, orch,
		)},
		// vfs copies the whole base rootfs per layer and the storage
		// lives on the RAM-backed /tmp, so the default 256 MiB VM
		// starves; 2 GiB gives a 1 GiB tmpfs for ~4 copies of the
		// ~73 MiB base.
		{Label: "resources", Cmd: fmt.Sprintf(
			"%s workspace resources --memory 2G --cpus 2 --name %s %s", prefix, wsName, orch,
		)},
		{Label: "target", Cmd: fmt.Sprintf(
			"%s workspace target %s --name %s %s", prefix, c.SandboxdFP, wsName, orch,
		)},
		{Label: "launch", Cmd: fmt.Sprintf(
			"%s workspace launch --name %s %s", prefix, wsName, orch,
		)},
	}, 30*time.Second)

	c.WaitForSandboxdOutput(t, okSentinel, 180*time.Second)

	// Host-side: the exported image's flattened filesystem must carry the
	// RUN-created file with the expected content.
	imgTar := filepath.Join(out, "image.tar")
	built, err := tarball.ImageFromPath(imgTar, nil)
	if err != nil {
		t.Fatalf("load built image %s: %v", imgTar, err)
	}
	got := extractFileFromImage(t, built, "run-proof.txt")
	if strings.TrimSpace(got) != runProof {
		t.Fatalf("run-proof.txt = %q, want %q", got, runProof)
	}
}

// extractFileFromImage flattens img's layers (whiteouts applied) and
// returns the content of the first tar entry whose base name matches
// name. Fails the test if the file is absent — for the RUN spike that
// means the RUN layer never materialised.
func extractFileFromImage(t *testing.T, img v1.Image, name string) string {
	t.Helper()
	rc := mutate.Extract(img)
	defer func() { _ = rc.Close() }()
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read flattened image tar: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == name {
			data, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read %s from image: %v", hdr.Name, err)
			}
			return string(data)
		}
	}
	t.Fatalf("file %q not found in the built image's layers", name)
	return ""
}

// copyFile copies src to dst (0600). Plain copy rather than hardlink so
// the two tempdirs needn't share a filesystem.
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src) //nolint:gosec // test-only fixture path.
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatalf("copy %s -> %s: %v", src, dst, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close %s: %v", dst, err)
	}
}
