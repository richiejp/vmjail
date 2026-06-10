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

// TestClusterWorkspaceDevcontainerBuild is the #101 slice-3c end-to-end
// proof: a workspace whose ONLY image source is a build-based
// devcontainer.json (a `build` stanza, no explicit image) is built in an
// ephemeral sandbox at dispatch, and the workload then boots from the
// built image.
//
// The whole 3c chain is load-bearing on one waited-for line:
//  1. the node reads .devcontainer/devcontainer.json, sees the build stanza,
//     and resolves the Dockerfile + context from the materialised content;
//  2. it boots the builder VM (DAFT_BUILD_IMAGE_REF) with a generated
//     buildah script, which pulls the base `FROM` a non-loopback registry
//     THROUGH the default-deny egress proxy and runs a RUN step that bakes
//     the sentinel into /built-proof.txt;
//  3. the host reads the docker-archive the build wrote and pushes it into
//     the orchestrator registry over the cluster's hosted-registry mTLS;
//  4. the workload boots from that built ref and its entrypoint prints the
//     sentinel — which the RUN baked and nothing else sets.
//
// A regression anywhere in that chain drops the line.
func TestClusterWorkspaceDevcontainerBuild(t *testing.T) {
	buildImgPath := requireEnvPath(t, "DAFT_BUILD_IMAGE",
		"nix build .#daft-build-image -o bin/daft-build-image")
	baseImgPath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")

	const sentinel = "D101-BUILD-E2E-OK"

	// The builder VM image: the node pulls it HOST-side, so a loopback test
	// registry suffices. Pushed raw — the build script is injected as the
	// container command override at boot, never baked into this image.
	buildTar := decompressDockerTarball(t, buildImgPath)
	builder, err := tarball.ImageFromPath(buildTar, nil)
	if err != nil {
		t.Fatalf("load build image %s: %v", buildTar, err)
	}
	buildRef := pushImageToRegistry(t, builder, "test/d101-build-image:v1")

	// The base the devcontainer's Dockerfile builds FROM: pushed to a
	// NON-loopback host IP so the build VM's `FROM` pull routes through the
	// egress proxy (Go's ProxyFromEnvironment bypasses loopback).
	hostIP := hostNonLoopbackIPv4(t)
	baseRef := pushImageToHostIPRegistry(t, baseImgPath, hostIP, "test/d101-build-base:v1")

	// Workspace content: a build-based devcontainer.json + its Dockerfile,
	// which bakes the sentinel via RUN and prints it via the entrypoint.
	content := t.TempDir()
	dcDir := filepath.Join(content, ".devcontainer")
	if err := os.MkdirAll(dcDir, 0o750); err != nil {
		t.Fatalf("mkdir .devcontainer: %v", err)
	}
	writeFile(t, filepath.Join(dcDir, "devcontainer.json"),
		`{"name":"d101-build","build":{"dockerfile":"Dockerfile"}}`)
	// Exec-form RUN/ENTRYPOINT: the nix base ships /bin/dash, not /bin/sh.
	writeFile(t, filepath.Join(dcDir, "Dockerfile"), fmt.Sprintf(
		"FROM %s\n"+
			"RUN [\"/bin/dash\", \"-c\", \"echo %s > /built-proof.txt\"]\n"+
			"ENTRYPOINT [\"/bin/dash\", \"-c\", \"cat /built-proof.txt; exec sleep infinity\"]\n",
		baseRef, sentinel,
	))

	c := testdaemons.NewCluster(t,
		testdaemons.WithEgressProxy(),
		testdaemons.WithBuildImage(buildRef))
	s := testtmux.New(t)

	wsName := "d101-build"
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D101B", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf(
			"%s workspace create --name %s %s", prefix, wsName, orch,
		)},
		// Host-bind the content at /workspace; the devcontainer + Dockerfile
		// live under it, so the node finds and builds them there.
		{Label: "mount", Cmd: fmt.Sprintf(
			"%s workspace mount add /workspace --source %s --name %s %s",
			prefix, content, wsName, orch,
		)},
		// The only image source: a build-based devcontainer path.
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

	// Generous: this dispatch boots TWO VMs (build then workload) and pulls a
	// multi-layer base through the proxy in between.
	c.WaitForSandboxdOutput(t, sentinel, 300*time.Second)
}
