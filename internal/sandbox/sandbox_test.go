package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/agent"
	"github.com/localai-org/darkaifactory/internal/observability"
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
	if cgroupStubInUnitTests {
		// Unit tests run on hosts without cgroup delegation and use fake
		// VMM runners that need no real per-VM accounting. Stub the
		// cgroup constructor to skip creation so the hard precondition
		// (which would correctly fail on a non-delegated host) doesn't
		// abort every boot-orchestration test. Integration tests build
		// with cgroupStubInUnitTests=false, run delegated (the make
		// integration systemd-run wrap), and exercise the real path.
		createVMCgroupFn = func(string) (string, error) { return "", nil }
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
	gotShareSockets []string                        // captured shareSockets arg, for share tests
	gotCollectNet   func() *observability.NetSample // captured callback, for net counter tests
	gotCgroupDir    string                          // captured cgroupDir arg, for shared-cgroup tests
	// neverEOF keeps the returned stdout reader blocking after the
	// output has been drained so Serve tests can verify the
	// ctx-cancel branch (a real VMM's stdout stays open until the
	// VM is killed). stop() closes the writer to unblock the reader.
	neverEOF bool
}

func (f *fakeRunner) run(_ context.Context, _ Config, shareSockets []string, cgroupDir string, collectNet func() *observability.NetSample) (io.ReadCloser, func(), error) {
	f.gotCollectNet = collectNet
	f.gotCgroupDir = cgroupDir
	f.gotShareSockets = shareSockets
	if f.startErr != nil {
		return nil, nil, f.startErr
	}
	if f.neverEOF {
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte(f.output))
			// Leave the writer open; stop() below closes it.
		}()
		return pr, func() {
			f.stopCalled = true
			_ = pw.Close()
		}, nil
	}
	return io.NopCloser(strings.NewReader(f.output)),
		func() { f.stopCalled = true },
		nil
}

// fakeShareSidecar is a shareSidecar that returns canned socket paths
// without spawning anything. Used to test the share-orchestration
// branches of helloWith.
type fakeShareSidecar struct {
	sockets      []string
	startErr     error
	stopCalled   bool
	gotCgroupDir string // captured cgroupDir arg, for shared-cgroup tests
}

func (f *fakeShareSidecar) start(_ context.Context, cfg Config, cgroupDir string) (sockets []string, stop func(), err error) {
	f.gotCgroupDir = cgroupDir
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

func (noShareSidecar) start(context.Context, Config, string) (sockets []string, stop func(), err error) {
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

// TestCloudHypervisorRunner_ObserverHappyPath: ObserverLogPath
// set but cgroup setup fails (the common no-delegation host
// case), so cmd.Start succeeds without CLONE_INTO_CGROUP and the
// observer writes /proc samples to the log. Covers the
// observer-start + observer-stop happy paths in run().
func TestCloudHypervisorRunner_ObserverHappyPath(t *testing.T) {
	t.Setenv(fakeVMMEnv, agent.Banner)
	logDir := t.TempDir()
	logPath := filepath.Join(logDir, "obs.jsonl")
	cfg := Config{
		CloudHypervisor:  os.Args[0],
		Kernel:           devNull,
		Initramfs:        devNull,
		ObserverLogPath:  logPath,
		ObserverMaxBytes: -1,
	}
	start := time.Now()
	if err := Hello(t.Context(), io.Discard, cfg); err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Hello took %v — stop probably did not kill the child", elapsed)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("observer log not created at %s: %v", logPath, err)
	}
}

// TestCloudHypervisorRunner_ObserverAndCgroupTeardown exercises the
// new cgroup-wrap + observer code in cloudHypervisorRunner.run by
// pointing it at the fake VMM, a fake cgroup root, and a real
// observer log path. The CLONE_INTO_CGROUP step at fork will
// reject our tempdir as "not a cgroup v2 directory" so cmd.Start
// errors — that's the test's *point*: it exercises the failure-
// path cleanup (close fd, remove cgroup, surface the start error).
func TestCloudHypervisorRunner_ObserverAndCgroupTeardown(t *testing.T) {
	// This test's whole point is the real createVMCgroup + placeInCgroup
	// failure path, so opt back out of the unit-build stub (TestMain) for
	// its duration.
	prevFn := createVMCgroupFn
	createVMCgroupFn = createVMCgroup
	t.Cleanup(func() { createVMCgroupFn = prevFn })

	fakeRoot := t.TempDir()
	prev := observability.SwapCgroupRoot(fakeRoot)
	t.Cleanup(func() { observability.SwapCgroupRoot(prev) })
	parent, err := observability.CurrentCgroup()
	if err != nil {
		t.Fatalf("CurrentCgroup: %v", err)
	}
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatalf("seed parent: %v", err)
	}

	t.Setenv(fakeVMMEnv, agent.Banner)
	logDir := t.TempDir()
	cfg := Config{
		CloudHypervisor:  os.Args[0],
		Kernel:           devNull,
		Initramfs:        devNull,
		ObserverLogPath:  filepath.Join(logDir, "obs.jsonl"),
		ObserverMaxBytes: -1,
	}

	err = Hello(t.Context(), io.Discard, cfg)
	if err == nil {
		t.Fatal("expected Hello to error (fake cgroup root rejected by CLONE_INTO_CGROUP)")
	}
	if !strings.Contains(err.Error(), "start cloud-hypervisor") {
		t.Errorf("err = %v, want 'start cloud-hypervisor' substring", err)
	}
	// Cleanup must have removed the cgroup directory we created.
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "daft-vm-") {
			t.Errorf("leaked cgroup dir under fake parent: %s", e.Name())
		}
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

// TestHelloWithSharedCgroupThreadsSameDirToRunnerAndShare pins
// the invariant that helloWith creates the per-VM cgroup once and
// threads the SAME path into both the share sidecar (virtiofsd
// placement) and the runner (CH placement). Anything else would
// split accounting across two cgroups and break the cgroup view's
// "VM tree in one place" promise.
func TestHelloWithSharedCgroupThreadsSameDirToRunnerAndShare(t *testing.T) {
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
	// Both must receive the same value, whatever it is. On a
	// non-delegated host that's ""; on a delegated host it's the
	// fresh cgroup dir. Either way they have to agree.
	if r.gotCgroupDir != sc.gotCgroupDir {
		t.Errorf("runner.cgroupDir=%q != share.cgroupDir=%q (must be same path)",
			r.gotCgroupDir, sc.gotCgroupDir)
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

// TestBuildKernelCmdlineHasNoDaftTokens pins the slice-3 cutover: every
// per-VM setting now travels over the vsock control channel, so the kernel
// cmdline carries zero daft.* tokens no matter how richly the Config is
// populated — only the static base remains. The settings themselves are
// covered by TestBuildGuestConfigProjectsConfig.
func TestBuildKernelCmdlineHasNoDaftTokens(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.Bundle = testBundlePath
	cfg.Shares = []*ShareConfig{{Tag: "ws", Mountpoint: testMountpoint, Overlay: true}}
	cfg.PortForwards = []PortForward{{VsockPort: 10000, ContainerPort: 8080}}
	cfg.OpensnoopLogPath = "/share/opensnoop.log"
	cfg.ObserveSummaryPath = "/share/progress.json"
	cfg.Vsock = &VsockConfig{
		HostSocket: testHostSocketPath,
		GuestCID:   3,
		Attach:     true,
		Net:        true,
		Trace:      true,
	}
	got := buildKernelCmdline(cfg)
	if got != kernelCmdlineBase {
		t.Errorf("cmdline = %q, want exactly the base %q (zero daft.* tokens)", got, kernelCmdlineBase)
	}
	if strings.Contains(got, "daft.") {
		t.Errorf("cmdline %q still carries a daft.* token", got)
	}
}

func TestShareSocketArgFormat(t *testing.T) {
	got := shareSocketArg("workspace", "/tmp/vhost.sock")
	want := "tag=workspace,socket=/tmp/vhost.sock"
	if got != want {
		t.Errorf("shareSocketArg = %q, want %q", got, want)
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

// TestValidateRejectsRelativeObserverLogPath: ObserverLogPath
// must be absolute. Relative paths would be resolved against
// whatever cwd daft-sandboxd happens to be in — surprising and
// post-launch-fragile (cwd can change). Caught at validate so
// no half-spawned cloud-hypervisor leaks before the operator
// sees the diagnostic.
func TestValidateRejectsRelativeObserverLogPath(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.ObserverLogPath = "obs.jsonl"
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "ObserverLogPath") {
		t.Fatalf("err = %v, want ObserverLogPath-relative error", err)
	}
}

// TestValidateAcceptsEmptyObserverLogPath: empty disables the
// observer entirely, which is the right default for unit tests
// that don't want a goroutine writing to the filesystem. Pin
// this so a future "always require an observer path" change is
// at least a deliberate decision rather than a drive-by.
func TestValidateAcceptsEmptyObserverLogPath(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.ObserverLogPath = ""
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate with empty ObserverLogPath: %v", err)
	}
}

// TestRemoveVMCgroupBestEffort_EmptyPathNoop: the helper is
// called unconditionally in run()'s cleanup, including when
// cgroup wrap was skipped. Empty path must be a clean no-op,
// not an attempt to rmdir "".
func TestRemoveVMCgroupBestEffort_EmptyPathNoop(t *testing.T) {
	removeVMCgroupBestEffort("") // must not panic, must not log
}

// TestRemoveVMCgroupBestEffort_LogsError: a non-empty path that
// fails to rmdir (because it doesn't exist, isn't under cgroup
// root, etc.) goes through oops.Log without bubbling up.
func TestRemoveVMCgroupBestEffort_LogsError(t *testing.T) {
	// /tmp/... is not under /sys/fs/cgroup so RemoveCgroup's
	// safety rail rejects the call — and we want the helper to
	// swallow that into a log, not propagate.
	removeVMCgroupBestEffort("/tmp/nonexistent-cgroup-test")
}

// TestVMCgroupName_UniquePerCall pins that vmCgroupName never
// returns the same string twice — the kernel rejects mkdir on
// existing cgroup directories with EEXIST, and a per-VM
// observer-log collision would silently merge two VMs'
// telemetry. Counter increments → suffix differs even when the
// hash collides.
func TestVMCgroupName_UniquePerCall(t *testing.T) {
	cfg := Config{ObserverLogPath: "/tmp/x.jsonl"}
	a := vmCgroupName(cfg)
	b := vmCgroupName(cfg)
	if a == b {
		t.Fatalf("vmCgroupName collided across calls: %q == %q", a, b)
	}
	if !strings.HasPrefix(a, "daft-vm-") || !strings.HasPrefix(b, "daft-vm-") {
		t.Errorf("unexpected prefix: a=%q b=%q", a, b)
	}
}

// TestVMCgroupName_HandlesEmptyLogPath: a caller using the
// cgroup wrap without the observer (no log path) still gets a
// well-formed name.
func TestVMCgroupName_HandlesEmptyLogPath(t *testing.T) {
	got := vmCgroupName(Config{})
	if !strings.HasPrefix(got, "daft-vm-") {
		t.Errorf("got %q, want daft-vm- prefix", got)
	}
}

// TestCreateVMCgroup_ErrorsWithoutDelegation: per the strict posture
// (CONTRIBUTING "Probe preconditions at startup; no silent fallbacks")
// createVMCgroup is a hard precondition — when the parent cgroup can't
// host a child it returns an error, NOT "" with a silent /proc-only
// degrade. We force that deterministically with a fake cgroup root
// whose parent path is left unseeded, so CreateSubCgroup's mkdir fails
// ENOENT regardless of whether this host happens to have real
// delegation. (Delegation itself is guaranteed earlier, at process
// startup, by the cgroupwrap systemd-run self-wrap.)
func TestCreateVMCgroup_ErrorsWithoutDelegation(t *testing.T) {
	fakeRoot := t.TempDir()
	prev := observability.SwapCgroupRoot(fakeRoot)
	t.Cleanup(func() { observability.SwapCgroupRoot(prev) })

	// Deliberately do NOT seed the session-scope parent under fakeRoot,
	// so the child mkdir has no parent directory to create into.
	path, err := createVMCgroup(vmCgroupName(Config{ObserverLogPath: "/tmp/setup-test"}))
	if err == nil {
		_ = observability.RemoveCgroup(path)
		t.Fatal("createVMCgroup succeeded against an unseeded fake root; want a hard error")
	}
	if path != "" {
		t.Errorf("createVMCgroup returned path %q alongside its error; want empty", path)
	}
}

// TestCreateVMCgroup_SuccessUnderFakeRoot drives the create path
// against a tempdir cgroup root, so the success-side branch stays
// covered on hosts without real cgroup delegation. We pre-populate
// the tempdir with the session-scope path that /proc/self/cgroup
// reports, so CurrentCgroup returns a writable parent.
func TestCreateVMCgroup_SuccessUnderFakeRoot(t *testing.T) {
	fakeRoot := t.TempDir()
	prev := observability.SwapCgroupRoot(fakeRoot)
	t.Cleanup(func() { observability.SwapCgroupRoot(prev) })

	// Mirror the real session-scope path under the fake root so
	// CurrentCgroup's filepath.Join(cgroupV2Root, sessionPath)
	// resolves to an existing, writable directory.
	realParent, err := observability.CurrentCgroup()
	if err != nil {
		t.Fatalf("CurrentCgroup after swap: %v", err)
	}
	if err := os.MkdirAll(realParent, 0o750); err != nil {
		t.Fatalf("seed fake parent %s: %v", realParent, err)
	}

	path, err := createVMCgroup(vmCgroupName(Config{ObserverLogPath: "/tmp/setup-success-test"}))
	t.Cleanup(func() {
		if path != "" {
			_ = observability.RemoveCgroup(path)
		}
	})
	if err != nil {
		t.Fatalf("createVMCgroup under fake root: %v", err)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Errorf("cgroup dir not created at %s: err=%v info=%v", path, err, info)
	}
}

// TestPlaceInCgroup_EmptyDirNoop pins that placeInCgroup with no
// cgroup is a no-op: it doesn't touch SysProcAttr and the returned
// closer is safe to call.
func TestPlaceInCgroup_EmptyDirNoop(t *testing.T) {
	cmd := exec.Command("/bin/true")
	closeFD := placeInCgroup(cmd, "")
	if cmd.SysProcAttr != nil {
		t.Errorf("SysProcAttr modified for empty cgroup dir: %+v", cmd.SysProcAttr)
	}
	closeFD() // must not panic
}

// TestPlaceInCgroup_OpensFD drives the open + wire path under a
// fake cgroup root so we exercise the SysProcAttr.UseCgroupFD
// branch without needing real delegation. The returned closer
// must release the fd; we don't fork — placeInCgroup hands off
// to cmd.Start, which the orchestrator owns.
func TestPlaceInCgroup_OpensFD(t *testing.T) {
	fakeRoot := t.TempDir()
	prev := observability.SwapCgroupRoot(fakeRoot)
	t.Cleanup(func() { observability.SwapCgroupRoot(prev) })

	realParent, err := observability.CurrentCgroup()
	if err != nil {
		t.Fatalf("CurrentCgroup: %v", err)
	}
	if err := os.MkdirAll(realParent, 0o750); err != nil {
		t.Fatalf("seed fake parent: %v", err)
	}
	dir, err := createVMCgroup(vmCgroupName(Config{ObserverLogPath: "/tmp/place-fd-test"}))
	if err != nil {
		t.Fatalf("createVMCgroup under fake root: %v", err)
	}
	t.Cleanup(func() { _ = observability.RemoveCgroup(dir) })

	cmd := exec.Command("/bin/true")
	closeFD := placeInCgroup(cmd, dir)
	t.Cleanup(closeFD)
	// SysProcAttr being non-nil and the CgroupFD-style wiring
	// being applied is platform-specific; we just pin that the
	// helper didn't fall back to a no-op when it had a valid dir.
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr nil after placeInCgroup with valid dir")
	}
}

// TestStartObserverIfConfigured covers the cfg.ObserverLogPath
// switch + best-effort error handling in the helper extracted
// from cloudHypervisorRunner.run. The fully wired path
// (real cloud-hypervisor, real PID) is covered by integration
// tests; this fills the unit-test side so the lookup decisions
// don't drift silently.
func TestStartObserverIfConfigured(t *testing.T) {
	t.Run("empty path returns nil", func(t *testing.T) {
		obs := startObserverIfConfigured(Config{}, 1, "", nil)
		if obs != nil {
			_ = obs.Stop()
			t.Fatal("want nil, got Observer")
		}
	})
	t.Run("invalid pid logs and returns nil", func(t *testing.T) {
		cfg := Config{ObserverLogPath: "/tmp/x.jsonl"}
		obs := startObserverIfConfigured(cfg, 0, "", nil)
		if obs != nil {
			_ = obs.Stop()
			t.Fatal("want nil for pid=0, got Observer")
		}
	})
	t.Run("relative path logs and returns nil", func(t *testing.T) {
		// observability.New rejects relative paths; we want
		// startObserverIfConfigured to swallow that to nil rather
		// than panic.
		cfg := Config{ObserverLogPath: "relative.jsonl"}
		obs := startObserverIfConfigured(cfg, 1, "", nil)
		if obs != nil {
			_ = obs.Stop()
			t.Fatal("want nil for relative path, got Observer")
		}
	})
	t.Run("happy path returns started observer", func(t *testing.T) {
		dir := t.TempDir()
		cfg := Config{ObserverLogPath: filepath.Join(dir, "obs.jsonl")}
		obs := startObserverIfConfigured(cfg, os.Getpid(), "", nil)
		if obs == nil {
			t.Fatal("want Observer, got nil")
		}
		if err := obs.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	})
}

// TestValidatePortForwardsRejectsTracePortCollision pins the
// cross-field invariant: a PortForward.VsockPort that matches an enabled
// feature's fixed port (here the trace channel's DefaultTraceVsockPort)
// would result in CH refusing the second guest-side bind at boot.
func TestValidatePortForwardsRejectsTracePortCollision(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.Vsock = &VsockConfig{
		HostSocket: testHostSocketPath,
		GuestCID:   3,
		Trace:      true,
	}
	cfg.PortForwards = []PortForward{{VsockPort: agent.DefaultTraceVsockPort, ContainerPort: 80}}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "trace channel") {
		t.Fatalf("err = %v, want trace-channel-collision error", err)
	}
}

// TestValidatePortForwardsRejectsNetPortCollision — a PortForward.VsockPort
// equal to the enabled net channel's fixed port would race the sandboxnet
// listener for the same guest-side bind.
func TestValidatePortForwardsRejectsNetPortCollision(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.Vsock = &VsockConfig{
		HostSocket: testHostSocketPath,
		GuestCID:   3,
		Net:        true,
	}
	cfg.PortForwards = []PortForward{{VsockPort: agent.DefaultNetVsockPort, ContainerPort: 80}}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "net channel") {
		t.Fatalf("err = %v, want net-channel-collision error", err)
	}
}

// TestValidatePortForwardsAllowsFeaturePortWhenFeatureOff confirms the
// reserved-port guard keys off the feature bit: a forward may reuse a
// feature's constant port when that feature is disabled.
func TestValidatePortForwardsAllowsFeaturePortWhenFeatureOff(t *testing.T) {
	cfg := validCfg().withDefaults()
	cfg.Vsock = &VsockConfig{HostSocket: testHostSocketPath, GuestCID: 3} // no features
	cfg.PortForwards = []PortForward{{VsockPort: agent.DefaultAttachVsockPort, ContainerPort: 80}}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() = %v, want nil (attach off, port free)", err)
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

	sockets, stop, err := virtiofsdSidecar{}.start(t.Context(), cfg, "")
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

	_, _, err := virtiofsdSidecar{}.start(ctx, cfg, "")
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

// TestServeReturnsOnRunnerExit pins that Serve returns nil once
// the fake VMM's stdout closes (i.e., the runner naturally exits)
// and tears down via stop().
func TestServeReturnsOnRunnerExit(t *testing.T) {
	r := &fakeRunner{output: "boot line\nmore output\n"}
	var buf bytes.Buffer
	err := serveWith(t.Context(), &buf, validCfg(), r, noShareSidecar{})
	if err != nil {
		t.Fatalf("serveWith: %v", err)
	}
	if !r.stopCalled {
		t.Error("stop function was not called after Serve returned")
	}
	// Serve mirrors the entire stdout, not just up to a banner.
	if !strings.Contains(buf.String(), "more output") {
		t.Errorf("output missing post-banner content: %q", buf.String())
	}
}

// TestServeReturnsOnCtxCancel pins the ctx-driven teardown
// branch: Serve blocks until ctx is cancelled even though the
// runner would otherwise stay alive indefinitely.
func TestServeReturnsOnCtxCancel(t *testing.T) {
	r := &fakeRunner{output: "boot\n", neverEOF: true}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- serveWith(ctx, &bytes.Buffer{}, validCfg(), r, noShareSidecar{})
	}()
	// Give the mirror goroutine a moment to start before cancelling.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serveWith did not return after ctx cancel")
	}
	if !r.stopCalled {
		t.Error("stop function was not called on ctx-cancel teardown")
	}
}

// TestServeRejectsEmptyConfigFields exercises the validate
// branch of Serve — same shape as the Hello equivalent.
func TestServeRejectsEmptyConfigFields(t *testing.T) {
	err := Serve(t.Context(), &bytes.Buffer{},
		Config{Kernel: "k", Initramfs: "i"})
	if err == nil {
		t.Fatal("Serve with empty CloudHypervisor returned nil")
	}
}

// TestServePropagatesRunnerStartError pins that a runner
// start-failure surfaces as Serve's return error.
func TestServePropagatesRunnerStartError(t *testing.T) {
	want := errors.New("synthetic start failure")
	r := &fakeRunner{startErr: want}
	err := serveWith(t.Context(), &bytes.Buffer{}, validCfg(), r, noShareSidecar{})
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}

// TestServePropagatesShareStartError pins the startShares branch
// in serveWith — a virtiofsd start-failure surfaces verbatim
// before runVMM is touched.
func TestServePropagatesShareStartError(t *testing.T) {
	want := errors.New("synthetic share-start failure")
	r := &fakeRunner{}
	sc := &fakeShareSidecar{startErr: want}
	cfg := validCfg()
	cfg.Virtiofsd = "/nonexistent/virtiofsd"
	cfg.Shares = []*ShareConfig{{HostDir: "/", Tag: "t", Mountpoint: "/m"}}
	err := serveWith(t.Context(), &bytes.Buffer{}, cfg, r, sc)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want wrap of %v", err, want)
	}
}
