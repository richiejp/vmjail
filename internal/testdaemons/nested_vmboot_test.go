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

// TestClusterNestedVMBoot is the feasibility gate for the D6c.4 slice-3
// "make integration inside a sandbox" dogfood: it proves a FULL inner VM
// boots nested, not just that KVM_CREATE_VM succeeds (TestClusterNestedKVMProbe
// already covers that ioctl). It boots a daft sandbox VM, mounts the
// self-contained `daft-sandboxd` (the Nix bundle that embeds the VMM, guest
// kernel, initramfs, and virtiofsd), enables nested virt, and runs
// `daft-sandboxd hello` INSIDE the container — which extracts the embedded
// artefacts, boots an L2 VM via cloud-hypervisor + virtiofsd + the vsock
// control channel, and prints the agent banner from the L2 serial console.
//
// A passing run means the inner VMM stack (CH against the bound /dev/kvm,
// virtiofsd over a UDS, the hybrid-vsock control channel) all work inside the
// hardened container with only the NestedVirtualization grant. Per-VM cgroup
// limits are the one layer the inner sandboxd consciously opts out of
// (DAFT_DISABLE_CGROUP_LIMITS=1 below): a nested VM sits at its cgroup-v2 root
// with no delegated subtree to enable controllers on, and the outer VM's
// per-VM cgroup already caps the whole nested stack's blast radius. A failure
// surfaces exactly which other layer the nested run is missing (an rlimit, a
// socket-path limit, …) — the whole point of running it before investing in
// the full integration-image delivery.
//
// Self-contained: no artefact delivery beyond the bundled binary, because
// that binary embeds everything `sandbox.Hello` needs.
func TestClusterNestedVMBoot(t *testing.T) {
	bundledBin := requireEnvPath(t, "DAFT_SANDBOXD_BUNDLED_BIN",
		"make integration-build (or nix build .#daft-sandboxd -o bin/daft-sandboxd-bundled)")
	toolingImg := requireEnvPath(t, "DAFT_NIX_TOOLING_IMAGE",
		"nix build .#nix-tooling-image -o bin/nix-tooling-image")
	ref := pushDockerTarballToRegistry(t, toolingImg, "test/nested-vmboot:v1")

	// Stage the bundled binary in a host dir we bind into the container.
	// cp-by-content (not symlink) so the container sees a real file, not a
	// dangling /nix/store link. The same dir doubles as HOME/cache/tmp for
	// the inner boot, so it must be roomy + writable (virtiofs, host-backed)
	// — the embedded-artefact extraction is tens of MiB, past the 256 MiB
	// container tmpfs.
	workSrc := t.TempDir()
	stageFile(t, bundledBin, filepath.Join(workSrc, "daft-sandboxd"), 0o755)

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	wsName := "nested-vmboot"
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	testdaemons.RunWorkspaceSteps(t, s, "NVMB", []testdaemons.WorkspaceStep{
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
		// Nested virt: binds /dev/kvm so the inner cloud-hypervisor can
		// arm a VM.
		{Label: "nested", Cmd: fmt.Sprintf(
			"%s workspace nested --enable --name %s %s", prefix, wsName, orch,
		)},
		// The outer VM hosts a container + an inner 256 MiB VM + the
		// artefact extraction. 4 GiB / 2 vCPU keeps it clear of the OOM
		// killer without over-provisioning.
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

	// Boot the inner VM off-pane (same /dev/null-stdin rationale as the KVM
	// probe). HOME/XDG_CACHE_HOME/TMPDIR all point at the host-backed /work
	// mount: the extraction cache is roomy, and the vsock UDS lands on a
	// short container path (/work/.tmp/...) so it stays under the 108-byte
	// sockaddr_un limit.
	bootLog := filepath.Join(t.TempDir(), "innerboot.log")
	paneDump := func() string { return s.Capture() }
	bootProc := startDaftSubprocess(t, bootLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName, "--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/sh", "-c",
		"mkdir -p /work/.cache /work/.tmp && "+
			// DAFT_DISABLE_USERNS=1: the inner sandboxd already shares the
			// container's userns, so skip its newuidmap self-wrap (no setuid
			// helper exists inside the hardened image anyway).
			// DAFT_DISABLE_CGROUP_LIMITS=1: a nested VM has no delegated cgroup
			// subtree, so the inner sandboxd can't enable controllers (write
			// +memory +cpu → EINVAL) and would otherwise trip the fail-closed
			// per-VM-limit gate. It opts out consciously — the outer VM's
			// per-VM cgroup already bounds the whole nested stack.
			"exec env HOME=/work XDG_CACHE_HOME=/work/.cache TMPDIR=/work/.tmp "+
			"DAFT_DISABLE_USERNS=1 DAFT_DISABLE_CGROUP_LIMITS=1 "+
			"/work/daft-sandboxd hello")
	// The agent banner from the L2 serial console is the proof the inner VM
	// booted all the way to userspace.
	waitForFileContains(t, bootProc, bootLog, "hello world from daft-agent", 120*time.Second, paneDump)
}

// stageFile copies src to dst with the given mode (dereferencing src), so a
// /nix/store-symlinked artefact becomes a real file inside a bind mount.
func stageFile(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("stageFile read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		t.Fatalf("stageFile write %s: %v", dst, err)
	}
	if err := os.Chmod(dst, mode); err != nil {
		t.Fatalf("stageFile chmod %s: %v", dst, err)
	}
}
