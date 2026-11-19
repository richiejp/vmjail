package sandbox

import (
	"fmt"
	"os"
)

// Env var names. Stable so external scripting (and our integration
// tests) can rely on them.
const (
	EnvCloudHypervisor = "VMJAIL_CLOUD_HYPERVISOR"
	EnvKernel          = "VMJAIL_KERNEL"
	EnvInitramfs       = "VMJAIL_INITRAMFS"
	EnvVirtiofsd       = "VMJAIL_VIRTIOFSD"
	// EnvBundle is consulted only by the integration test to point at
	// a host-side OCI bundle. The bundle is copied into the share dir
	// and then surfaced inside the VM via vmjail.bundle=/share/bundle.
	// Not part of the production Config; ConfigFromEnv ignores it.
	EnvBundle = "VMJAIL_BUNDLE"
)

// ConfigFromEnv builds a Config from environment variables. Returns an
// error listing every missing required variable in one shot — clearer
// than failing on the first one.
//
// requireArtefacts controls whether the artefact paths are mandatory.
// Pass true when the artefacts can only come from the environment (e.g.
// `vmjail sandbox hello`, or a plain `go build` vmjail-sandboxd with no
// embedded assets). Pass false when the binary has the artefacts
// embedded via internal/bundled: the path vars are then read as an
// override if present, but their absence is not an error because the
// bundled assets are the source of truth at dispatch.
//
// Required (requireArtefacts): VMJAIL_CLOUD_HYPERVISOR, VMJAIL_KERNEL, VMJAIL_INITRAMFS
// Optional: VMJAIL_VIRTIOFSD
func ConfigFromEnv(requireArtefacts bool) (Config, error) {
	cfg := Config{
		CloudHypervisor: os.Getenv(EnvCloudHypervisor),
		Kernel:          os.Getenv(EnvKernel),
		Initramfs:       os.Getenv(EnvInitramfs),
		Virtiofsd:       os.Getenv(EnvVirtiofsd),
	}

	if !requireArtefacts {
		return cfg, nil
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
