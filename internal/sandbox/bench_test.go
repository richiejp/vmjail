package sandbox

// Unit benches only. The KVM + Nix-built-artefact benches live in
// integration_test.go behind the `integration` build tag.

import (
	"io"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/agent"
)

// realisticBootLog approximates the volume and shape of kernel boot
// output scanForBanner sees in production: ~8 KiB of mixed [    0.001]-
// prefixed lines ending with the agent banner. Boot logs are bounded —
// if this benchmark balloons we want to know.
var realisticBootLog = func() string {
	var b strings.Builder
	b.Grow(8 * 1024)
	for range 64 {
		b.WriteString("[    0.001234] some kernel module says something useful\n")
	}
	b.WriteString("[    0.099000] Run /init as init process\n")
	b.WriteString(agent.Banner)
	b.WriteByte('\n')
	return b.String()
}()

func BenchmarkScanForBanner(b *testing.B) {
	b.SetBytes(int64(len(realisticBootLog)))
	for b.Loop() {
		_ = scanForBanner(io.Discard, strings.NewReader(realisticBootLog))
	}
}

func BenchmarkConfigFromEnv(b *testing.B) {
	b.Setenv(EnvCloudHypervisor, "/usr/bin/cloud-hypervisor")
	b.Setenv(EnvKernel, "/boot/bzImage")
	b.Setenv(EnvInitramfs, "/boot/initramfs.cpio.gz")
	b.Setenv(EnvMemory, "512M")
	b.Setenv(EnvCPUs, "4")
	for b.Loop() {
		_, _ = ConfigFromEnv(true)
	}
}
