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
}

// ErrBannerNotSeen is returned when the VM exits or the timeout fires
// before the agent prints its banner on the console.
var ErrBannerNotSeen = errors.New("sandbox: agent banner not seen on console")

// Hello boots a minimal sandbox VM with the given Config, mirrors its
// console output to w, and returns nil once the agent prints its banner.
// The VM is killed before Hello returns regardless of outcome.
//
// This is the smoke test for the microVM stack — kernel decompresses,
// initramfs unpacks, /init (daft-agent) runs and prints, we see it.
func Hello(ctx context.Context, w io.Writer, cfg Config) error {
	if cfg.CloudHypervisor == "" {
		return fmt.Errorf("sandbox: CloudHypervisor path is required")
	}
	if cfg.Kernel == "" {
		return fmt.Errorf("sandbox: Kernel path is required")
	}
	if cfg.Initramfs == "" {
		return fmt.Errorf("sandbox: Initramfs path is required")
	}
	if cfg.Memory == "" {
		cfg.Memory = "256M"
	}
	if cfg.CPUs == 0 {
		cfg.CPUs = 1
	}

	runCtx, cancel := context.WithTimeout(ctx, HelloTimeout)
	defer cancel()

	cmd := exec.CommandContext(
		runCtx, cfg.CloudHypervisor,
		"--kernel", cfg.Kernel,
		"--initramfs", cfg.Initramfs,
		"--cmdline", "console=hvc0 reboot=t panic=-1",
		"--console", "tty",
		"--serial", "off",
		"--cpus", fmt.Sprintf("boot=%d", cfg.CPUs),
		"--memory", "size="+cfg.Memory,
	)

	// Cloud Hypervisor sends both kernel logs and our agent's banner
	// over the virtio-console (hvc0 → its stdout) when --console tty
	// is set. We hijack stdout so we can scan for the banner. Stderr
	// is discarded; CH's own logs are noisy and not what we're testing.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("sandbox: stdout pipe: %w", err)
	}
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("sandbox: start cloud-hypervisor: %w", err)
	}
	// CommandContext kills on ctx done; this defer is the belt to that
	// suspenders so we don't leak a VM if Wait is bypassed.
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_, _ = io.Copy(io.Discard, stdout)
		_ = cmd.Wait()
	}()

	scanner := bufio.NewScanner(stdout)
	// Kernel boot lines can be long; allow generous buffer to avoid
	// "token too long" mid-boot.
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
