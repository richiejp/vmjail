package sandboxdcli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/richiejp/darkaifactory/internal/bundled"
	"github.com/richiejp/darkaifactory/internal/sandbox"
	"github.com/richiejp/darkaifactory/internal/version"
)

func runCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	err = Execute(args, &outBuf, &errBuf)
	return outBuf.String(), errBuf.String(), err
}

func TestVersionPrintsVersion(t *testing.T) {
	stdout, _, err := runCmd(t, "version")
	if err != nil {
		t.Fatalf("daft-sandboxd version: %v", err)
	}
	if got := strings.TrimSpace(stdout); got != version.Version {
		t.Fatalf("version output = %q, want %q", got, version.Version)
	}
}

func TestHelloIsRegistered(t *testing.T) {
	stdout, _, err := runCmd(t, "--help")
	if err != nil {
		t.Fatalf("daft-sandboxd --help: %v", err)
	}
	if !strings.Contains(stdout, "hello") {
		t.Fatalf("help output should advertise hello sub-command, got:\n%s", stdout)
	}
}

// TestHelloRefusesWithoutBundle exercises the dev `go test` path: in
// this build, internal/bundled has stub assets, so daft-sandboxd hello
// should refuse with a user-actionable error pointing at the env-var
// fallback.
func TestHelloRefusesWithoutBundle(t *testing.T) {
	if bundled.IsBundled() {
		t.Skip("test binary has real bundled artifacts; refusal path n/a")
	}
	_, _, err := runCmd(t, "hello")
	if err == nil {
		t.Fatal("daft-sandboxd hello with stub bundle returned nil, want error")
	}
	if !strings.Contains(err.Error(), "no bundled artifacts") {
		t.Errorf("error should mention missing bundle, got: %v", err)
	}
}

func TestUnknownCommandReturnsError(t *testing.T) {
	_, _, err := runCmd(t, "definitely-not-a-real-subcommand")
	if err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
}

func TestRunHelloWithCacheDirError(t *testing.T) {
	want := errors.New("cache dir failure")
	err := runHelloWith(t.Context(), io.Discard, helloDeps{
		cacheDir: func() (string, error) { return "", want },
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestRunHelloWithExtractError(t *testing.T) {
	want := errors.New("filesystem broken")
	err := runHelloWith(t.Context(), io.Discard, helloDeps{
		cacheDir: func() (string, error) { return "/tmp", nil },
		extract:  func(string) (bundled.Paths, error) { return bundled.Paths{}, want },
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestRunHelloWithBootSuccess(t *testing.T) {
	const cacheRoot = "/tmp/cache"
	bootCalled := false
	var bootCfg sandbox.Config
	err := runHelloWith(t.Context(), io.Discard, helloDeps{
		cacheDir: func() (string, error) { return cacheRoot, nil },
		extract: func(dir string) (bundled.Paths, error) {
			if dir != cacheRoot {
				t.Errorf("extract called with %q, want %q", dir, cacheRoot)
			}
			return bundled.Paths{
				CloudHypervisor: "/x/cloud-hypervisor",
				Kernel:          "/x/bzImage",
				Initramfs:       "/x/initramfs.cpio.gz",
			}, nil
		},
		boot: func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
			bootCalled = true
			bootCfg = cfg
			return nil
		},
	})
	if err != nil {
		t.Fatalf("runHelloWith: %v", err)
	}
	if !bootCalled {
		t.Fatal("boot was not invoked")
	}
	if bootCfg.CloudHypervisor != "/x/cloud-hypervisor" ||
		bootCfg.Kernel != "/x/bzImage" ||
		bootCfg.Initramfs != "/x/initramfs.cpio.gz" {
		t.Errorf("boot called with wrong Config: %+v", bootCfg)
	}
}

func TestExtractWrapperReturnsErrNotBundledOnStubs(t *testing.T) {
	// In `go test`, the embedded slices in internal/bundled are
	// stubs, so the public Extract should report ErrNotBundled.
	// (The internal extractTo path is exercised by the bundled
	// package's own tests with injected fake bytes.)
	_, err := bundled.Extract(t.TempDir())
	if !errors.Is(err, bundled.ErrNotBundled) {
		t.Fatalf("Extract on stub binary = %v, want ErrNotBundled", err)
	}
}
