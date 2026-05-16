package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
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
	// fakeVirtiofsdEnv: when set the test binary impersonates virtiofsd
	// well enough for the sidecar's spawn / wait-for-socket / stop
	// dance — it parses --socket-path, creates a Unix socket node
	// there, and blocks until killed.
	fakeVirtiofsdEnv = "DAFT_TEST_FAKE_VIRTIOFSD"
	// devNull is /dev/null. Used as a stand-in for the kernel and
	// initramfs paths in tests that swap in a fake VMM — the fake
	// ignores its args, so the paths only need to exist.
	devNull = "/dev/null"
	// testMountpoint is the in-VM share mountpoint reused across
	// share tests; testBundlePath is the in-VM bundle path.
	// testTagDaftshare lives in integration_test.go because only
	// the tagged integration tests use it (the non-tagged unit
	// tests pick their own tag names per-test).
	testMountpoint    = "/share"
	testBundlePath    = "/share/bundle"
	testFakeVirtiofsd = "/nonexistent/virtiofsd"
)

func TestMain(m *testing.M) {
	if script := os.Getenv(fakeVMMEnv); script != "" {
		runFakeVMM(script)
		return
	}
	if os.Getenv(fakeVirtiofsdEnv) != "" {
		runFakeVirtiofsd(os.Args)
		return
	}
	os.Exit(m.Run())
}

// runFakeVirtiofsd impersonates virtiofsd's startup contract just
// enough for the sidecar tests: parse --socket-path, listen on it (so
// a socket inode appears), and block until killed. Other args are
// ignored; the real virtiofsd parses them strictly but the sidecar
// only cares about the socket appearing.
func runFakeVirtiofsd(args []string) {
	var socketPath string
	for i, a := range args {
		if a == "--socket-path" && i+1 < len(args) {
			socketPath = args[i+1]
			break
		}
	}
	if socketPath == "" {
		os.Exit(2)
	}
	l, err := net.Listen("unix", socketPath)
	if err != nil {
		os.Exit(3)
	}
	defer func() { _ = l.Close() }()
	select {}
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
	output          string
	startErr        error
	stopCalled      bool
	gotShareSockets []string // captured shareSockets arg, for share tests
}

func (f *fakeRunner) run(_ context.Context, _ Config, shareSockets []string) (io.ReadCloser, func(), error) {
	f.gotShareSockets = shareSockets
	if f.startErr != nil {
		return nil, nil, f.startErr
	}
	return io.NopCloser(strings.NewReader(f.output)),
		func() { f.stopCalled = true },
		nil
}

// fakeShareSidecar is a shareSidecar that returns canned socket paths
// without spawning anything. Used to test the share-orchestration
// branches of helloWith.
type fakeShareSidecar struct {
	sockets    []string
	startErr   error
	stopCalled bool
}

func (f *fakeShareSidecar) start(_ context.Context, cfg Config) (sockets []string, stop func(), err error) {
	if f.startErr != nil {
		return nil, nil, f.startErr
	}
	// If sockets is unset, generate one per share so callers don't
	// need to keep them in sync with cfg.Shares.
	socks := f.sockets
	if socks == nil {
		socks = make([]string, len(cfg.Shares))
		for i := range socks {
			socks[i] = fmt.Sprintf("/tmp/fake-vhost-%d.sock", i)
		}
	}
	return socks, func() { f.stopCalled = true }, nil
}

// noShareSidecar implements shareSidecar but is never called — passed
// to helloWith in tests that exercise the no-share path. Tests assert
// helloWith honoured len(Config.Shares) == 0 by checking that start
// was never invoked (panics on call).
type noShareSidecar struct{}

func (noShareSidecar) start(context.Context, Config) (sockets []string, stop func(), err error) {
	panic("noShareSidecar.start called: helloWith should not start virtiofsd when Shares is empty")
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
	if cfg.Memory != defaultMemory {
		t.Errorf("Memory default = %q, want %q", cfg.Memory, defaultMemory)
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
	err := helloWith(t.Context(), &buf, validCfg(), r, noShareSidecar{})
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
	err := helloWith(t.Context(), &buf, validCfg(), r, noShareSidecar{})
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
	err := helloWith(t.Context(), io.Discard, validCfg(), r, noShareSidecar{})
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

func TestHelloWithShareStartsSidecarAndPassesSocketsToRunner(t *testing.T) {
	dir := t.TempDir()

	cfg := validCfg()
	cfg.Virtiofsd = testFakeVirtiofsd
	cfg.Shares = []*ShareConfig{
		{HostDir: dir, Tag: testTagWorkspace, Mountpoint: testMountpoint},
	}

	r := &fakeRunner{output: agent.Banner + "\n"}
	sc := &fakeShareSidecar{sockets: []string{"/tmp/fake.sock"}}

	if err := helloWith(t.Context(), io.Discard, cfg, r, sc); err != nil {
		t.Fatalf("helloWith: %v", err)
	}
	if !sc.stopCalled {
		t.Error("share sidecar stop function was not called")
	}
	if len(r.gotShareSockets) != 1 || r.gotShareSockets[0] != sc.sockets[0] {
		t.Errorf("runner saw shareSockets=%v, want %v", r.gotShareSockets, sc.sockets)
	}
}

func TestHelloWithMultipleSharesPassesEverySocket(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	cfg := validCfg()
	cfg.Virtiofsd = testFakeVirtiofsd
	cfg.Shares = []*ShareConfig{
		{HostDir: dir1, Tag: "bundles", Mountpoint: "/bundles"},
		{HostDir: dir2, Tag: "workspace", Mountpoint: "/workspace"},
	}

	r := &fakeRunner{output: agent.Banner + "\n"}
	sc := &fakeShareSidecar{sockets: []string{"/tmp/a.sock", "/tmp/b.sock"}}

	if err := helloWith(t.Context(), io.Discard, cfg, r, sc); err != nil {
		t.Fatalf("helloWith: %v", err)
	}
	if len(r.gotShareSockets) != 2 {
		t.Fatalf("runner saw %d sockets, want 2", len(r.gotShareSockets))
	}
}

func TestHelloRejectsDuplicateShareTag(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Virtiofsd = testFakeVirtiofsd
	cfg.Shares = []*ShareConfig{
		{HostDir: dir, Tag: "dup", Mountpoint: "/a"},
		{HostDir: dir, Tag: "dup", Mountpoint: "/b"},
	}
	err := Hello(t.Context(), io.Discard, cfg)
	if err == nil || !strings.Contains(err.Error(), "duplicate Share.Tag") {
		t.Fatalf("err = %v, want duplicate-tag error", err)
	}
}

func TestHelloWithSharePropagatesSidecarStartError(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Virtiofsd = testFakeVirtiofsd
	cfg.Shares = []*ShareConfig{
		{HostDir: dir, Tag: "ws", Mountpoint: testMountpoint},
	}

	startErr := errors.New("synthetic virtiofsd failure")
	sc := &fakeShareSidecar{startErr: startErr}
	r := &fakeRunner{output: agent.Banner + "\n"}

	err := helloWith(t.Context(), io.Discard, cfg, r, sc)
	if !errors.Is(err, startErr) {
		t.Fatalf("err = %v, want wrap of %v", err, startErr)
	}
	if r.gotShareSockets != nil {
		t.Error("runner must not be called when share sidecar fails")
	}
}

func TestHelloRejectsShareWithoutVirtiofsd(t *testing.T) {
	dir := t.TempDir()
	cfg := validCfg()
	cfg.Shares = []*ShareConfig{
		{HostDir: dir, Tag: "ws", Mountpoint: testMountpoint},
	}
	// No cfg.Virtiofsd set.

	err := Hello(t.Context(), io.Discard, cfg)
	if err == nil || !strings.Contains(err.Error(), "Virtiofsd") {
		t.Fatalf("err = %v, want mention of Virtiofsd", err)
	}
}

func TestShareConfigValidate(t *testing.T) {
	dir := t.TempDir()
	tmpFile := filepath.Join(dir, "regular-file")
	if err := os.WriteFile(tmpFile, nil, 0o600); err != nil {
		t.Fatalf("create file: %v", err)
	}

	tests := []struct {
		name      string
		s         ShareConfig
		errSubstr string
	}{
		{
			name:      "missing host dir",
			s:         ShareConfig{Tag: "t", Mountpoint: "/m"},
			errSubstr: "Share.HostDir",
		},
		{
			name:      "relative host dir",
			s:         ShareConfig{HostDir: "rel", Tag: "t", Mountpoint: "/m"},
			errSubstr: "must be absolute",
		},
		{
			name:      "missing tag",
			s:         ShareConfig{HostDir: dir, Mountpoint: "/m"},
			errSubstr: "Share.Tag",
		},
		{
			name:      "relative mountpoint",
			s:         ShareConfig{HostDir: dir, Tag: "t", Mountpoint: "rel"},
			errSubstr: "must be absolute",
		},
		{
			name:      "host dir nonexistent",
			s:         ShareConfig{HostDir: "/nope/does-not-exist", Tag: "t", Mountpoint: "/m"},
			errSubstr: "Share.HostDir",
		},
		{
			name:      "host dir is a file",
			s:         ShareConfig{HostDir: tmpFile, Tag: "t", Mountpoint: "/m"},
			errSubstr: "not a directory",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.s.validate()
			if err == nil || !strings.Contains(err.Error(), tt.errSubstr) {
				t.Fatalf("err = %v, want substring %q", err, tt.errSubstr)
			}
		})
	}
}

func TestBuildKernelCmdlineIncludesShare(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.Shares = []*ShareConfig{{Tag: "ws", Mountpoint: testMountpoint}}
	got := buildKernelCmdline(cfg)
	if !strings.Contains(got, agent.ShareCmdlineKey+"=ws:/share") {
		t.Errorf("cmdline %q missing share spec", got)
	}
	if !strings.Contains(got, "console=hvc0") {
		t.Errorf("cmdline %q lost the base cmdline", got)
	}
}

func TestShareSocketArgFormat(t *testing.T) {
	got := shareSocketArg("workspace", "/tmp/vhost.sock")
	want := "tag=workspace,socket=/tmp/vhost.sock"
	if got != want {
		t.Errorf("shareSocketArg = %q, want %q", got, want)
	}
}

func TestBuildKernelCmdlineIncludesBundle(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.Bundle = testBundlePath
	got := buildKernelCmdline(cfg)
	if !strings.Contains(got, agent.BundleCmdlineKey+"=/share/bundle") {
		t.Errorf("cmdline %q missing bundle path", got)
	}
}

func TestBuildKernelCmdlineIncludesWorkspaceOverlay(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.Shares = []*ShareConfig{{Tag: "ws", Mountpoint: testMountpoint, Overlay: true}}
	got := buildKernelCmdline(cfg)
	if !strings.Contains(got, agent.WorkspaceOverlayCmdlineKey+"="+testMountpoint) {
		t.Errorf("cmdline %q missing overlay token", got)
	}
}

func TestBuildKernelCmdlineOmitsOverlayWhenFlagFalse(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.Shares = []*ShareConfig{{Tag: "ws", Mountpoint: testMountpoint}}
	got := buildKernelCmdline(cfg)
	if strings.Contains(got, agent.WorkspaceOverlayCmdlineKey) {
		t.Errorf("cmdline %q should not contain overlay token when Overlay=false", got)
	}
}

// TestBuildKernelCmdlineIncludesPortForwards pins the C1.2 wiring:
// each PortForwards entry emits exactly one daft.port=<vsock>:<container>
// token, in declaration order, so the in-VM agent sees the same shape
// FormatPortForwardCmdline produces.
func TestBuildKernelCmdlineIncludesPortForwards(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.PortForwards = []PortForward{
		{VsockPort: 10000, ContainerPort: 8080},
		{VsockPort: 10001, ContainerPort: 22},
	}
	got := buildKernelCmdline(cfg)
	for _, want := range []string{"daft.port=10000:8080", "daft.port=10001:22"} {
		if !strings.Contains(got, want) {
			t.Errorf("cmdline %q missing %q", got, want)
		}
	}
}

// TestValidatePortForwardsRejectsDuplicateVsockPort exercises the
// duplicate-bind guard. Without it CH would surface the collision
// as an opaque virtio-vsock error after the VM was already spawned.
func TestValidatePortForwardsRejectsDuplicateVsockPort(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.PortForwards = []PortForward{
		{VsockPort: 10000, ContainerPort: 80},
		{VsockPort: 10000, ContainerPort: 81},
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want duplicate-port error", err)
	}
}

// TestValidatePortForwardsRejectsZeroPorts covers the two
// non-zero invariants. Both halves of every PortForward must be
// set; a zero on either side would silently drop a port mapping
// from the agent's parse without a useful error.
func TestValidatePortForwardsRejectsZeroPorts(t *testing.T) {
	for _, tc := range []struct {
		name string
		pf   PortForward
		want string
	}{
		{"zero vsock", PortForward{VsockPort: 0, ContainerPort: 80}, "VsockPort"},
		{"zero container", PortForward{VsockPort: 10000, ContainerPort: 0}, "ContainerPort"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validCfg().withDefaults()
			cfg.PortForwards = []PortForward{tc.pf}
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %s in message", err, tc.want)
			}
		})
	}
}

// TestValidatePortForwardsRejectsTracePortCollision pins the
// cross-field invariant: a PortForward.VsockPort that matches the
// already-assigned Vsock.TracePort would result in CH refusing the
// second guest-side bind at boot.
func TestValidatePortForwardsRejectsTracePortCollision(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.Vsock = &VsockConfig{
		HostSocket: testHostSocketPath,
		GuestCID:   3,
		TracePort:  10000,
	}
	cfg.PortForwards = []PortForward{{VsockPort: 10000, ContainerPort: 80}}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "TracePort") {
		t.Fatalf("err = %v, want TracePort-collision error", err)
	}
}

func TestHelloRejectsRelativeBundle(t *testing.T) {
	cfg := validCfg()
	cfg.Bundle = "relative/bundle"
	err := Hello(t.Context(), io.Discard, cfg)
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("err = %v, want mention of absolute path", err)
	}
}

func TestBuildMemoryArgAddsSharedOnForShare(t *testing.T) {
	cfg := validCfg().withDefaults()
	if buildMemoryArg(cfg) != "size="+defaultMemory {
		t.Errorf("no-share memory = %q, want size=%s", buildMemoryArg(cfg), defaultMemory)
	}
	cfg.Shares = []*ShareConfig{{Tag: "ws", Mountpoint: testMountpoint}}
	if got := buildMemoryArg(cfg); !strings.Contains(got, "shared=on") {
		t.Errorf("with-share memory = %q, want shared=on", got)
	}
}

// TestVirtiofsdSidecarStartViaFakeBinary points the real
// virtiofsdSidecar at this test binary, which TestMain turns into a
// fake "virtiofsd" that just creates the socket node and blocks. This
// covers the spawn / wait-for-socket / stop logic without needing a
// real virtiofsd or its dependencies.
func TestVirtiofsdSidecarStartViaFakeBinary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(fakeVirtiofsdEnv, "1")

	cfg := Config{
		Virtiofsd: os.Args[0],
		Shares: []*ShareConfig{{
			HostDir:    dir,
			Tag:        "ws",
			Mountpoint: testMountpoint,
		}},
	}

	sockets, stop, err := virtiofsdSidecar{}.start(t.Context(), cfg)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer stop()

	if len(sockets) != 1 {
		t.Fatalf("got %d sockets, want 1", len(sockets))
	}
	info, err := os.Stat(sockets[0])
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("path %s is not a socket: mode=%v", sockets[0], info.Mode())
	}
}

func TestVirtiofsdSidecarTimesOutWhenSocketNeverAppears(t *testing.T) {
	dir := t.TempDir()
	// fakeVirtiofsdEnv unset → fake refuses to create the socket.

	cfg := Config{
		Virtiofsd: os.Args[0],
		Shares: []*ShareConfig{{
			HostDir:    dir,
			Tag:        "ws",
			Mountpoint: testMountpoint,
		}},
	}

	// Tighten the timeout for the test by using a context deadline.
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()

	_, _, err := virtiofsdSidecar{}.start(ctx, cfg)
	if err == nil {
		t.Fatal("start succeeded with no socket; want timeout error")
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

// Integration tests + benchmarks live under the `integration` build
// tag in integration_test.go (Linux + KVM + Nix-built artefacts).
// Run them via `make integration`.

// FuzzConfigFromEnv feeds arbitrary strings to every env var
// ConfigFromEnv reads. The current parser only does string membership
// checks plus a single strconv.Atoi, so the seed corpus alone covers
// every branch — but the fuzz target exists as a guard: anyone who
// later adds richer parsing here (a memory-size validator, an
// allowlist, a path canonicaliser) inherits a panic-freedom invariant
// for free.
//
// Run actively with:
//
//	go test -run='^$' -fuzz=FuzzConfigFromEnv -fuzztime=30s ./internal/sandbox
func FuzzConfigFromEnv(f *testing.F) {
	const ch, k, i = "/ch", "/k", "/i"
	seeds := [][5]string{
		{"", "", "", "", ""},                        // all missing
		{ch, k, i, "", ""},                          // required only
		{ch, k, i, defaultMemory, "1"},              // happy path
		{ch, k, i, "1G", "4"},                       // larger sizes
		{ch, k, i, "", "not-a-number"},              // bad CPUs
		{ch, k, i, "", "0"},                         // zero CPUs
		{ch, k, i, "", "-1"},                        // negative CPUs
		{ch, k, i, "", "999999999999999999"},        // overflow
		{strings.Repeat("a", 4096), k, i, "m", "1"}, // long values
	}
	for _, s := range seeds {
		f.Add(s[0], s[1], s[2], s[3], s[4])
	}

	f.Fuzz(func(t *testing.T, ch, kernel, initramfs, memory, cpus string) {
		// POSIX env vars cannot contain NUL; the kernel rejects such
		// values at execve time, so they can never reach ConfigFromEnv
		// in production. Skip rather than letting t.Setenv fail.
		for _, v := range []string{ch, kernel, initramfs, memory, cpus} {
			if strings.ContainsRune(v, 0) {
				t.Skip()
			}
		}
		t.Setenv(EnvCloudHypervisor, ch)
		t.Setenv(EnvKernel, kernel)
		t.Setenv(EnvInitramfs, initramfs)
		t.Setenv(EnvMemory, memory)
		t.Setenv(EnvCPUs, cpus)
		_, _ = ConfigFromEnv()
	})
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
