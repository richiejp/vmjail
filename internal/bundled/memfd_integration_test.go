//go:build linux && integration

package bundled

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/richiejp/vmjail/internal/agent"
	"github.com/richiejp/vmjail/internal/sandbox"
)

// Exercise production sealing and child descriptor inheritance using current
// built artifacts, including virtiofsd and a real container inside the VM.
func TestSealedAssetsBootContainer(t *testing.T) {
	read := func(env string) []byte {
		t.Helper()
		path := os.Getenv(env)
		if path == "" {
			t.Fatalf("set %s to a built artifact", env)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	p, err := loadAssets(read(sandbox.EnvCloudHypervisor), read(sandbox.EnvKernel), read(sandbox.EnvInitramfs), read(sandbox.EnvVirtiofsd))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	bundle := os.Getenv(sandbox.EnvBundle)
	if bundle == "" {
		t.Fatal("set VMJAIL_BUNDLE to the hello bundle")
	}
	vsock, cleanup, err := sandbox.NewStandaloneVsock()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	cfg := sandbox.Config{
		CloudHypervisor: p.CloudHypervisor, Kernel: p.Kernel, Initramfs: p.Initramfs, Virtiofsd: p.Virtiofsd, ArtifactFiles: p.Files, Vsock: vsock,
		Shares: []*sandbox.ShareConfig{{HostDir: bundle, Tag: "bundle", Mountpoint: "/bundle"}}, Bundle: "/bundle",
		ObserverLogPath: filepath.Join(t.TempDir(), "observe.jsonl"),
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := sandbox.Hello(ctx, &out, cfg); err != nil {
		t.Fatalf("boot sealed assets: %v\n%s", err, &out)
	}
	if !strings.Contains(out.String(), agent.ContainerBanner) {
		t.Fatalf("missing container banner:\n%s", &out)
	}
}
