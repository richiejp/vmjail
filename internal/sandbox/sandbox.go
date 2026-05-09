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
	"os/exec"
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
	// empty so simple smoke tests don't need to think about it.
	Memory string
	// CPUs is the number of vCPUs to assign. Defaults to 1 when zero.
	CPUs int
	// ExtraArgs are appended verbatim to the cloud-hypervisor command
	// line. Used by tests to flag a self-exec'd helper as a fake VMM
	// without the helper having to understand cloud-hypervisor syntax.
	ExtraArgs []string
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
	return nil
}

// withDefaults returns a copy of c with empty optional fields filled in.
func (c Config) withDefaults() Config {
	if c.Memory == "" {
		c.Memory = "256M"
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
type runner interface {
	// run starts the VMM, returning a reader for its console output and
	// a stop function the caller must invoke (typically via defer) to
	// release process resources. Errors from start are returned;
	// errors from stop are intentionally swallowed — the caller has
	// already consumed what it needed from the output.
	run(ctx context.Context, cfg Config) (io.ReadCloser, func(), error)
}

// cloudHypervisorRunner runs cloud-hypervisor as a child process and
// captures its console (hvc0) on stdout. It implements runner.
type cloudHypervisorRunner struct{}

func (cloudHypervisorRunner) run(ctx context.Context, cfg Config) (io.ReadCloser, func(), error) {
	args := []string{
		"--kernel", cfg.Kernel,
		"--initramfs", cfg.Initramfs,
		"--cmdline", "console=hvc0 reboot=t panic=-1",
		"--console", "tty",
		"--serial", "off",
		"--cpus", fmt.Sprintf("boot=%d", cfg.CPUs),
		"--memory", "size=" + cfg.Memory,
	}
	args = append(args, cfg.ExtraArgs...)

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
func Hello(ctx context.Context, w io.Writer, cfg Config) error {
	return helloWith(ctx, w, cfg, cloudHypervisorRunner{})
}

// helloWith is Hello parameterised on a runner so tests can swap in a
// fake. Production code calls Hello, which threads cloudHypervisorRunner.
func helloWith(ctx context.Context, w io.Writer, cfg Config, r runner) error {
	if err := cfg.validate(); err != nil {
		return err
	}
	cfg = cfg.withDefaults()

	runCtx, cancel := context.WithTimeout(ctx, HelloTimeout)
	defer cancel()

	stdout, stop, err := r.run(runCtx, cfg)
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
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

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
