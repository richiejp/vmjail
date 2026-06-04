//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/testdaemons"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// TestClusterNestedSandboxE2E is ladder step 3 of the D6c.4 slice-3 dogfood
// (see PLAN.md): run a REAL `internal/sandbox` end-to-end VM-boot test
// nested, inside a daft sandbox container. Step 2 (TestClusterNestedVMBoot)
// proved the bundled `daft-sandboxd hello` boots an inner VM; this proves
// the project's own integration test (`TestHelloIntegrationBootsVM`) passes
// against a nested /dev/kvm — i.e. the dogfood path that `make integration`
// would exercise, on a single test.
//
// Delivery model (the cheap one, decoupled from slice 2): the test binary is
// pre-compiled on the host (static, CGO=0) and the raw VM artefacts (CH,
// kernel, initramfs, virtiofsd) are bind-mounted in. NO Go toolchain, source
// tree, or eBPF/kernel build closure is needed inside the container — the
// binary + artefacts are all the integration test consumes. This is the
// daft-check-image "deliver, don't rebuild" philosophy applied to the e2e
// path.
func TestClusterNestedSandboxE2E(t *testing.T) {
	toolingImg := requireEnvPath(t, "DAFT_NIX_TOOLING_IMAGE",
		"nix build .#nix-tooling-image -o bin/nix-tooling-image")
	// The raw VM artefacts the internal/sandbox test resolves from env.
	ch := requireEnvPath(t, "DAFT_CLOUD_HYPERVISOR", "make integration-build")
	kernel := requireEnvPath(t, "DAFT_KERNEL", "make integration-build")
	initramfs := requireEnvPath(t, "DAFT_INITRAMFS", "make integration-build")
	virtiofsd := requireEnvPath(t, "DAFT_VIRTIOFSD", "make integration-build")

	ref := pushDockerTarballToRegistry(t, toolingImg, "test/nested-sandbox-e2e:v1")

	// Stage everything the nested run needs into one host-backed mount dir:
	// a static, pre-built integration-test binary + the raw artefacts.
	workSrc := t.TempDir()
	buildSandboxTestBinary(t, filepath.Join(workSrc, "sandbox.test"))
	stageFile(t, ch, filepath.Join(workSrc, "cloud-hypervisor"), 0o755)
	stageFile(t, kernel, filepath.Join(workSrc, "bzImage"), 0o644)
	stageFile(t, initramfs, filepath.Join(workSrc, "initramfs.cpio.gz"), 0o644)
	stageFile(t, virtiofsd, filepath.Join(workSrc, "virtiofsd"), 0o755)

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	wsName := "nested-sandbox-e2e"
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "NSE2E", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf(
			"%s workspace create --name %s %s", prefix, wsName, orch,
		)},
		{Label: "image", Cmd: fmt.Sprintf(
			"%s workspace image %s --name %s %s", prefix, ref, wsName, orch,
		)},
		{Label: "mount", Cmd: fmt.Sprintf(
			"%s workspace mount add /work --source %s --name %s %s",
			prefix, workSrc, wsName, orch,
		)},
		{Label: "nested", Cmd: fmt.Sprintf(
			"%s workspace nested --enable --name %s %s", prefix, wsName, orch,
		)},
		{Label: "resources", Cmd: fmt.Sprintf(
			"%s workspace resources --memory 4G --cpus 2 --name %s %s",
			prefix, wsName, orch,
		)},
		{Label: "target", Cmd: fmt.Sprintf(
			"%s workspace target %s --name %s %s", prefix, c.SandboxdFP, wsName, orch,
		)},
		{Label: "launch", Cmd: fmt.Sprintf(
			"%s workspace launch --name %s %s", prefix, wsName, orch,
		)},
	}, 30*time.Second)

	c.WaitForSandboxdOutput(t, "daft-nix-tooling ready", 60*time.Second)

	// Run the staged integration-test binary inside the container, off-pane.
	//   * a stub go.mod two dirs above cwd satisfies the test's
	//     repoRootFromTest() check (artefacts come from env, so the repo root
	//     is otherwise unused).
	//   * DAFT_* point at the bind-mounted artefacts; TMPDIR=/work/.tmp keeps
	//     the per-VM hybrid-vsock UDS path short (sockaddr_un < 108 B).
	//   * DAFT_DISABLE_USERNS=1: the sandbox boot path doesn't re-exec, but
	//     set it for parity with the bundled case / future container nesting.
	innerScript := "set -e; " +
		"mkdir -p /work/.tmp /work/r/internal/sandbox; " +
		"printf 'module daftstub\\ngo 1.21\\n' > /work/r/go.mod; " +
		"cd /work/r/internal/sandbox; " +
		"exec env DAFT_CLOUD_HYPERVISOR=/work/cloud-hypervisor " +
		"DAFT_KERNEL=/work/bzImage DAFT_INITRAMFS=/work/initramfs.cpio.gz " +
		"DAFT_VIRTIOFSD=/work/virtiofsd DAFT_DISABLE_USERNS=1 " +
		"HOME=/work TMPDIR=/work/.tmp " +
		"/work/sandbox.test -test.run '^TestHelloIntegrationBootsVM$' -test.v"

	e2eLog := filepath.Join(t.TempDir(), "e2e.log")
	paneDump := func() string { return s.Capture() }
	e2eProc := startDaftSubprocess(t, e2eLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName, "--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/sh", "-c", innerScript)
	// The integration test prints the standard go-test PASS line once the
	// nested VM booted and its banner matched.
	waitForFileContains(t, e2eProc, e2eLog,
		"--- PASS: TestHelloIntegrationBootsVM", 150*time.Second, paneDump)
}

// buildSandboxTestBinary compiles the internal/sandbox integration test
// suite into a static, self-contained binary at dst. CGO is disabled so the
// binary runs in any container image regardless of its libc.
func buildSandboxTestBinary(t *testing.T, dst string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(wd, "..", ".."))
	cmd := exec.Command("go", "test", "-c", "-tags=integration",
		"-o", dst, "./internal/sandbox/")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sandbox integration test binary: %v\n%s", err, out)
	}
}
