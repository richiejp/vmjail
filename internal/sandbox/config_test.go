package sandbox

import (
	"os"
	"testing"

	"github.com/richiejp/vmjail/internal/bundled"
)

func TestApplyArtifactDefaults(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "artifact-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })

	paths := bundled.Paths{
		CloudHypervisor: "/packaged/vmm",
		Kernel:          "/packaged/kernel",
		Initramfs:       "/packaged/initramfs",
		Virtiofsd:       "/packaged/virtiofsd",
		Files:           [4]*os.File{file},
	}
	var cfg Config
	cfg.ApplyArtifactDefaults(paths)
	if cfg.ArtifactFiles != paths.Files ||
		cfg.CloudHypervisor != paths.CloudHypervisor || cfg.Kernel != paths.Kernel ||
		cfg.Initramfs != paths.Initramfs || cfg.Virtiofsd != paths.Virtiofsd {
		t.Fatalf("bundled defaults = %+v", cfg)
	}

	cfg = Config{
		CloudHypervisor: "/external/vmm",
		Kernel:          "/external/kernel",
		Initramfs:       "/external/initramfs",
		Virtiofsd:       "/external/virtiofsd",
	}
	want := cfg
	cfg.ApplyArtifactDefaults(paths)
	if cfg.ArtifactFiles != paths.Files ||
		cfg.CloudHypervisor != want.CloudHypervisor || cfg.Kernel != want.Kernel ||
		cfg.Initramfs != want.Initramfs || cfg.Virtiofsd != want.Virtiofsd {
		t.Fatalf("configured overrides = %+v", cfg)
	}
}
