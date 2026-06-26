//go:build linux && integration

package testdaemons_test

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/richiejp/darkaifactory/internal/testdaemons"
	"github.com/richiejp/darkaifactory/internal/testtmux"
)

// TestClusterWorkspaceExecAgainstNixImage is the D6b.3 proof:
// load a Nix-built dash+coreutils OCI image from DAFT_NIX_IMAGE,
// push it to an in-process OCI registry the sandboxd can pull
// from, drive the full `daft workspace create / image / mount /
// target / launch` sequence through the tmux pane, wait for the
// container's readiness sentinel to surface on the sandboxd's
// stdout, then drive `daft workspace exec` and assert the
// in-container output appears in the pane.
//
// This is the first slice that exercises the attach + exec path
// end-to-end through the daft CLI as a user would drive it.
// D6b.4 will swap the image for a full nix toolchain so a dev
// server can run inside the container.
func TestClusterWorkspaceExecAgainstNixImage(t *testing.T) {
	imgPath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")
	ref := pushDockerTarballToRegistry(t, imgPath, "test/d6b3-nix:v1")

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	mountSrc := t.TempDir()
	wsName := "d6b3-nix"
	prefix := fmt.Sprintf("%s --config-dir %s",
		c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D6B3", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf(
			"%s workspace create --name %s %s",
			prefix, wsName, orch,
		)},
		{Label: "image", Cmd: fmt.Sprintf(
			"%s workspace image %s --name %s %s",
			prefix, ref, wsName, orch,
		)},
		{Label: "mount", Cmd: fmt.Sprintf(
			"%s workspace mount add /workspace --source %s --name %s %s",
			prefix, mountSrc, wsName, orch,
		)},
		{Label: "target", Cmd: fmt.Sprintf(
			"%s workspace target %s --name %s %s",
			prefix, c.SandboxdFP, wsName, orch,
		)},
		{Label: "launch", Cmd: fmt.Sprintf(
			"%s workspace launch --name %s %s",
			prefix, wsName, orch,
		)},
	}, 30*time.Second)

	// The nix-shell-image entrypoint runs
	//     echo daft-nix-shell ready; exec sleep infinity
	// inside the container, so this string surfaces on sandboxd
	// stdout (container stdout → agent → vsock → sandboxd)
	// once the VM has booted and crun has launched the container.
	// It's the readiness gate before exec — the attach route is
	// registered earlier, but dialing it before the container is
	// up would race.
	c.WaitForSandboxdOutput(t, "daft-nix-shell ready", 60*time.Second)

	// Drive `daft workspace exec` against the running container.
	// --tty=false skips the local raw-mode dance (the pane is a
	// TTY, but we don't need interactive editing for a one-shot
	// `echo`) and keeps the pane buffer clean. The inner dash
	// computes the sentinel via arithmetic so the literal
	// "D6B3-EXEC-6000" appears only in the container's output —
	// the typed line, single-quoted to defer expansion, still
	// reads as $((6000+$?)) in the pane echo.
	execCmd := fmt.Sprintf(
		"%s workspace exec %s --tty=false %s -- "+
			`/bin/dash -c 'echo D6B3-EXEC-$((6000+$?))'`,
		prefix, wsName, orch,
	)
	s.Enter(execCmd)
	s.WaitFor("D6B3-EXEC-6000", 60*time.Second)
}

// requireEnvPath fatals with a clear pointer when the named env
// var is missing or its target doesn't exist. The dev shell
// exports paths to the flake-realised image tarballs; outside the
// dev shell, the test author has to nix-build the image manually.
// hint is interpolated into the failure message as the suggested
// `nix build` invocation.
func requireEnvPath(t *testing.T, envVar, hint string) string {
	t.Helper()
	p := os.Getenv(envVar)
	if p == "" {
		t.Fatalf("testdaemons: %s not set "+
			"(enter `nix develop` or run `%s`)",
			envVar, hint)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("testdaemons: stat %s=%q: %v", envVar, p, err)
	}
	return p
}

// pushDockerTarballToRegistry loads the docker-format tarball at
// imgPath into a v1.Image, stands up an in-process OCI registry
// on a free loopback port, pushes the image under refName (e.g.
// "test/d6b3-nix:v1"), and returns the full ref the sandboxd can
// pull from.
//
// dockerTools.buildLayeredImage emits a gzipped tarball, but
// go-containerregistry's tarball.ImageFromPath / crane.Load only
// read raw tar — so we decompress into a temp file first.
func pushDockerTarballToRegistry(t *testing.T, imgPath, refName string) string {
	t.Helper()

	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse registry URL: %v", err)
	}

	tarPath := decompressDockerTarball(t, imgPath)
	img, err := tarball.ImageFromPath(tarPath, nil)
	if err != nil {
		t.Fatalf("load image %s: %v", tarPath, err)
	}

	ref := u.Host + "/" + refName
	r, err := name.ParseReference(ref)
	if err != nil {
		t.Fatalf("parse ref: %v", err)
	}
	if err := crane.Push(img, r.String()); err != nil {
		t.Fatalf("push image %s: %v", ref, err)
	}
	return ref
}

// decompressDockerTarball reads gzPath (a .tar.gz produced by
// dockerTools.buildLayeredImage), gunzips it into a sibling file
// inside t.TempDir(), and returns the new path. Cleanup is
// handled by t.TempDir.
func decompressDockerTarball(t *testing.T, gzPath string) string {
	t.Helper()
	in, err := os.Open(gzPath) //nolint:gosec // gzPath is DAFT_NIX_IMAGE; test-only.
	if err != nil {
		t.Fatalf("open %s: %v", gzPath, err)
	}
	defer func() { _ = in.Close() }()

	gz, err := gzip.NewReader(in)
	if err != nil {
		t.Fatalf("gzip reader %s: %v", gzPath, err)
	}
	defer func() { _ = gz.Close() }()

	tarPath := filepath.Join(t.TempDir(), "nix-shell-image.tar")
	out, err := os.Create(tarPath) //nolint:gosec // tarPath is under t.TempDir; test-only.
	if err != nil {
		t.Fatalf("create %s: %v", tarPath, err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, gz); err != nil {
		t.Fatalf("decompress %s: %v", gzPath, err)
	}
	return tarPath
}
