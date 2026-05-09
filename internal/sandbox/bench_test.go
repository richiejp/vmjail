package sandbox

import (
	"bytes"
	"io"
	"os"
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
		_, _ = ConfigFromEnv()
	}
}

// BenchmarkHelloBoot times one full cold-boot of the sandbox VM through
// the same Hello() the host CLI calls: spawn cloud-hypervisor, decompress
// the kernel, unpack the initramfs, run u-root's init, exec daft-agent,
// scan the banner off the console, kill the VM. The point of this
// benchmark is to catch order-of-magnitude regressions in boot time;
// bench-check fails the build if it doubles, warns if it grows >10%.
//
// Skipped unless the artifact env vars (DAFT_CLOUD_HYPERVISOR / KERNEL /
// INITRAMFS) and /dev/kvm are both available — same gate as the
// integration test.
func BenchmarkHelloBoot(b *testing.B) {
	cfg, err := ConfigFromEnv()
	if err != nil {
		b.Skipf("artifact env vars not set; skipping VM boot benchmark: %v", err)
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		b.Skipf("/dev/kvm not available; skipping: %v", err)
	}
	for b.Loop() {
		var buf bytes.Buffer
		if err := Hello(b.Context(), &buf, cfg); err != nil {
			b.Fatalf("Hello: %v\n--- console ---\n%s", err, buf.String())
		}
	}
}

// BenchmarkHelloBootWithShare is the share-enabled twin of
// BenchmarkHelloBoot. It captures the additional cost of starting
// virtiofsd alongside CH and the agent mounting+reading the share —
// the per-VM tax of file sharing. Same skip gate as the no-share
// benchmark, plus DAFT_VIRTIOFSD must be set.
func BenchmarkHelloBootWithShare(b *testing.B) {
	cfg, err := ConfigFromEnv()
	if err != nil {
		b.Skipf("artifact env vars not set; skipping share VM boot benchmark: %v", err)
	}
	if cfg.Virtiofsd == "" {
		b.Skipf("%s not set; skipping share VM boot benchmark", EnvVirtiofsd)
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		b.Skipf("/dev/kvm not available; skipping: %v", err)
	}

	dir := b.TempDir()
	if err := os.WriteFile(dir+"/"+agent.ShareMarkerFile, []byte("bench"), 0o644); err != nil {
		b.Fatalf("write marker: %v", err)
	}
	cfg.Share = &ShareConfig{
		HostDir:    dir,
		Tag:        "daftshare",
		Mountpoint: testMountpoint,
	}

	for b.Loop() {
		var buf bytes.Buffer
		if err := Hello(b.Context(), &buf, cfg); err != nil {
			b.Fatalf("Hello: %v\n--- console ---\n%s", err, buf.String())
		}
	}
}
