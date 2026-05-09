package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/richiejp/darkaifactory/internal/agent"
)

// TestMain doubles the test binary as a "fake VMM" when invoked with the
// fakeVMMEnv variable set. Parent tests set the env var, exec
// os.Args[0], and the child prints its scripted lines (separated by `|`)
// then sleeps until killed. This lets us cover cloudHypervisorRunner
// (which requires a real binary on disk that accepts cloud-hypervisor's
// flag set) without needing KVM or a real VMM.
//
// The check happens before m.Run, so flag.Parse() never sees the
// cloud-hypervisor flags our parent passes us.
const (
	fakeVMMEnv = "DAFT_TEST_FAKE_VMM_LINES"
	// devNull is /dev/null. Used as a stand-in for the kernel and
	// initramfs paths in tests that swap in a fake VMM — the fake
	// ignores its args, so the paths only need to exist.
	devNull = "/dev/null"
)

func TestMain(m *testing.M) {
	if script := os.Getenv(fakeVMMEnv); script != "" {
		runFakeVMM(script)
		return
	}
	os.Exit(m.Run())
}

// runFakeVMM emulates the cloud-hypervisor binary just enough for
// cloudHypervisorRunner to scan its stdout. Lines are pipe-separated
// because env vars do not preserve newlines portably.
//
// After printing, the fake either exits (when the script ends with
// the literal token "EXIT") or sleeps until killed by the parent.
func runFakeVMM(script string) {
	lines := strings.Split(script, "|")
	exitAfter := false
	if len(lines) > 0 && lines[len(lines)-1] == "EXIT" {
		exitAfter = true
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		fmt.Println(line)
	}
	if exitAfter {
		return
	}
	select {}
}

// fakeRunner is a runner that returns a fixed reader and records that
// stop was called. Used to test helloWith without spawning anything.
type fakeRunner struct {
	output     string
	startErr   error
	stopCalled bool
}

func (f *fakeRunner) run(_ context.Context, _ Config) (io.ReadCloser, func(), error) {
	if f.startErr != nil {
		return nil, nil, f.startErr
	}
	return io.NopCloser(strings.NewReader(f.output)),
		func() { f.stopCalled = true },
		nil
}

// validCfg returns a Config with all required fields populated. The
// paths point nowhere meaningful — tests that don't actually exec
// cloud-hypervisor never dereference them.
func validCfg() Config {
	return Config{
		CloudHypervisor: "/nonexistent/cloud-hypervisor",
		Kernel:          "/nonexistent/bzImage",
		Initramfs:       "/nonexistent/initrd",
	}
}

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

func TestConfigWithDefaultsFillsMemoryAndCPUs(t *testing.T) {
	cfg := Config{}.withDefaults()
	if cfg.Memory != "256M" {
		t.Errorf("Memory default = %q, want %q", cfg.Memory, "256M")
	}
	if cfg.CPUs != 1 {
		t.Errorf("CPUs default = %d, want 1", cfg.CPUs)
	}

	// Explicit values are preserved.
	cfg = Config{Memory: "1G", CPUs: 4}.withDefaults()
	if cfg.Memory != "1G" || cfg.CPUs != 4 {
		t.Errorf("withDefaults overwrote explicit values: %+v", cfg)
	}
}

func TestHelloWithFakeRunnerFindsBanner(t *testing.T) {
	r := &fakeRunner{
		output: "[    0.001] kernel boot\nrandom log\n" + agent.Banner + "\nlater output\n",
	}
	var buf bytes.Buffer
	err := helloWith(t.Context(), &buf, validCfg(), r)
	if err != nil {
		t.Fatalf("helloWith: %v", err)
	}
	if !r.stopCalled {
		t.Error("stop function was not called")
	}
	if !strings.Contains(buf.String(), agent.Banner) {
		t.Errorf("output missing banner: %q", buf.String())
	}
	// Output before banner is mirrored; output after banner should not
	// be — Hello returns the moment it sees the banner.
	if strings.Contains(buf.String(), "later output") {
		t.Errorf("output continued past banner: %q", buf.String())
	}
}

func TestHelloWithFakeRunnerReportsMissingBanner(t *testing.T) {
	r := &fakeRunner{output: "no banner here\nor here\n"}
	var buf bytes.Buffer
	err := helloWith(t.Context(), &buf, validCfg(), r)
	if !errors.Is(err, ErrBannerNotSeen) {
		t.Fatalf("err = %v, want ErrBannerNotSeen", err)
	}
	if !r.stopCalled {
		t.Error("stop function was not called even on banner miss")
	}
}

func TestHelloWithFakeRunnerPropagatesStartError(t *testing.T) {
	startErr := errors.New("synthetic start failure")
	r := &fakeRunner{startErr: startErr}
	err := helloWith(t.Context(), io.Discard, validCfg(), r)
	if !errors.Is(err, startErr) {
		t.Fatalf("err = %v, want %v", err, startErr)
	}
	if r.stopCalled {
		t.Error("stop must not be called when start fails")
	}
}

// errReader is an io.Reader that always returns an error. Lets us test
// that scanForBanner wraps non-EOF read errors instead of swallowing
// them as a banner-not-seen condition.
type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestScanForBannerWrapsReadError(t *testing.T) {
	want := errors.New("synthetic read failure")
	err := scanForBanner(io.Discard, errReader{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want wrap of %v", err, want)
	}
}

// TestCloudHypervisorRunnerViaFakeBinary exercises the real
// cloudHypervisorRunner by pointing it at this test binary, which TestMain
// turns into a fake VMM when DAFT_TEST_FAKE_VMM_LINES is set. We verify
// that:
//   - Hello succeeds against a "VMM" that prints kernel-style noise then
//     the banner.
//   - The stop function actually kills the child (no leaked process).
func TestCloudHypervisorRunnerViaFakeBinary(t *testing.T) {
	t.Setenv(fakeVMMEnv, "[    0.001] kernel boot|"+agent.Banner)

	cfg := Config{
		CloudHypervisor: os.Args[0],
		Kernel:          devNull,
		Initramfs:       devNull,
	}

	var buf bytes.Buffer
	err := Hello(t.Context(), &buf, cfg)
	if err != nil {
		t.Fatalf("Hello against fake VMM: %v\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), agent.Banner) {
		t.Fatalf("banner missing from captured output: %q", buf.String())
	}
}

// TestCloudHypervisorRunnerStopKillsChild confirms the stop function
// actually terminates the subprocess. We launch a fake VMM that would
// sleep forever, run Hello (which sees the banner immediately and
// returns), and assert the runtime is short — if stop didn't kill the
// child, the test would hang and time out instead of returning quickly.
func TestCloudHypervisorRunnerStopKillsChild(t *testing.T) {
	t.Setenv(fakeVMMEnv, agent.Banner) // no EXIT — child sleeps forever

	cfg := Config{
		CloudHypervisor: os.Args[0],
		Kernel:          devNull,
		Initramfs:       devNull,
	}

	start := time.Now()
	if err := Hello(t.Context(), io.Discard, cfg); err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Hello took %v — stop probably did not kill the child", elapsed)
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

// TestHelloIntegrationBootsVM is the real end-to-end test: build the
// sandbox-VM stack with `nix build`, set the env vars, run `go test`.
// Skipped if any required artifact env var is missing or KVM is not
// available. The mock-based tests above cover the orchestration logic
// regardless; this test exists to catch breakage in the artifact
// pipeline (kernel config, initramfs layout, cloud-hypervisor flags).
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

// FuzzScanForBanner exercises scanForBanner with arbitrary byte
// streams. The reader consumed in production is the VMM subprocess's
// stdout — bytes-shaped input from an untrusted source — which is
// exactly the shape CONTRIBUTING.md mandates a FuzzXxx target for.
//
// Invariant: scanForBanner must never panic. Banner-not-seen and read
// errors are fine; we only fail iterations the Go fuzzer detects as
// crashes (it already does that automatically).
//
// Run actively with:
//
//	go test -run='^$' -fuzz=FuzzScanForBanner -fuzztime=30s ./internal/sandbox
func FuzzScanForBanner(f *testing.F) {
	seeds := []string{
		"",
		agent.Banner,
		agent.Banner + "\n",
		"prefix\n" + agent.Banner + "\nsuffix",
		"no banner here\nor here\n",
		// Embedded NULs and other control bytes — kernel boot output
		// can contain weird control sequences.
		"\x00\x01\x02 " + agent.Banner + " \x7f",
		// Long lines, including over the initial scanner buffer (64K).
		strings.Repeat("a", 100*1024),
		// Many newlines.
		strings.Repeat("\n", 1024),
		// Many banner-like lines.
		strings.Repeat(agent.Banner+"\n", 100),
		// Banner split across what could look like a chunk boundary.
		strings.Repeat("x", 60*1024) + "\n" + agent.Banner,
		// Non-UTF8 bytes interleaved with the banner.
		"\xff\xfe\xfd " + agent.Banner,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		// Output is discarded — we're testing the parser, not the
		// rendering. Errors are expected for most random inputs.
		_ = scanForBanner(io.Discard, strings.NewReader(raw))
	})
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
