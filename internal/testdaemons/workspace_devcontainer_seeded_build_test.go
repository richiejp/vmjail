//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/testdaemons"
	"github.com/richiejp/darkaifactory/internal/testtmux"
)

// TestClusterWorkspaceDevcontainerBuildSeededBuilder is the PLAN 3d proof:
// a build-based devcontainer builds and boots with NO DAFT_BUILD_IMAGE_REF
// set. The bundled (//go:embed) daft-orchestrator auto-seeds its build
// image into the embedded registry at startup under
// ociregistry.BuiltinBuildRef(), and the node defaults to that ref — so the
// whole build path works with zero build-image env wiring.
//
// It is the sibling of TestClusterWorkspaceDevcontainerBuild, which instead
// pushes the builder by hand and sets DAFT_BUILD_IMAGE_REF via
// WithBuildImage. Here that is entirely absent: only WithBundledOrchestrator.
//
// The build VM still pulls the base `FROM` through the default-deny egress
// proxy (pushed to a non-loopback host IP so Go's ProxyFromEnvironment does
// not bypass it), exactly as the sibling test does.
func TestClusterWorkspaceDevcontainerBuildSeededBuilder(t *testing.T) {
	orchBin := requireBundledOrchestrator(t)
	baseImgPath := requireEnvPath(t, "DAFT_NIX_IMAGE",
		"nix build .#nix-shell-image -o bin/nix-shell-image")

	const sentinel = "D101-SEEDED-BUILDER-OK"

	hostIP := hostNonLoopbackIPv4(t)
	baseRef := pushImageToHostIPRegistry(t, baseImgPath, hostIP, "test/d101-seeded-base:v1")

	content := t.TempDir()
	dcDir := filepath.Join(content, ".devcontainer")
	if err := os.MkdirAll(dcDir, 0o750); err != nil {
		t.Fatalf("mkdir .devcontainer: %v", err)
	}
	writeFile(t, filepath.Join(dcDir, "devcontainer.json"),
		`{"name":"d101-seeded","build":{"dockerfile":"Dockerfile"},"overrideCommand":false}`)
	// Exec-form RUN/ENTRYPOINT: the nix base ships /bin/dash, not /bin/sh.
	writeFile(t, filepath.Join(dcDir, "Dockerfile"), fmt.Sprintf(
		"FROM %s\n"+
			"RUN [\"/bin/dash\", \"-c\", \"echo %s > /built-proof.txt\"]\n"+
			"ENTRYPOINT [\"/bin/dash\", \"-c\", \"cat /built-proof.txt; exec sleep infinity\"]\n",
		baseRef, sentinel,
	))

	// No WithBuildImage / DAFT_BUILD_IMAGE_REF: the builder comes from the
	// orchestrator's auto-seeded built-in image.
	c := testdaemons.NewCluster(t,
		testdaemons.WithEgressProxy(),
		testdaemons.WithBundledOrchestrator(orchBin))
	s := testtmux.New(t)

	wsName := "d101-seeded"
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D101S", []testdaemons.WorkspaceStep{
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

	// Generous: boots the seeded builder VM, pulls a multi-layer base
	// through the proxy, then boots the built workload.
	c.WaitForSandboxdOutput(t, sentinel, 300*time.Second)
}

// requireBundledOrchestrator returns the path to the //go:embed-bundled
// daft-orchestrator, or skips/fails when it is not available.
func requireBundledOrchestrator(t *testing.T) string {
	t.Helper()
	p := os.Getenv("DAFT_ORCH_BUNDLED_BIN")
	if p == "" {
		t.Fatalf("testdaemons: DAFT_ORCH_BUNDLED_BIN not set " +
			"(run via `make integration`, or `nix build .#daft-orchestrator " +
			"-o bin/daft-orchestrator-bundled` and point it at " +
			"bin/daft-orchestrator-bundled/bin/daft-orchestrator)")
	}
	return p
}
