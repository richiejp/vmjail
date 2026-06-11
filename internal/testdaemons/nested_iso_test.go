//go:build linux && integration

package testdaemons_test

import (
	"bufio"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/testdaemons"
	"github.com/localai-org/darkaifactory/internal/testtmux"
)

// TestClusterNestedISOBoot is the slice-1 proof of the iso-nested-qemu
// diversion (branch iso-nested-qemu): booting an UNTRUSTED, opaque ISO
// (Linux + an SSH-reachable agent we cannot rebuild) inside a daft
// sandbox and reaching its sshd from the host — without teaching daft's
// own VMM to boot ISOs, give its guest a NIC, or speak anything but
// vsock.
//
// The trick is to nest, not to extend daft. The outer daft VM is the
// normal hardened stack (daft-agent over vsock, virtiofs, per-VM
// cgroups, the /proc sampler) and stays completely unchanged. The ISO
// rides as an L2 payload that qemu boots the *normal* way: real -cdrom
// boot, real virtio-net, slirp user-mode NAT with a hostfwd that lands
// the ISO's sshd on the container's loopback :2222. qemu gets KVM
// acceleration from the NestedVirtualization grant's bound /dev/kvm —
// the same /dev/kvm TestClusterNestedVMBoot drives with cloud-hypervisor.
//
// Two assertions, one per layer of the chain:
//
//  1. launch-iso (baked into iso-qemu-runner-image) boots the ISO and
//     blocks until the ISO's sshd answers "SSH-2.0" on the slirp
//     hostfwd, then prints DAFT_ISO_SSH_READY. Reaching that sentinel
//     proves qemu + nested KVM + slirp + the guest sshd all came up
//     *inside* the container.
//
//  2. A host-side `daft workspace forward` reaches that same sshd
//     through daft's EXISTING port-forward path — host TCP →
//     ForwardPort RPC → orchestrator → sandboxd → vsock → agent →
//     container :2222 → qemu slirp → ISO sshd. The SSH banner read back
//     on the local port is the property the whole diversion exists for.
//
// Because qemu's hostfwd turns the ISO's sshd into an ordinary TCP
// listener on the container loopback, layer 2 needs no new daft
// networking — it is the same port-forward the nix-server forward test
// exercises. Swapping this fixture for a real LocalAI ISO is a one-line
// --source change on the /iso mount.
func TestClusterNestedISOBoot(t *testing.T) {
	qemuImg := requireEnvPath(t, "DAFT_ISO_QEMU_IMAGE",
		"make integration-build (nix build .#iso-qemu-runner-image -o bin/iso-qemu-runner-image)")
	isoPath := requireEnvPath(t, "DAFT_ISO_SSH_FIXTURE",
		"make integration-build (nix build .#iso-ssh-fixture -o bin/iso-ssh-fixture)")
	ref := pushDockerTarballToRegistry(t, qemuImg, "test/iso-qemu-runner:v1")

	// Mount the ISO read-only off its own /nix/store directory. The Make
	// env resolves DAFT_ISO_SSH_FIXTURE to the real .iso (realpath, no
	// dangling symlink), and virtiofsd serves it from the host side, so
	// the guest sees a real file at /iso/<name> with no store-symlink
	// dance and no multi-hundred-MB copy per run.
	isoDir := filepath.Dir(isoPath)
	isoName := filepath.Base(isoPath)

	c := testdaemons.NewCluster(t)
	s := testtmux.New(t)

	wsName := "nested-iso"
	const containerPort = uint32(2222)
	localPort := freeLocalPort(t)
	prefix := fmt.Sprintf("%s --config-dir %s", c.DaftBin, c.ClientConfigDir)
	orch := "--orchestrator " + c.OrchestratorURL

	// Writable scratch the launcher uses for qemu's serial log (HOME and
	// QEMU_SERIAL_LOG both resolve under /work). Host-backed virtiofs, so
	// roomy + writable unlike the small container tmpfs.
	workSrc := t.TempDir()

	testdaemons.RunWorkspaceSteps(t, s, "NISO", []testdaemons.WorkspaceStep{
		{Label: "create", Cmd: fmt.Sprintf(
			"%s workspace create --name %s %s", prefix, wsName, orch,
		)},
		{Label: "image", Cmd: fmt.Sprintf(
			"%s workspace image %s --name %s %s", prefix, ref, wsName, orch,
		)},
		{Label: "iso", Cmd: fmt.Sprintf(
			"%s workspace mount add /iso --source %s --name %s %s",
			prefix, isoDir, wsName, orch,
		)},
		{Label: "work", Cmd: fmt.Sprintf(
			"%s workspace mount add /work --source %s --name %s %s",
			prefix, workSrc, wsName, orch,
		)},
		// Nested virt: binds /dev/kvm so the inner qemu gets KVM accel.
		{Label: "nested", Cmd: fmt.Sprintf(
			"%s workspace nested --enable --name %s %s", prefix, wsName, orch,
		)},
		// The outer VM holds qemu's 1.5 GiB inner RAM + the ISO page
		// cache + the outer daft stack. 6 GiB / 2 vCPU keeps it clear of
		// the OOM killer without over-provisioning.
		{Label: "resources", Cmd: fmt.Sprintf(
			"%s workspace resources --memory 6G --cpus 2 --name %s %s",
			prefix, wsName, orch,
		)},
		// Declare container :2222 forwardable so the host can reach the
		// ISO's sshd through the orchestrator.
		{Label: "port", Cmd: fmt.Sprintf(
			"%s workspace port add %d --name %s %s",
			prefix, containerPort, wsName, orch,
		)},
		{Label: "target", Cmd: fmt.Sprintf(
			"%s workspace target %s --name %s %s", prefix, c.SandboxdFP, wsName, orch,
		)},
		{Label: "launch", Cmd: fmt.Sprintf(
			"%s workspace launch --name %s %s", prefix, wsName, orch,
		)},
	}, 30*time.Second)

	c.WaitForSandboxdOutput(t, "daft-iso-qemu ready", 60*time.Second)

	// Layer 1: boot the ISO as a nested L2 VM. launch-iso backgrounds
	// qemu, polls the slirp-forwarded sshd until it speaks SSH-2.0, then
	// prints the sentinel and keeps qemu in the foreground so layer 2 has
	// a live target. NixOS boots under nested KVM in well under a minute,
	// but allow generous slack for a cold page cache over virtiofs.
	bootLog := filepath.Join(t.TempDir(), "isoboot.log")
	paneDump := func() string { return s.Capture() }
	bootProc := startDaftSubprocess(t, bootLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "exec", wsName, "--tty=false",
		"--orchestrator", c.OrchestratorURL,
		"--", "/bin/launch-iso", "/iso/"+isoName,
		fmt.Sprintf("%d", containerPort))
	waitForFileContains(t, bootProc, bootLog,
		"DAFT_ISO_SSH_READY", 5*time.Minute, paneDump)

	// Layer 2: reach that sshd from the HOST through daft's port-forward.
	forwardLog := filepath.Join(t.TempDir(), "forward.log")
	forwardProc := startDaftSubprocess(t, forwardLog,
		c.DaftBin, "--config-dir", c.ClientConfigDir,
		"workspace", "forward",
		fmt.Sprintf("%d:%d", localPort, containerPort), wsName,
		"--orchestrator", c.OrchestratorURL)
	waitForFileContains(t, forwardProc, forwardLog,
		"daft workspace forward:", 30*time.Second, paneDump)

	banner := readSSHBanner(t,
		fmt.Sprintf("127.0.0.1:%d", localPort), 30*time.Second)
	if !strings.HasPrefix(banner, "SSH-2.0") {
		t.Fatalf("forwarded-port banner = %q, want SSH-2.0 prefix", banner)
	}
	t.Logf("reached nested ISO sshd through daft forward: %s", banner)
}

// readSSHBanner dials addr and returns the first line the server sends,
// retrying until timeout. An SSH server emits its "SSH-2.0-..."
// identification string immediately on connect, so the banner alone
// proves the byte path terminates at a live sshd — no auth needed.
func readSSHBanner(t *testing.T, addr string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			lastErr = err
			time.Sleep(500 * time.Millisecond)
			continue
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		line, err := bufio.NewReader(conn).ReadString('\n')
		_ = conn.Close()
		if err == nil && strings.TrimSpace(line) != "" {
			return strings.TrimRight(line, "\r\n")
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("no SSH banner from %s within %s: %v", addr, timeout, lastErr)
	return ""
}
