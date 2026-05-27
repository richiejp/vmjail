//go:build linux && integration

package testdaemons_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/testdaemons"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// nestedKVMScript opens /dev/kvm and issues two ioctls via python3:
//
//	KVM_GET_API_VERSION = _IO(0xAE, 0) = 0xAE00  -> must return 12
//	KVM_CREATE_VM       = _IO(0xAE, 1) = 0xAE01  -> returns a vm fd
//
// _IO(type,nr) on Linux encodes as (type<<8)|nr (dir=0, size=0).
// KVM_CREATE_VM succeeding is the gold proof of nested virt: the
// kvm core char device alone would let the open + API-version call
// succeed, but creating a VM forces the vendor backend (kvm_intel /
// kvm_amd) to actually arm VMX/SVM — which only works if Cloud
// Hypervisor exposed the feature CPUID bit to this (L1) guest and
// the guest kernel has CONFIG_KVM_INTEL/AMD built in. A fd >= 0
// means an L2 hypervisor could run here.
const nestedKVMScript = `
import fcntl, os, sys
KVMIO = 0xAE
KVM_GET_API_VERSION = (KVMIO << 8) | 0x00
KVM_CREATE_VM = (KVMIO << 8) | 0x01
try:
    fd = os.open("/dev/kvm", os.O_RDWR | os.O_CLOEXEC)
except OSError as e:
    print("NESTED-KVM-OPEN-FAIL", e)
    sys.exit(1)
ver = fcntl.ioctl(fd, KVM_GET_API_VERSION, 0)
print("KVM-API-VERSION", ver)
try:
    vmfd = fcntl.ioctl(fd, KVM_CREATE_VM, 0)
except OSError as e:
    print("NESTED-KVM-CREATE-FAIL", e)
    sys.exit(1)
print("KVM-CREATE-VM-FD", vmfd)
print("NESTED-KVM-OK" if vmfd >= 0 else "NESTED-KVM-FAIL")
`

// TestClusterNestedKVMProbe is the SPIKE proof that the daft sandbox
// VM is itself a working KVM hypervisor — i.e. nested virtualisation
// is enabled end-to-end (host kvm_intel.nested=Y -> Cloud Hypervisor
// exposes vmx -> guest kernel CONFIG_KVM* -> /dev/kvm bound into the
// container). It launches the nix-tooling image (which ships python3),
// then execs the python KVM probe and asserts the container can create
// a VM through /dev/kvm.
//
// Requires the rebuilt kernel (KVM host config) and the runtimespec
// change that binds /dev/kvm into the container with a 10:232 cgroup
// device-allow. Without either, the open or the KVM_CREATE_VM ioctl
// fails and this test surfaces exactly which layer is missing.
func TestClusterNestedKVMProbe(t *testing.T) {
	imgPath := requireEnvPath(t, "DAFT_NIX_TOOLING_IMAGE",
		"nix build .#nix-tooling-image -o bin/nix-tooling-image")
	ref := pushDockerTarballToRegistry(t, imgPath, "test/nested-kvm:v1")

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	wsName := "nested-kvm"
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	// A workspace needs at least one mount to be launch-ready; the probe
	// doesn't read it, so an empty host dir bound at /workspace suffices.
	mountSrc := t.TempDir()

	testdaemons.RunWorkspaceSteps(t, s, "NKVM", []testdaemons.WorkspaceStep{
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

	c.WaitForSandboxdOutput(t, "daft-nix-tooling ready", 60*time.Second)

	// Run the probe off-pane: `daft workspace exec --tty=false`'s stdin
	// pump only unblocks on EOF, which the tmux PTY never delivers, so
	// running it in the pane would wedge the shell. /dev/null stdin via
	// startDaftSubprocess sidesteps that. python3 -c takes the script as
	// a single argv element, so there's no shell quoting to get wrong.
	probeLog := filepath.Join(t.TempDir(), "probe.log")
	paneDump := func() string { return s.Capture() }
	probeProc := startDaftSubprocess(t, probeLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName, "--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/python3", "-c", nestedKVMScript)
	waitForFileContains(t, probeProc, probeLog, "NESTED-KVM-OK", 60*time.Second, paneDump)
}
