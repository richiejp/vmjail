// Package sandbox boots and supervises sandbox VMs by driving the
// configured VMM (Cloud Hypervisor in v1) and ferrying their console
// output back to the caller. The host-side `daft sandbox` commands in
// internal/cli call into this package.
//
// The public surface is deliberately VMM-agnostic so we can swap the
// backend behind the same interface (Firecracker, QEMU q35) — see PLAN.md
// "MicroVM stack".
package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/richiejp/darkaifactory/internal/agent"
)

// HelloTimeout caps how long we wait for the in-VM agent to print its
// banner. The full boot path on a warm host should take well under a
// second; the generous default leaves headroom for first-run kernel
// decompression and sandbox-host load.
const HelloTimeout = 30 * time.Second

// Config selects the VMM and the artifacts it boots. All paths are
// absolute on the sandbox-host. The struct is exported so tests and
// callers can build one without going through the env.
type Config struct {
	// CloudHypervisor is the path to the cloud-hypervisor binary.
	CloudHypervisor string
	// Kernel is the path to the bzImage to boot.
	Kernel string
	// Initramfs is the path to the cpio archive to load as initrd.
	Initramfs string
	// Memory is the guest RAM size, e.g. "256M". Defaults to 256M when
	// empty so simple smoke tests don't need to think about it. When
	// Share is set the value is silently upgraded to include
	// shared=on, which virtio-fs requires for guest mappings.
	Memory string
	// CPUs is the number of vCPUs to assign. Defaults to 1 when zero.
	CPUs int
	// Virtiofsd is the path to the virtiofsd binary. Required when
	// Share is set, ignored otherwise.
	Virtiofsd string
	// Share, when non-zero, exposes a host directory to the guest via
	// virtio-fs. The agent mounts it at Share.Mountpoint and reads
	// Share.Mountpoint/agent.ShareMarkerFile.
	Share *ShareConfig
}

// ShareConfig describes one host→guest virtiofs mount. v0 supports a
// single share; multi-share is straightforward (slice of these) but
// out of scope until we have a use case.
type ShareConfig struct {
	// HostDir is the absolute path on the sandbox-host that virtiofsd
	// will export. Must exist and be a directory.
	HostDir string
	// Tag is the virtio-fs device tag the guest mounts by name.
	Tag string
	// Mountpoint is the absolute path inside the guest where the agent
	// mounts the share. Must start with `/`.
	Mountpoint string
}

// validate returns an error listing every missing required field.
// Missing fields are user-actionable, so we check before doing anything
// that has side effects (process spawn, file I/O).
func (c *Config) validate() error {
	if c.CloudHypervisor == "" {
		return fmt.Errorf("sandbox: CloudHypervisor path is required")
	}
	if c.Kernel == "" {
		return fmt.Errorf("sandbox: Kernel path is required")
	}
	if c.Initramfs == "" {
		return fmt.Errorf("sandbox: Initramfs path is required")
	}
	if c.Share != nil {
		if c.Virtiofsd == "" {
			return fmt.Errorf("sandbox: Virtiofsd path is required when Share is set")
		}
		if err := c.Share.validate(); err != nil {
			return err
		}
	}
	return nil
}

// validate checks the Share-only invariants. HostDir existence is
// checked here because virtiofsd would otherwise fail with a less
// helpful error after we've started spawning processes.
func (s *ShareConfig) validate() error {
	if s.HostDir == "" {
		return fmt.Errorf("sandbox: Share.HostDir is required")
	}
	if !filepath.IsAbs(s.HostDir) {
		return fmt.Errorf("sandbox: Share.HostDir %q must be absolute", s.HostDir)
	}
	if s.Tag == "" {
		return fmt.Errorf("sandbox: Share.Tag is required")
	}
	if !strings.HasPrefix(s.Mountpoint, "/") {
		return fmt.Errorf("sandbox: Share.Mountpoint %q must be absolute", s.Mountpoint)
	}
	info, err := os.Stat(s.HostDir)
	if err != nil {
		return fmt.Errorf("sandbox: Share.HostDir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("sandbox: Share.HostDir %q is not a directory", s.HostDir)
	}
	return nil
}

// withDefaults returns a copy of c with empty optional fields filled in.
func (c Config) withDefaults() Config {
	if c.Memory == "" {
		c.Memory = defaultMemory
	}
	if c.CPUs == 0 {
		c.CPUs = 1
	}
	return c
}

// ErrBannerNotSeen is returned when the VMM exits or the timeout fires
// before the agent prints its banner on the console.
var ErrBannerNotSeen = errors.New("sandbox: agent banner not seen on console")

// runner abstracts the VMM subprocess so the orchestration code in Hello
// can be unit-tested with a fake. Real callers use cloudHypervisorRunner;
// tests inject a stub that returns a pre-canned stream.
//
// shareSocket is the Unix-socket path of the virtiofsd sidecar to wire
// up via `--fs tag=...,socket=...`, or "" when no share is configured.
// Threading it as an explicit parameter (rather than smuggling it
// through Config) keeps Config a description-of-intent and the runner
// stateless.
type runner interface {
	run(ctx context.Context, cfg Config, shareSocket string) (io.ReadCloser, func(), error)
}

// shareSidecar abstracts the virtiofsd subprocess. Lifecycle: start
// before the VMM (cloud-hypervisor connects to its socket on boot),
// stop after the VMM exits. Like runner, swappable in tests.
type shareSidecar interface {
	start(ctx context.Context, cfg Config) (socketPath string, stop func(), err error)
}

// cloudHypervisorRunner runs cloud-hypervisor as a child process and
// captures its console (hvc0) on stdout. It implements runner.
type cloudHypervisorRunner struct{}

// virtiofsdSidecar runs virtiofsd as a child process listening on a
// per-VM Unix socket inside ctx-scoped temp dir. It implements
// shareSidecar.
type virtiofsdSidecar struct{}

// kernelCmdlineBase is the static prefix of the boot cmdline we pass to
// every sandbox VM. console drives kernel logs and our agent's stdout
// to the host's controlling terminal; reboot=t/panic=-1 ensures the
// guest doesn't hang on a panic. helloWith appends `daft.share=...` on
// top when Share is configured.
const kernelCmdlineBase = "console=hvc0 reboot=t panic=-1"

// defaultMemory is the guest RAM size used when Config.Memory is empty.
const defaultMemory = "256M"

// virtiofsdReadyTimeout caps how long start waits for virtiofsd's
// socket to appear before giving up. Should be near-instant on a warm
// host; the generous default leaves room for cold-start.
const virtiofsdReadyTimeout = 5 * time.Second

// start spawns virtiofsd against cfg.Share, writes its socket into a
// fresh temp dir, and waits for the socket to appear before returning.
// The returned stop function tears down the process and removes the
// temp dir.
func (virtiofsdSidecar) start(ctx context.Context, cfg Config) (string, func(), error) {
	dir, err := os.MkdirTemp("", "daft-virtiofs-")
	if err != nil {
		return "", nil, fmt.Errorf("sandbox: virtiofsd tmpdir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	socket := filepath.Join(dir, "vhost.sock")

	// --sandbox=none keeps virtiofsd from trying to chroot/setuid; we
	// run it as the same user that owns the share dir, so the
	// production hardening (PLAN.md "rootless agent") is layered on
	// at the orchestration boundary, not inside virtiofsd.
	cmd := exec.CommandContext(
		ctx, cfg.Virtiofsd,
		"--socket-path", socket,
		"--shared-dir", cfg.Share.HostDir,
		"--sandbox=none",
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("sandbox: spawn virtiofsd: %w", err)
	}

	stop := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		cleanup()
	}

	if err := waitForSocket(ctx, socket, virtiofsdReadyTimeout); err != nil {
		stop()
		return "", nil, err
	}
	return socket, stop, nil
}

// waitForSocket polls path until it exists as a socket node, ctx is
// cancelled, or timeout elapses. Returns a wrapped error on timeout
// for the operator's logs.
func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sandbox: virtiofsd socket %s did not appear within %s", path, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// buildKernelCmdline returns the cmdline string CH boots with. Adds
// `daft.share=<tag>:<mp>` when the config has a share so the in-VM
// agent picks up the mount instructions.
func buildKernelCmdline(cfg Config) string {
	if cfg.Share == nil {
		return kernelCmdlineBase
	}
	return kernelCmdlineBase + " " + agent.FormatShareCmdline(agent.ShareSpec{
		Tag:        cfg.Share.Tag,
		Mountpoint: cfg.Share.Mountpoint,
	})
}

// buildMemoryArg returns the value for `--memory size=...`. virtio-fs
// requires `shared=on` so the guest's mappings can resolve to host
// pages; we force it whenever Share is configured so callers don't
// have to know the detail.
func buildMemoryArg(cfg Config) string {
	if cfg.Share == nil {
		return "size=" + cfg.Memory
	}
	return "size=" + cfg.Memory + ",shared=on"
}

func (cloudHypervisorRunner) run(ctx context.Context, cfg Config, shareSocket string) (io.ReadCloser, func(), error) {
	args := []string{
		"--kernel", cfg.Kernel,
		"--initramfs", cfg.Initramfs,
		"--cmdline", buildKernelCmdline(cfg),
		"--console", "tty",
		"--serial", "off",
		"--cpus", fmt.Sprintf("boot=%d", cfg.CPUs),
		"--memory", buildMemoryArg(cfg),
	}
	if shareSocket != "" {
		args = append(args, "--fs",
			fmt.Sprintf("tag=%s,socket=%s", cfg.Share.Tag, shareSocket))
	}
	cmd := exec.CommandContext(ctx, cfg.CloudHypervisor, args...)

	// Cloud Hypervisor sends both kernel logs and our agent's banner
	// over the virtio-console (hvc0 → its stdout) when --console tty
	// is set. Hijacking stdout lets us scan for the banner. Stderr is
	// discarded — CH's own logs are noisy and not what Hello tests.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: stdout pipe: %w", err)
	}
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("sandbox: start cloud-hypervisor: %w", err)
	}

	stop := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		// Drain stdout before Wait so the kernel buffer doesn't keep
		// the writer parked. Either Wait or this drain may have already
		// raced past EOF; both are fine.
		_, _ = io.Copy(io.Discard, stdout)
		_ = cmd.Wait()
	}
	return stdout, stop, nil
}

// Hello boots a minimal sandbox VM with the given Config, mirrors its
// console output to w, and returns nil once the agent prints its banner.
// The VM is killed before Hello returns regardless of outcome.
//
// This is the smoke test for the microVM stack — kernel decompresses,
// initramfs unpacks, /init (daft-agent) runs and prints, we see it.
// When Config.Share is set, virtiofsd is started alongside CH so the
// guest can mount the share.
func Hello(ctx context.Context, w io.Writer, cfg Config) error {
	return helloWith(ctx, w, cfg, cloudHypervisorRunner{}, virtiofsdSidecar{})
}

// helloWith is Hello parameterised on its subprocess drivers so tests
// can swap in fakes. Production code calls Hello, which threads the
// real cloudHypervisorRunner / virtiofsdSidecar.
func helloWith(ctx context.Context, w io.Writer, cfg Config, r runner, sc shareSidecar) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	cfg = cfg.withDefaults()

	runCtx, cancel := context.WithTimeout(ctx, HelloTimeout)
	defer cancel()

	var shareSocket string
	if cfg.Share != nil {
		sock, stopShare, err := sc.start(runCtx, cfg)
		if err != nil {
			return fmt.Errorf("sandbox: start virtiofsd: %w", err)
		}
		defer stopShare()
		shareSocket = sock
	}

	stdout, stop, err := r.run(runCtx, cfg, shareSocket)
	if err != nil {
		return err
	}
	defer stop()

	return scanForBanner(w, stdout)
}

// scanForBanner mirrors r line-by-line to w and returns nil the moment
// a line containing agent.Banner appears. Returns ErrBannerNotSeen on
// EOF, or a wrapped scanner error on read failure.
//
// Buffer is sized for kernel boot lines, which can be long.
func scanForBanner(w io.Writer, r io.Reader) error {
	scanner := bufio.NewScanner(r)
	// 64 KiB initial, 256 KiB cap. Real kernel boot lines are <1 KiB;
	// the cap is just a hostile-input ceiling.
	scanner.Buffer(make([]byte, 64*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Text()
		_, _ = fmt.Fprintln(w, line)
		if strings.Contains(line, agent.Banner) {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("sandbox: read console: %w", err)
	}
	return ErrBannerNotSeen
}
