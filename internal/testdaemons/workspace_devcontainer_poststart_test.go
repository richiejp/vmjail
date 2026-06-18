//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/localai-org/darkaifactory/internal/testdaemons"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// TestClusterWorkspaceDevcontainerPostStart is the 3f proof for the
// agent-side lifecycle path: a build-based devcontainer that declares a
// postStartCommand has that command run INSIDE the container after it
// starts, on a real VM.
//
// The chain it exercises end to end:
//  1. the node resolves the devcontainer.json at dispatch and threads the
//     postStartCommand into the boot config / GuestConfig;
//  2. the workload boots and its entrypoint keeps the container alive;
//  3. the in-VM agent waits for the container to reach crun "running",
//     then crun-execs the postStartCommand and streams its output to the
//     console under the "post-start: " prefix;
//  4. that output travels container/agent → vsock → sandboxd stdout, where
//     this test waits on the sentinel.
//
// A regression anywhere in that chain (proto field dropped, agent never
// fires, readiness probe wrong, exec env missing) drops the line.
//
// The postStartCommand is given in ARRAY form so it's crun-exec'd directly:
// the nix base ships /bin/dash, not /bin/sh, so a shell-form hook (which
// normalises to ["/bin/sh","-c",…]) would have nothing to exec.
func TestClusterWorkspaceDevcontainerPostStart(t *testing.T) {
	buildImgPath := requireEnvPath(t, "DAFT_BUILD_IMAGE",
		"nix build .#daft-build-image -o bin/daft-build-image")
	baseImgPath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")

	const (
		bootSentinel      = "D101-POSTSTART-CONTAINER-READY"
		postStartSentinel = "D101-POSTSTART-OK"
	)

	buildTar := decompressDockerTarball(t, buildImgPath)
	builder, err := tarball.ImageFromPath(buildTar, nil)
	if err != nil {
		t.Fatalf("load build image %s: %v", buildTar, err)
	}
	buildRef := pushImageToRegistry(t, builder, "test/d101-poststart-builder:v1")

	hostIP := hostNonLoopbackIPv4(t)
	baseRef := pushImageToHostIPRegistry(t, baseImgPath, hostIP, "test/d101-poststart-base:v1")

	content := t.TempDir()
	dcDir := filepath.Join(content, ".devcontainer")
	if err := os.MkdirAll(dcDir, 0o750); err != nil {
		t.Fatalf("mkdir .devcontainer: %v", err)
	}
	// postStartCommand in array form runs directly (no /bin/sh). The
	// entrypoint stays alive (sleep infinity) so the container reaches
	// crun "running" and the agent's post-start step fires.
	writeFile(t, filepath.Join(dcDir, "devcontainer.json"), fmt.Sprintf(
		`{"name":"d101-poststart","build":{"dockerfile":"Dockerfile"},`+
			`"postStartCommand":["/bin/dash","-c","echo %s"]}`,
		postStartSentinel,
	))
	writeFile(t, filepath.Join(dcDir, "Dockerfile"), fmt.Sprintf(
		"FROM %s\n"+
			"ENTRYPOINT [\"/bin/dash\", \"-c\", \"echo %s; exec sleep infinity\"]\n",
		baseRef, bootSentinel,
	))

	c := testdaemons.NewCluster(t,
		testdaemons.WithEgressProxy(),
		testdaemons.WithBuildImage(buildRef))
	s := testtmux.New(t)

	wsName := "d101-poststart"
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D101PS", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf(
			"%s workspace create --name %s %s", prefix, wsName, orch,
		)},
		{Label: "mount", Cmd: fmt.Sprintf(
			"%s workspace mount add /workspace --source %s --name %s %s",
			prefix, content, wsName, orch,
		)},
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

	// Boots the builder VM, pulls the base through the proxy, builds, then
	// boots the workload; the agent runs postStart once it's up. The
	// "post-start:"-prefixed sentinel proves the command ran in-container.
	got := c.WaitForSandboxdOutput(t, postStartSentinel, 300*time.Second)
	if !containsPostStartPrefix(got, postStartSentinel) {
		t.Errorf("postStart sentinel should be emitted under the post-start prefix")
	}
}

// containsPostStartPrefix reports whether the sentinel appears on a line
// tagged with the agent's post-start prefix (proving it came from the
// lifecycle runner, not the entrypoint or build).
func containsPostStartPrefix(out, sentinel string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "post-start: ") && strings.Contains(line, sentinel) {
			return true
		}
	}
	return false
}
