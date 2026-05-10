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
	// Shares is non-empty the value is silently upgraded to include
	// shared=on, which virtio-fs requires for guest mappings.
	Memory string
	// CPUs is the number of vCPUs to assign. Defaults to 1 when zero.
	CPUs int
	// Virtiofsd is the path to the virtiofsd binary. Required when
	// Shares is non-empty, ignored otherwise.
	Virtiofsd string
	// Shares lists host directories to expose to the guest via
	// virtio-fs. One virtiofsd is spawned per entry; the agent
	// mounts each one at its Mountpoint inside the VM.
	//
	// Order is meaningful only for the v0 marker-demo: when Bundle
	// is empty, the agent reads hello.txt from Shares[0].Mountpoint
	// and reports the contents on the console. With Bundle set,
	// every share is mount-only.
	Shares []*ShareConfig
	// Bundle is the *in-guest* absolute path of an OCI bundle the
	// agent should crun-run. Usually a sub-path under one of the
	// Shares' Mountpoint so the host can ship the bundle into the
	// VM via virtiofs. Empty (the default) means no container is
	// run.
	Bundle string
}

// ShareConfig describes one host→guest virtiofs mount.
type ShareConfig struct {
	// HostDir is the absolute path on the sandbox-host that virtiofsd
	// will export. Must exist and be a directory.
	HostDir string
	// Tag is the virtio-fs device tag the guest mounts by name.
	// Must be unique across the Shares slice.
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
	if len(c.Shares) > 0 {
		if c.Virtiofsd == "" {
			return fmt.Errorf("sandbox: Virtiofsd path is required when Shares is non-empty")
		}
		seenTag := make(map[string]bool, len(c.Shares))
		for i, s := range c.Shares {
			if err := s.validate(); err != nil {
				return fmt.Errorf("sandbox: Shares[%d]: %w", i, err)
			}
			if seenTag[s.Tag] {
				return fmt.Errorf("sandbox: duplicate Share.Tag %q", s.Tag)
			}
			seenTag[s.Tag] = true
		}
	}
	if c.Bundle != "" && !strings.HasPrefix(c.Bundle, "/") {
		return fmt.Errorf("sandbox: Bundle %q must be an absolute in-guest path", c.Bundle)
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
// shareSockets carries the Unix-socket paths of every virtiofsd
// sidecar to wire up via `--fs tag=...,socket=...`, parallel to
// cfg.Shares. Empty (or nil) when no share is configured. Threading
// it as an explicit parameter (rather than smuggling it through
// Config) keeps Config a description-of-intent and the runner
// stateless.
type runner interface {
	run(ctx context.Context, cfg Config, shareSockets []string) (io.ReadCloser, func(), error)
}

// shareSidecar abstracts the virtiofsd subprocess. Lifecycle: start
// before the VMM (cloud-hypervisor connects to its socket(s) on
// boot), stop after the VMM exits. Returns one socket path per
// cfg.Shares entry, in the same order. Like runner, swappable in
// tests.
type shareSidecar interface {
	start(ctx context.Context, cfg Config) (socketPaths []string, stop func(), err error)
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

// start spawns one virtiofsd per cfg.Shares entry, returning a slice
// of socket paths (parallel to cfg.Shares). The returned stop tears
// down every spawned process and removes the temp dirs.
func (virtiofsdSidecar) start(ctx context.Context, cfg Config) ([]string, func(), error) {
	sockets := make([]string, 0, len(cfg.Shares))
	stops := make([]func(), 0, len(cfg.Shares))

	stopAll := func() {
		// Tear down in reverse spawn order, mostly cosmetic.
		for i := len(stops) - 1; i >= 0; i-- {
			stops[i]()
		}
	}

	for i, share := range cfg.Shares {
		socket, stop, err := startOneVirtiofsd(ctx, cfg.Virtiofsd, share)
		if err != nil {
			stopAll()
			return nil, nil, fmt.Errorf("sandbox: share %d (%s): %w", i, share.Tag, err)
		}
		sockets = append(sockets, socket)
		stops = append(stops, stop)
	}
	return sockets, stopAll, nil
}

// startOneVirtiofsd spawns a single virtiofsd against share, writes
// its socket into a fresh temp dir, and waits for the socket to
// appear before returning. stop tears down the process + temp dir.
func startOneVirtiofsd(ctx context.Context, binary string, share *ShareConfig) (string, func(), error) {
	dir, err := os.MkdirTemp("", "daft-virtiofs-")
	if err != nil {
		return "", nil, fmt.Errorf("tmpdir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	socket := filepath.Join(dir, "vhost.sock")

	// --sandbox=none keeps virtiofsd from trying to chroot/setuid; we
	// run it as the same user that owns the share dir, so the
	// production hardening (PLAN.md "rootless agent") is layered on
	// at the orchestration boundary, not inside virtiofsd.
	cmd := exec.CommandContext(
		ctx, binary,
		"--socket-path", socket,
		"--shared-dir", share.HostDir,
		"--sandbox=none",
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("spawn virtiofsd: %w", err)
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

// buildKernelCmdline returns the cmdline string CH boots with. Emits
// one `daft.share=<tag>:<mp>` token per share plus `daft.bundle=
// <path>` when Bundle is set, so the in-VM agent picks up the mount
// + container-launch instructions.
func buildKernelCmdline(cfg Config) string {
	parts := []string{kernelCmdlineBase}
	for _, s := range cfg.Shares {
		parts = append(parts, agent.FormatShareCmdline(agent.ShareSpec{
			Tag:        s.Tag,
			Mountpoint: s.Mountpoint,
		}))
	}
	if cfg.Bundle != "" {
		parts = append(parts, agent.FormatBundleCmdline(cfg.Bundle))
	}
	return strings.Join(parts, " ")
}

// buildMemoryArg returns the value for `--memory size=...`. virtio-fs
// requires `shared=on` so the guest's mappings can resolve to host
// pages; we force it whenever any Share is configured so callers
// don't have to know the detail.
func buildMemoryArg(cfg Config) string {
	if len(cfg.Shares) == 0 {
		return "size=" + cfg.Memory
	}
	return "size=" + cfg.Memory + ",shared=on"
}

// shareSocketArg renders a single virtiofsd socket as cloud-
// hypervisor's `--fs tag=...,socket=...` argument value.
func shareSocketArg(tag, socket string) string {
	return fmt.Sprintf("tag=%s,socket=%s", tag, socket)
}

func (cloudHypervisorRunner) run(ctx context.Context, cfg Config, shareSockets []string) (io.ReadCloser, func(), error) {
	args := []string{
		"--kernel", cfg.Kernel,
		"--initramfs", cfg.Initramfs,
		"--cmdline", buildKernelCmdline(cfg),
		"--console", "tty",
		"--serial", "off",
		"--cpus", fmt.Sprintf("boot=%d", cfg.CPUs),
		"--memory", buildMemoryArg(cfg),
	}
	for i, sock := range shareSockets {
		args = append(args, "--fs", shareSocketArg(cfg.Shares[i].Tag, sock))
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

	var shareSockets []string
	if len(cfg.Shares) > 0 {
		socks, stopShare, err := sc.start(runCtx, cfg)
		if err != nil {
			return fmt.Errorf("sandbox: start virtiofsd: %w", err)
		}
		defer stopShare()
		shareSockets = socks
	}

	stdout, stop, err := r.run(runCtx, cfg, shareSockets)
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
