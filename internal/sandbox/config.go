package sandbox

import (
	"fmt"
	"os"
	"strconv"
)

// Env var names. Stable so external scripting (and our integration
// tests) can rely on them.
const (
	EnvCloudHypervisor = "DAFT_CLOUD_HYPERVISOR"
	EnvKernel          = "DAFT_KERNEL"
	EnvInitramfs       = "DAFT_INITRAMFS"
	EnvMemory          = "DAFT_MEMORY"
	EnvCPUs            = "DAFT_CPUS"
	EnvVirtiofsd       = "DAFT_VIRTIOFSD"
	// EnvBundle is consulted only by the integration test to point at
	// a host-side OCI bundle. The bundle is copied into the share dir
	// and then surfaced inside the VM via daft.bundle=/share/bundle.
	// Not part of the production Config; ConfigFromEnv ignores it.
	EnvBundle = "DAFT_BUNDLE"
)

// ConfigFromEnv builds a Config from environment variables. Returns an
// error listing every missing required variable in one shot — clearer
// than failing on the first one.
//
// Required:  DAFT_CLOUD_HYPERVISOR, DAFT_KERNEL, DAFT_INITRAMFS
// Optional:  DAFT_MEMORY (default "256M"), DAFT_CPUS (default 1)
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		CloudHypervisor: os.Getenv(EnvCloudHypervisor),
		Kernel:          os.Getenv(EnvKernel),
		Initramfs:       os.Getenv(EnvInitramfs),
		Memory:          os.Getenv(EnvMemory),
		Virtiofsd:       os.Getenv(EnvVirtiofsd),
	}
	if v := os.Getenv(EnvCPUs); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("sandbox: invalid %s=%q: %w", EnvCPUs, v, err)
		}
		cfg.CPUs = n
	}

	var missing []string
	if cfg.CloudHypervisor == "" {
		missing = append(missing, EnvCloudHypervisor)
	}
	if cfg.Kernel == "" {
		missing = append(missing, EnvKernel)
	}
	if cfg.Initramfs == "" {
		missing = append(missing, EnvInitramfs)
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("sandbox: missing required env vars: %v", missing)
	}
	return cfg, nil
}
