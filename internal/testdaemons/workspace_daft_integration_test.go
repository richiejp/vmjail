//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/testdaemons"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// daftNestedIntegrationSentinel is printed by the wrapper shell when the
// curated `go test -tags=integration` exits 0 inside the sandbox. Distinct
// from the readiness sentinel (daft-check ready) so a successful run is not
// confused with the container merely having booted.
const daftNestedIntegrationSentinel = "DAFT-NESTED-INTEGRATION-OK"

// daftNestedIntegrationRun is the curated subset run nested for the first
// landing: just the basic VM-boot integration test, which step 3
// (TestClusterNestedSandboxE2E) already proved boots an inner VM nested —
// here it is COMPILED inside the sandbox by the in-image Go toolchain
// against the vendored deps, not delivered as a pre-built binary. That is
// the difference that makes this the real `make integration` dogfood loop.
// Widen this pattern (or drop -run entirely) once the timeout budget below
// is shown to hold for the heavier container/share/opensnoop cases.
const daftNestedIntegrationRun = "^TestHelloIntegrationBootsVM$"

// daftNestedIntegrationInnerTimeout caps the inner pipeline: a cold-GOCACHE
// offline compile of `internal/sandbox` (+tags=integration) plus one nested
// L3 VM boot. The compile is the dominant, mostly-serial cost; the boot is
// seconds. GNU `timeout` SIGKILLs on overrun, the wrapper shell exits, `daft
// workspace exec` propagates non-zero, and waitForFileContains fails-fast on
// proc.exited() rather than waiting out the outer cap. Provisional 12 min;
// tightens once the warm-GOCACHE follow-up (PLAN.md D6c.4 slice 1) lands.
const daftNestedIntegrationInnerTimeout = 720 // seconds, formatted into the wrapper

// daftNestedIntegrationSlowBoot is the outer cap for waitForFileContains:
// just past the inner GNU-timeout so any overrun surfaces as a fast
// subprocess-exit failure (~5 s after the inner kill), not an outer-timeout
// fatal that swallows the cause. Per CONTRIBUTING.md "Slowness is a bug" this
// is a deadline, not a generous ceiling.
const daftNestedIntegrationSlowBoot = 780 * time.Second

// TestClusterWorkspaceDaftIntegration is ladder step 4 of the D6c.4 slice-3
// dogfood (see PLAN.md): the full `make integration` flow, nested. Where
// step 3 ran a PRE-BUILT integration-test binary nested, this drops the daft
// source tree + vendored deps into a workspace and runs `go test
// -tags=integration` INSIDE it — the in-image Go toolchain compiles the
// integration suite offline and the resulting binary boots an inner L3 VM
// against the nested /dev/kvm. That is the real developer `make integration`
// loop dogfooded end-to-end: compile-inside + nested-boot, not a baked binary.
//
// Delivery model (deliver, don't rebuild — same as self-build step 1 and
// nested step 3): one host-backed /workspace mount carries the tracked-HEAD
// source, the vendored Go modules, AND the raw VM artefacts (CH / kernel /
// initramfs / virtiofsd). The build is fully offline (GOFLAGS=-mod=vendor
// GOPROXY=off); NO nix, no go-module fetch, no kernel/eBPF build closure is
// needed inside — the artefacts are mounted, not rebuilt.
//
// Gated behind DAFT_RUN_NESTED_INTEGRATION=1: a cold-GOCACHE integration
// compile plus a nested VM boot overruns the default per-test budget. Same
// opt-in shape as TestClusterWorkspaceDaftSelfBuild; the baked warm-GOCACHE
// follow-up is what lets it tighten and eventually join the default suite.
func TestClusterWorkspaceDaftIntegration(t *testing.T) {
	if os.Getenv("DAFT_RUN_NESTED_INTEGRATION") != "1" {
		t.Skip("set DAFT_RUN_NESTED_INTEGRATION=1 to run the nested `make integration` dogfood (offline go test -tags=integration inside the sandbox, booting an inner VM against nested /dev/kvm — see PLAN.md D6c.4 slice 3 ladder step 4)")
	}

	// The nix-less check image already ships the Go toolchain + a
	// long-running entrypoint and "daft-check ready" sentinel, so it loads
	// and pushes directly — no withLongRunningEntrypoint rewrap.
	imgPath := requireEnvPath(t, "DAFT_CHECK_IMAGE",
		"nix build .#daft-check-image -o bin/daft-check-image")
	// The raw VM artefacts the inner integration test's resolveArtifacts
	// reads from DAFT_* env.
	ch := requireEnvPath(t, "DAFT_CLOUD_HYPERVISOR", "make integration-build")
	kernel := requireEnvPath(t, "DAFT_KERNEL", "make integration-build")
	initramfs := requireEnvPath(t, "DAFT_INITRAMFS", "make integration-build")
	virtiofsd := requireEnvPath(t, "DAFT_VIRTIOFSD", "make integration-build")

	// Stage everything the nested run needs under one host-backed mount dir:
	// the tracked source, the vendored deps, and the raw artefacts.
	srcDir := t.TempDir()
	exportDaftSourceTo(t, srcDir)
	exportVendorTo(t, srcDir)
	artDir := filepath.Join(srcDir, ".artefacts")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatalf("mkdir artefacts: %v", err)
	}
	stageFile(t, ch, filepath.Join(artDir, "cloud-hypervisor"), 0o755)
	stageFile(t, kernel, filepath.Join(artDir, "bzImage"), 0o644)
	stageFile(t, initramfs, filepath.Join(artDir, "initramfs.cpio.gz"), 0o644)
	stageFile(t, virtiofsd, filepath.Join(artDir, "virtiofsd"), 0o755)

	ref := pushDockerTarballToRegistry(t, imgPath, "test/d6c4-daft-nested-integration:v1")

	// No egress proxy: the build is fully offline, so a cluster without
	// egress is the stronger assertion — if anything reaches for
	// cache.nixos.org or proxy.golang.org it fails rather than silently
	// falling back to the network.
	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	buildLog := filepath.Join(t.TempDir(), "nested-integration.log")
	wsName := "d6c4-nested-integration"

	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "D6C4N", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf(
			"%s workspace create --name %s %s", prefix, wsName, orch,
		)},
		{Label: "image", Cmd: fmt.Sprintf(
			"%s workspace image %s --name %s %s", prefix, ref, wsName, orch,
		)},
		{Label: "mount", Cmd: fmt.Sprintf(
			"%s workspace mount add /workspace --source %s --name %s %s",
			prefix, srcDir, wsName, orch,
		)},
		// nested --enable grants /dev/kvm (10:232) so the inner integration
		// binary can boot an L3 VM (the NestedVirtualization config section).
		{Label: "nested", Cmd: fmt.Sprintf(
			"%s workspace nested --enable --name %s %s", prefix, wsName, orch,
		)},
		// The outer envelope stacks an offline integration compile (every
		// internal/sandbox dep, cold GOCACHE) under a nested inner VM. 12 GiB
		// / 6 vCPU per PLAN.md D6c.4 sizing for a nested dogfood workspace.
		{Label: "resources", Cmd: fmt.Sprintf(
			"%s workspace resources --memory 12G --cpus 6 --name %s %s",
			prefix, wsName, orch,
		)},
		{Label: "target", Cmd: fmt.Sprintf(
			"%s workspace target %s --name %s %s", prefix, c.SandboxdFP, wsName, orch,
		)},
		{Label: "launch", Cmd: fmt.Sprintf(
			"%s workspace launch --name %s %s", prefix, wsName, orch,
		)},
	}, 30*time.Second)

	c.WaitForSandboxdOutput(t, "daft-check ready", 60*time.Second)

	// Inner wrapper — mirrors the self-build env (CGO_ENABLED=0 for the
	// C-compiler-less image, GOTOOLCHAIN=local to pin the baked Go,
	// GOFLAGS=-mod=vendor + GOPROXY=off for the offline build from
	// /workspace/vendor, GOCACHE/GOTMPDIR on host-backed /workspace because
	// each exceeds the 256 MiB /tmp tmpfs) and adds:
	//   * DAFT_* pointing at the bind-mounted raw artefacts, so the inner
	//     integration test's resolveArtifacts uses them (its repoRootFromTest
	//     resolves /workspace naturally — the real go.mod is right there).
	//   * GOTMPDIR=/workspace/.gotmp doubles as the test binary's TMPDIR (go
	//     test sets it), keeping the per-VM hybrid-vsock UDS path short
	//     (os.MkdirTemp("") and t.TempDir() both land there; sockaddr_un < 108).
	//   * DAFT_DISABLE_USERNS=1: the sandbox boot path doesn't re-exec, but
	//     set for parity with the bundled/nested cases.
	//   * `timeout --signal=KILL Ns` bounds the inner pipeline; the trailing
	//     `&& echo <sentinel>` lets waitForFileContains detect success without
	//     relying on the absence of go-test output (which is voluminous even
	//     on a pass).
	execShell := fmt.Sprintf(
		"exec env HOME=/workspace/.home USER=root PATH=/bin "+
			"CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-mod=vendor GOPROXY=off "+
			"GOCACHE=/workspace/.gocache GOPATH=/workspace/.gopath GOTMPDIR=/workspace/.gotmp "+
			"DAFT_CLOUD_HYPERVISOR=/workspace/.artefacts/cloud-hypervisor "+
			"DAFT_KERNEL=/workspace/.artefacts/bzImage "+
			"DAFT_INITRAMFS=/workspace/.artefacts/initramfs.cpio.gz "+
			"DAFT_VIRTIOFSD=/workspace/.artefacts/virtiofsd "+
			"DAFT_DISABLE_USERNS=1 "+
			"/bin/sh -c 'mkdir -p /workspace/.home /workspace/.gotmp && cd /workspace && "+
			"timeout --signal=KILL %ds go test -tags=integration -count=1 -run %s -v ./internal/sandbox/ "+
			"&& echo %s'",
		daftNestedIntegrationInnerTimeout, daftNestedIntegrationRun, daftNestedIntegrationSentinel,
	)

	// Both debugging aids attach before the build exec so they cover the run
	// from the first sample: the /proc profiler answers "is the VM
	// OOM/CPU-bound/blocked"; the Tier-2 gadget stream (sandbox observe)
	// answers "what is the build/boot actually doing". Invisible on a pass,
	// right there in the log on a stall (per [[feedback-observability]]).
	attachWorkspaceProfiler(t, c, wsName)
	attachWorkspaceObserve(t, c, wsName)
	buildProc := startDaftSubprocess(t, buildLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName,
		"--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/sh", "-c", execShell)
	waitForFileContains(t, buildProc, buildLog,
		daftNestedIntegrationSentinel, daftNestedIntegrationSlowBoot,
		func() string { return s.Capture() })
}
