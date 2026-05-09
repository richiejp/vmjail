package sandbox

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/agent"
)

func TestHelloRejectsEmptyConfigFields(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantSub string
	}{
		{
			name:    "no cloud-hypervisor",
			cfg:     Config{Kernel: "k", Initramfs: "i"},
			wantSub: "CloudHypervisor",
		},
		{
			name:    "no kernel",
			cfg:     Config{CloudHypervisor: "c", Initramfs: "i"},
			wantSub: "Kernel",
		},
		{
			name:    "no initramfs",
			cfg:     Config{CloudHypervisor: "c", Kernel: "k"},
			wantSub: "Initramfs",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Hello(t.Context(), &bytes.Buffer{}, tt.cfg)
			if err == nil {
				t.Fatalf("Hello with empty %s returned nil, want error", tt.wantSub)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("error %q should mention %q", err.Error(), tt.wantSub)
			}
		})
	}
}

func TestConfigFromEnvReportsMissingVars(t *testing.T) {
	clearVars(t)

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("ConfigFromEnv with no env returned nil, want error")
	}
	for _, want := range []string{EnvCloudHypervisor, EnvKernel, EnvInitramfs} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err.Error(), want)
		}
	}
}

func TestConfigFromEnvReturnsConfigWhenAllSet(t *testing.T) {
	clearVars(t)
	t.Setenv(EnvCloudHypervisor, "/tmp/ch")
	t.Setenv(EnvKernel, "/tmp/bzImage")
	t.Setenv(EnvInitramfs, "/tmp/initrd")
	t.Setenv(EnvMemory, "512M")
	t.Setenv(EnvCPUs, "2")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.CloudHypervisor != "/tmp/ch" || cfg.Kernel != "/tmp/bzImage" ||
		cfg.Initramfs != "/tmp/initrd" || cfg.Memory != "512M" || cfg.CPUs != 2 {
		t.Fatalf("Config = %+v, missing or wrong field", cfg)
	}
}

func TestConfigFromEnvRejectsBadCPUs(t *testing.T) {
	clearVars(t)
	t.Setenv(EnvCloudHypervisor, "/tmp/ch")
	t.Setenv(EnvKernel, "/tmp/bzImage")
	t.Setenv(EnvInitramfs, "/tmp/initrd")
	t.Setenv(EnvCPUs, "not-a-number")

	_, err := ConfigFromEnv()
	if err == nil {
		t.Fatal("ConfigFromEnv with invalid DAFT_CPUS returned nil, want error")
	}
	if !strings.Contains(err.Error(), EnvCPUs) {
		t.Errorf("error should mention %s, got: %v", EnvCPUs, err)
	}
}

// TestHelloIntegrationBootsVM is a real end-to-end test: build the
// sandbox-VM stack with `nix build`, set the env vars, run `go test`.
// Skipped if any required artifact env var is missing or KVM is not
// available.
//
// Run locally with:
//
//	DAFT_CLOUD_HYPERVISOR=$(nix build --print-out-paths .#cloud-hypervisor)/bin/cloud-hypervisor \
//	DAFT_KERNEL=$(nix build --print-out-paths .#kernel)/bzImage \
//	DAFT_INITRAMFS=$(nix build --print-out-paths .#initramfs) \
//	  go test -run TestHelloIntegrationBootsVM ./internal/sandbox
func TestHelloIntegrationBootsVM(t *testing.T) {
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Skipf("artifact env vars not set; skipping VM boot test: %v", err)
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skipf("/dev/kvm not available; skipping: %v", err)
	}

	var buf bytes.Buffer
	err = Hello(t.Context(), &buf, cfg)
	if err != nil {
		t.Fatalf("Hello: %v\n--- console ---\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), agent.Banner) {
		t.Fatalf("console output missing banner %q\n--- console ---\n%s", agent.Banner, buf.String())
	}
}

// clearVars wipes the env vars Hello and ConfigFromEnv consult so each
// test starts from a known clean state regardless of caller environment.
func clearVars(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		EnvCloudHypervisor, EnvKernel, EnvInitramfs, EnvMemory, EnvCPUs,
	} {
		t.Setenv(k, "")
	}
}
