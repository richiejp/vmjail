//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/agent"
	"github.com/localai-org/darkaifactory/internal/testdaemons"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// TestClusterEmbeddedSandboxdBoot proves the production artefact path:
// the Nix-built daft-sandboxd embeds cloud-hypervisor / kernel /
// initramfs / virtiofsd via //go:embed and Extract()s them at runtime.
// Every other integration test boots via a go-built sandboxd that reads
// those artefacts from DAFT_* env vars, so the embed/Extract path itself
// is otherwise never exercised end to end.
//
// WithBundledSandboxd runs the real bundled binary with the artefact
// env vars pointed at bogus paths. serve still calls ConfigFromEnv at
// startup (so the vars must be non-empty), but the bundled binary's
// Extract() returns the embedded artefacts and ignores the env — so a
// VM can boot only if extraction won; if embedding were broken, the env
// fallback would hit the bogus paths and fail. A successful launch +
// container banner is thus a direct proof of the embedded path. (The
// workload still comes from the pushed hello image; only the VM-boot
// artefacts are under test here.)
func TestClusterEmbeddedSandboxdBoot(t *testing.T) {
	bundlePath := requireBundle(t)
	sandboxdBin := requireBundledSandboxd(t)
	ref := pushHelloImage(t, bundlePath)

	c := testdaemons.NewCluster(t, testdaemons.WithBundledSandboxd(sandboxdBin))
	s := testtmux.New(t)

	mountSrc := t.TempDir()
	wsName := "embedded-boot"
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "EMBED", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf(
			"%s workspace create --name %s %s", prefix, wsName, orch,
		)},
		{Label: "image", Cmd: fmt.Sprintf(
			"%s workspace image %s --name %s %s", prefix, ref, wsName, orch,
		)},
		{Label: "mount", Cmd: fmt.Sprintf(
			"%s workspace mount add /workspace --source %s --name %s %s",
			prefix, mountSrc, wsName, orch,
		)},
		{Label: "target", Cmd: fmt.Sprintf(
			"%s workspace target %s --name %s %s", prefix, c.SandboxdFP, wsName, orch,
		)},
		{Label: "launch", Cmd: fmt.Sprintf(
			"%s workspace launch --name %s %s", prefix, wsName, orch,
		)},
	}, 30*time.Second)

	if got := s.Capture(); !strings.Contains(got, "Dispatched") {
		t.Fatalf("post-launch capture missing Dispatched:\n%s", got)
	}

	// The banner only appears if the bundled sandboxd booted a real VM
	// from its embedded artefacts (the DAFT_* paths were bogus) and the
	// agent ran crun against the hello image.
	c.WaitForSandboxdOutput(t, agent.ContainerBanner, 60*time.Second)
}

// requireBundledSandboxd fatals with a clear pointer when
// DAFT_SANDBOXD_BUNDLED_BIN is unset. Only the embedded-boot test needs
// the real Nix-built (//go:embed-bundled) binary, so it's gated
// separately from the go-built-binary path the other tests use.
func requireBundledSandboxd(t *testing.T) string {
	t.Helper()
	p := os.Getenv("DAFT_SANDBOXD_BUNDLED_BIN")
	if p == "" {
		t.Fatalf("testdaemons: DAFT_SANDBOXD_BUNDLED_BIN not set " +
			"(run via `make integration`, or `nix build .#daft-sandboxd " +
			"-o bin/daft-sandboxd-bundled` and point it at " +
			"bin/daft-sandboxd-bundled/bin/daft-sandboxd)")
	}
	return p
}
