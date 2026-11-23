//go:build linux && integration

// Integration tests + benchmarks live behind the `integration` build
// tag. They require Linux + /dev/kvm + all sandbox-host artefacts on
// disk (cloud-hypervisor, guest kernel, initramfs, virtiofsd, and
// the Nix-built OCI bundle). None of these are optional: if any are
// missing, the resolver t.Fatalfs. There are no skips. Linux+KVM
// is a hard prerequisite for this codebase — CI environments
// without KVM are useless and don't gate commits.
//
// Run them via `make integration`, which builds the artefacts via
// Nix and points the resolver at `./result-*` symlinks at the repo
// root.

package sandbox

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/richiejp/vmjail/internal/agent"
	"github.com/richiejp/vmjail/internal/observability/progress"
)

// testTagVmjailshare is the virtio-fs tag the integration tests use
// when wiring a single share into the VM. Lives here because only
// the tagged integration tests reference it; unit tests pick their
// own tag names per-test.
const testTagVmjailshare = "vmjailshare"

// resolveArtifacts builds a sandbox.Config from a) VMJAIL_* env vars
// when set, falling back to b) ./result-* symlinks at the repo
// root (the default Nix output convention). It is intentionally
// strict: every prerequisite for running the sandbox — the
// artefacts AND `/dev/kvm` — must be present, otherwise the test
// fails loudly. There are no legitimate skips. If you can't run
// these tests, you can't run the thing the tests are testing, and
// you have no business merging changes to it.
func resolveArtifacts(t testing.TB) Config {
	t.Helper()

	repoRoot := repoRootFromTest(t)

	cfg := Config{
		CloudHypervisor: artefactPath(t, EnvCloudHypervisor,
			filepath.Join(repoRoot, "result-ch", "bin", "cloud-hypervisor")),
		Kernel: artefactPath(t, EnvKernel,
			filepath.Join(repoRoot, "result-kernel", "vmlinux")),
		Initramfs: artefactPath(t, EnvInitramfs,
			filepath.Join(repoRoot, "result-initramfs")),
		Virtiofsd: artefactPath(t, EnvVirtiofsd,
			filepath.Join(repoRoot, "result-virtiofsd", "bin", "virtiofsd")),
	}

	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Fatalf(
			"sandbox integration: /dev/kvm unavailable (%v). Linux+KVM is "+
				"a hard prerequisite for this project; develop on a host that "+
				"can actually run a sandbox.", err,
		)
	}
	// Vsock is always-on: every real-VM boot gets a hybrid-vsock device so
	// the agent fetches its config (shares, bundle, observe paths) and
	// streams its BOOT_COMPLETE event over the control channel rather than
	// the kernel cmdline. Tests that need specific protocol ports
	// (trace/attach) override cfg.Vsock wholesale; the rest inherit this.
	cfg.Vsock = &VsockConfig{
		HostSocket: filepath.Join(t.TempDir(), "control.sock"),
		GuestCID:   3,
	}
	return cfg
}

// resolveBundle locates the Nix-built OCI bundle the container
// integration tests use. Same env-var-then-symlink resolution as
// resolveArtifacts.
func resolveBundle(t testing.TB) string {
	t.Helper()
	return artefactPath(t, EnvBundle,
		filepath.Join(repoRootFromTest(t), "result-bundle"))
}

// artefactPath returns the env var's value when set, otherwise
// fallback if it exists on disk, otherwise t.Fatalfs with a clear
// instruction.
func artefactPath(t testing.TB, envKey, fallback string) string {
	t.Helper()
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	if _, err := os.Stat(fallback); err == nil {
		return fallback
	}
	t.Fatalf(
		"sandbox integration: artefact missing — set %s or run `make integration` "+
			"to nix-build it (looked for %s)", envKey, fallback,
	)
	return ""
}

// repoRootFromTest returns the absolute path to the repo root. Tests
// run with cwd = the package dir; repo root is two levels up from
// internal/<pkg>/.
func repoRootFromTest(t testing.TB) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root resolution failed at %s: %v", root, err)
	}
	return root
}

// copyTree wraps os.CopyFS with one extra step: it resolves symlinks
// at the source root before recursing. Nix outputs are symlinks into
// /nix/store, and os.CopyFS / os.DirFS won't follow a symlinked root.
func copyTree(src, dst string) error {
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		return err
	}
	return os.CopyFS(dst, os.DirFS(resolved))
}

// TestHelloIntegrationBootsVM is the no-share end-to-end smoke test:
// spawn the VMM, boot the kernel, exec the agent, scan the banner.
func TestHelloIntegrationBootsVM(t *testing.T) {
	cfg := resolveArtifacts(t)

	var buf bytes.Buffer
	err := Hello(traceCtx(t), &buf, cfg)
	if err != nil {
		t.Fatalf("Hello: %v\n--- console ---\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), agent.Banner) {
		t.Fatalf("console output missing banner %q\n--- console ---\n%s", agent.Banner, buf.String())
	}
}

// TestHelloIntegrationBootsVMWithShare is the e2e proof of file
// sharing: the host writes a marker file, the agent in the VM mounts
// virtio-fs, reads it, and reports the contents on the console.
func TestHelloIntegrationBootsVMWithShare(t *testing.T) {
	cfg := resolveArtifacts(t)

	dir := t.TempDir()
	const payload = "share-payload-from-integration-test"
	if err := os.WriteFile(filepath.Join(dir, agent.ShareMarkerFile), []byte(payload), 0o644); err != nil {
		t.Fatalf("write marker file: %v", err)
	}

	cfg.Shares = []*ShareConfig{{
		HostDir:    dir,
		Tag:        testTagVmjailshare,
		Mountpoint: testMountpoint,
	}}

	var buf bytes.Buffer
	if err := Hello(traceCtx(t), &buf, cfg); err != nil {
		t.Fatalf("Hello: %v\n--- console ---\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), agent.Banner) {
		t.Fatalf("banner missing from output:\n%s", buf.String())
	}
	wantLine := agent.ShareReportPrefix + payload
	if !strings.Contains(buf.String(), wantLine) {
		t.Fatalf("share line %q missing; full console output:\n%s", wantLine, buf.String())
	}
}

// TestHelloIntegrationBootsVMWithContainer is the e2e proof of the
// container slice: the host shares a bundle dir over virtiofs, delivers
// the bundle path over the vsock control channel, the agent mounts the
// share and execs crun, and the host scanner sees the container's stdout
// (with the container: prefix) before the agent banner.
func TestHelloIntegrationBootsVMWithContainer(t *testing.T) {
	cfg := resolveArtifacts(t)
	bundleSrc := resolveBundle(t)

	shareDir := t.TempDir()
	bundleInShare := filepath.Join(shareDir, "bundle")
	if err := copyTree(bundleSrc, bundleInShare); err != nil {
		t.Fatalf("copy bundle into share: %v", err)
	}

	cfg.Shares = []*ShareConfig{{
		HostDir:    shareDir,
		Tag:        testTagVmjailshare,
		Mountpoint: testMountpoint,
	}}
	cfg.Bundle = testBundlePath

	var buf bytes.Buffer
	if err := Hello(traceCtx(t), &buf, cfg); err != nil {
		t.Fatalf("Hello: %v\n--- console ---\n%s", err, buf.String())
	}
	wantLine := agent.ContainerReportPrefix + agent.ContainerBanner
	if !strings.Contains(buf.String(), wantLine) {
		t.Fatalf("container line %q missing; full console:\n%s", wantLine, buf.String())
	}
}

// linePresent reports whether want is an exact line in the newline-separated
// text s — so a check for "read" is not satisfied by "readv"/"readahead".
func linePresent(s, want string) bool {
	for _, l := range strings.Split(s, "\n") {
		if l == want {
			return true
		}
	}
	return false
}

// TestHelloIntegrationOpensnoopCapturesOpenat is the e2e proof of the
// Tier-2 open-snoop eBPF gadget: boot a container VM with
// vmjail.observe.opensnoop pointed at a path on a writable virtiofs share,
// let crun + the workload do their openat(2) storm, then read the trace
// back from the host side of the share and assert the gadget loaded,
// attached, and decoded real events.
//
// virtiofsd runs with its default cache mode (no writeback), so guest
// writes reach the host file as they happen — no graceful guest flush is
// needed, which matters because the host tears the VM down abruptly once
// it sees the banner.
func TestHelloIntegrationOpensnoopCapturesOpenat(t *testing.T) {
	cfg := resolveArtifacts(t)
	bundleSrc := resolveBundle(t)

	shareDir := t.TempDir()
	bundleInShare := filepath.Join(shareDir, "bundle")
	if err := copyTree(bundleSrc, bundleInShare); err != nil {
		t.Fatalf("copy bundle into share: %v", err)
	}

	cfg.Shares = []*ShareConfig{{
		HostDir:    shareDir,
		Tag:        testTagVmjailshare,
		Mountpoint: testMountpoint,
	}}
	cfg.Bundle = testBundlePath
	// Guest path on the writable share; host reads it at <shareDir>/<base>.
	const guestLogName = "opensnoop.log"
	cfg.OpensnoopLogPath = testMountpoint + "/" + guestLogName
	hostLog := filepath.Join(shareDir, guestLogName)
	// The exec-snoop gadget (#7a) writes its execve trace alongside.
	const guestExecLogName = "execsnoop.log"
	cfg.ExecsnoopLogPath = testMountpoint + "/" + guestExecLogName
	hostExecLog := filepath.Join(shareDir, guestExecLogName)
	// The sleep-snoop gadget (#7b) writes its nanosleep trace alongside.
	const guestSleepLogName = "sleepsnoop.log"
	cfg.SleepsnoopLogPath = testMountpoint + "/" + guestSleepLogName
	hostSleepLog := filepath.Join(shareDir, guestSleepLogName)
	// The io-snoop gadget (#7c) writes its per-fd I/O trace alongside.
	const guestIoLogName = "iosnoop.log"
	cfg.IosnoopLogPath = testMountpoint + "/" + guestIoLogName
	hostIoLog := filepath.Join(shareDir, guestIoLogName)
	// The boot-time syscall dump writes the guest's available-syscall list.
	const guestSyscallsName = "syscalls.txt"
	cfg.SyscallListPath = testMountpoint + "/" + guestSyscallsName
	hostSyscalls := filepath.Join(shareDir, guestSyscallsName)
	// The build-progress summary (#6b) writes alongside the raw trace.
	const guestSummaryName = "progress.json"
	cfg.ObserveSummaryPath = testMountpoint + "/" + guestSummaryName
	hostSummary := filepath.Join(shareDir, guestSummaryName)

	// Surface the captured traces + summary on failure, alongside the Tier-1 dump.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		if data, err := os.ReadFile(hostLog); err == nil {
			t.Logf("opensnoop trace (%s):\n%s", hostLog, data)
		}
		if data, err := os.ReadFile(hostExecLog); err == nil {
			t.Logf("execsnoop trace (%s):\n%s", hostExecLog, data)
		}
		if data, err := os.ReadFile(hostSleepLog); err == nil {
			t.Logf("sleepsnoop trace (%s):\n%s", hostSleepLog, data)
		}
		if data, err := os.ReadFile(hostIoLog); err == nil {
			t.Logf("iosnoop trace (%s):\n%s", hostIoLog, data)
		}
		if data, err := os.ReadFile(hostSyscalls); err == nil {
			t.Logf("syscall list (%s):\n%s", hostSyscalls, data)
		}
		if data, err := os.ReadFile(hostSummary); err == nil {
			t.Logf("build-progress summary (%s):\n%s", hostSummary, data)
		}
	})

	var buf bytes.Buffer
	if err := Hello(traceCtx(t), &buf, cfg); err != nil {
		t.Fatalf("Hello: %v\n--- console ---\n%s", err, buf.String())
	}
	if !strings.Contains(buf.String(), agent.OpensnoopReportPrefix) {
		t.Fatalf("agent never reported starting the gadget; console:\n%s", buf.String())
	}

	data, err := os.ReadFile(hostLog)
	if err != nil {
		t.Fatalf("read opensnoop trace %s: %v\n--- console ---\n%s", hostLog, err, buf.String())
	}
	// Event.String() renders `... comm=%q ... path=%q`; both substrings
	// present proves at least one openat record was decoded and written.
	if !strings.Contains(string(data), "comm=") || !strings.Contains(string(data), "path=\"/") {
		t.Fatalf("opensnoop trace has no decoded openat event (%d bytes)\n--- trace ---\n%s\n--- console ---\n%s",
			len(data), data, buf.String())
	}
	// The PID-namespace filter keeps only the container workload: the
	// agent runs /hello (comm "hello") in a child PID namespace, so its
	// openat (the Go runtime's /sys reads, the /workspace probe) must be
	// captured, while the agent's own opens (comm "vmjail-agent") must not.
	// Both checks together prove the filter is on and selective, not just
	// that *something* was traced.
	if !strings.Contains(string(data), `comm="hello"`) {
		t.Fatalf("opensnoop trace missing the container workload (comm=\"hello\"); filter may have dropped everything\n--- trace ---\n%s\n--- console ---\n%s",
			data, buf.String())
	}
	if strings.Contains(string(data), `comm="vmjail-agent"`) {
		t.Fatalf("opensnoop trace contains agent opens (comm=\"vmjail-agent\"); the container-workload filter is not excluding the agent's namespace\n--- trace ---\n%s",
			data)
	}
	if !strings.Contains(buf.String(), agent.OpensnoopReportPrefix+"tracing container openat") {
		t.Fatalf("agent did not report the container-scoped filter; console:\n%s", buf.String())
	}
	// The sys_exit hook captured the return value: the workload probes
	// /workspace/in.txt (absent in this e2e — no /workspace share), so that
	// openat returns ENOENT, which the gadget renders as ret=ENOENT. This
	// proves enter→exit correlation and errno decode end to end.
	if !strings.Contains(string(data), "ret=ENOENT") {
		t.Fatalf("opensnoop trace has no ENOENT return value; the sys_exit hook or errno decode is broken\n--- trace ---\n%s",
			data)
	}

	// The agent started the container-scoped exec gadget (deterministic
	// console line). The container execs /hello exactly once, very early, so
	// whether that single record flushes to the host before the abrupt
	// teardown is racy — unlike open-snoop's continuous openat storm. So the
	// trace content is asserted only when it did flush (catching an argv /
	// errno decode regression without flaking on the race); the BPF
	// load/attach/argv-decode is otherwise covered by the unit tests and a
	// privileged bpftool load. When present it must be the workload's exec
	// (argv=[/hello] ret=ok) — the agent's own crun exec runs in the root PID
	// namespace and is filtered out.
	if !strings.Contains(buf.String(), agent.ExecsnoopReportPrefix+"tracing container execve") {
		t.Fatalf("agent did not report the container-scoped exec filter; console:\n%s", buf.String())
	}
	if execData, err := os.ReadFile(hostExecLog); err == nil && len(execData) > 0 {
		if !strings.Contains(string(execData), "argv=[/hello") || !strings.Contains(string(execData), "ret=ok") {
			t.Errorf("execsnoop trace flushed but is not the workload's successful exec\n--- trace ---\n%s", execData)
		} else {
			t.Logf("execsnoop captured: %s", bytes.TrimSpace(execData))
		}
	}

	// The agent started the container-scoped sleep gadget (deterministic
	// console line). Whether the trivial /hello workload calls nanosleep at all
	// is workload-dependent, so the raw trace may legitimately be empty — only
	// the console report and the summary section (below) are asserted
	// deterministically. Any flushed line must be a well-formed sleep record.
	if !strings.Contains(buf.String(), agent.SleepsnoopReportPrefix+"tracing container nanosleep") {
		t.Fatalf("agent did not report the container-scoped sleep filter; console:\n%s", buf.String())
	}
	if sleepData, err := os.ReadFile(hostSleepLog); err == nil && len(sleepData) > 0 {
		if !strings.Contains(string(sleepData), "comm=") || !strings.Contains(string(sleepData), "dur=") {
			t.Errorf("sleepsnoop trace flushed but is not a well-formed sleep record\n--- trace ---\n%s", sleepData)
		} else {
			t.Logf("sleepsnoop captured: %s", bytes.TrimSpace(sleepData))
		}
	}

	// The agent started the container-scoped io gadget (deterministic console
	// line). io-snoop aggregates per-fd in the kernel and the Go drain ticks
	// ~1 Hz, so whether a per-fd snapshot has flushed before the abrupt teardown
	// is racy (like sleep-snoop) — only the console report and the summary
	// section (below) are asserted deterministically. When the trace did flush
	// its header lists the attached syscalls (read/write are always present) and
	// any per-fd line is well-formed.
	if !strings.Contains(buf.String(), agent.IosnoopReportPrefix+"tracing container fd I/O") {
		t.Fatalf("agent did not report the container-scoped io filter; console:\n%s", buf.String())
	}
	if ioData, err := os.ReadFile(hostIoLog); err == nil && len(ioData) > 0 {
		if !strings.Contains(string(ioData), "# iosnoop attached=[") ||
			!strings.Contains(string(ioData), "read") || !strings.Contains(string(ioData), "write") {
			t.Errorf("iosnoop trace flushed but its header does not list the core I/O syscalls\n--- trace ---\n%s", ioData)
		} else {
			t.Logf("iosnoop captured:\n%s", bytes.TrimSpace(ioData))
		}
	}

	// The boot-time syscall dump is written synchronously at boot (not
	// event-driven), so unlike the gadget traces it's reliably present before
	// teardown — assert it strictly. It must enumerate the core fd-I/O syscalls
	// any Linux guest exposes. The full list is logged so the actual
	// in-microvm syscall set is visible in the test output.
	if !strings.Contains(buf.String(), agent.SyscallsReportPrefix+"wrote ") {
		t.Fatalf("agent did not report writing the syscall list; console:\n%s", buf.String())
	}
	scData, err := os.ReadFile(hostSyscalls)
	if err != nil {
		t.Fatalf("read syscall list %s: %v\n--- console ---\n%s", hostSyscalls, err, buf.String())
	}
	syscallSet := string(scData)
	for _, want := range []string{"read", "write", "openat", "execve"} {
		if !linePresent(syscallSet, want) {
			t.Errorf("syscall list missing %q\n--- list ---\n%s", want, syscallSet)
		}
	}
	t.Logf("microvm exposes %d syscall tracepoints:\n%s",
		strings.Count(strings.TrimSpace(syscallSet), "\n")+1, syscallSet)

	// The summary writer ran the whole guest→host pipeline: cmdline token →
	// agent parse → writer → atomic rewrite onto the share. Assert the host
	// reads back a valid, parseable summary with an open-snoop section. The
	// exact Total depends on tick timing vs the abrupt VM teardown (the
	// writer fed by the same aggregator is unit-tested), so we assert the
	// wire contract held, not a specific count.
	if !strings.Contains(buf.String(), agent.SummaryReportPrefix) {
		t.Fatalf("agent never reported starting the summary writer; console:\n%s", buf.String())
	}
	sumData, err := os.ReadFile(hostSummary)
	if err != nil {
		t.Fatalf("read build-progress summary %s: %v\n--- console ---\n%s", hostSummary, err, buf.String())
	}
	summary, err := progress.Parse(sumData)
	if err != nil {
		t.Fatalf("parse build-progress summary: %v\n--- summary ---\n%s", err, sumData)
	}
	if summary.Opensnoop == nil || summary.TakenAtUnixMs <= 0 {
		t.Fatalf("summary missing opensnoop section or timestamp: %+v\n--- summary ---\n%s", summary, sumData)
	}
	// Both gadgets fold into the one summary, so the exec-snoop section must
	// be present. Like the open-snoop section above we assert presence, not a
	// specific Total: the workload execs /hello exactly once, very early, so
	// whether that single event lands in the summary snapshot that reaches
	// the host before the abrupt teardown is timing-dependent — the raw exec
	// trace (asserted above) is the deterministic proof the gadget captured
	// it.
	if summary.ExecSnoop == nil {
		t.Fatalf("summary missing exec-snoop section: %+v\n--- summary ---\n%s", summary, sumData)
	}
	// All three gadgets fold into the one summary, so the sleep-snoop section
	// must be present too — its Total is timing/workload-dependent (asserted as
	// presence, like exec-snoop).
	if summary.SleepSnoop == nil {
		t.Fatalf("summary missing sleep-snoop section: %+v\n--- summary ---\n%s", summary, sumData)
	}
	// All four gadgets fold into the one summary, so the io-snoop section must
	// be present too — its Total/bytes are timing/workload-dependent (asserted
	// as presence, like exec-snoop and sleep-snoop).
	if summary.IOSnoop == nil {
		t.Fatalf("summary missing io-snoop section: %+v\n--- summary ---\n%s", summary, sumData)
	}
	t.Logf("build-progress summary: opens total=%d rate=%.1f/s | execs total=%d top_commands=%v | sleeps total=%d total_sleep_ms=%d | io calls=%d r=%d w=%d",
		summary.Opensnoop.Total, summary.Opensnoop.OpenatRate,
		summary.ExecSnoop.Total, summary.ExecSnoop.TopCommands,
		summary.SleepSnoop.Total, summary.SleepSnoop.TotalSleepMs,
		summary.IOSnoop.Total, summary.IOSnoop.ReadBytes, summary.IOSnoop.WriteBytes)
}

// TestHelloIntegrationBootsVMWithImagestoreBundle proves the
// `vmjail-sandboxd run`-style flow without touching the CLI: pull a
// real image into a real on-disk imagestore, ask the store to
// build a bundle, share the bundle dir into the VM, and run it.
//
// The image comes from an in-process registry (same trick the
// imagestore unit tests use), so no network is involved. This is
// the missing e2e check that asserts the full pull → bundle → boot
// → run pipeline holds together end-to-end.
//
// Lives here (not in internal/sandboxdcli) because the imagestore
// dep would create an import cycle through sandboxdcli, and this
// is the only place that boots a real VM.
func TestHelloIntegrationBootsVMWithImagestoreBundle(t *testing.T) {
	cfg := resolveArtifacts(t)
	host, ref := pushHelloIntegrationImage(t)
	_ = host

	store := newImagestore(t)
	pulled, err := storePull(t, store, ref)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	bundle, err := storeBundle(t, store, pulled.Digest)
	if err != nil {
		t.Fatalf("Bundle: %v", err)
	}

	cfg.Shares = []*ShareConfig{{
		HostDir:    filepath.Dir(bundle.Path),
		Tag:        "bundles",
		Mountpoint: "/bundles",
	}}
	cfg.Bundle = filepath.Join("/bundles", filepath.Base(bundle.Path))

	var buf bytes.Buffer
	if err := Hello(traceCtx(t), &buf, cfg); err != nil {
		t.Fatalf("Hello: %v\n--- console ---\n%s", err, buf.String())
	}
	wantLine := agent.ContainerReportPrefix + agent.ContainerBanner
	if !strings.Contains(buf.String(), wantLine) {
		t.Fatalf("container line %q missing; full console:\n%s", wantLine, buf.String())
	}
}

// BenchmarkHelloBoot times one full cold-boot of the sandbox VM
// through the same Hello() the host CLI calls.
func BenchmarkHelloBoot(b *testing.B) {
	cfg := resolveArtifacts(b)
	for b.Loop() {
		var buf bytes.Buffer
		if err := Hello(b.Context(), &buf, cfg); err != nil {
			b.Fatalf("Hello: %v\n--- console ---\n%s", err, buf.String())
		}
	}
}

// BenchmarkHelloBootWithShare captures the per-VM cost of the
// virtiofsd sidecar + the agent mounting+reading the share.
func BenchmarkHelloBootWithShare(b *testing.B) {
	cfg := resolveArtifacts(b)

	dir := b.TempDir()
	if err := os.WriteFile(dir+"/"+agent.ShareMarkerFile, []byte("bench"), 0o644); err != nil {
		b.Fatalf("write marker: %v", err)
	}
	cfg.Shares = []*ShareConfig{{
		HostDir:    dir,
		Tag:        testTagVmjailshare,
		Mountpoint: testMountpoint,
	}}

	for b.Loop() {
		var buf bytes.Buffer
		if err := Hello(b.Context(), &buf, cfg); err != nil {
			b.Fatalf("Hello: %v\n--- console ---\n%s", err, buf.String())
		}
	}
}

// BenchmarkHelloBootWithContainer measures the full container slice:
// virtiofsd + kernel boot + share mount + crun --no-pivot + container
// exec to completion.
func BenchmarkHelloBootWithContainer(b *testing.B) {
	cfg := resolveArtifacts(b)
	bundleSrc := resolveBundle(b)

	dir := b.TempDir()
	if err := copyTree(bundleSrc, dir+"/bundle"); err != nil {
		b.Fatalf("copy bundle: %v", err)
	}
	cfg.Shares = []*ShareConfig{{
		HostDir:    dir,
		Tag:        testTagVmjailshare,
		Mountpoint: testMountpoint,
	}}
	cfg.Bundle = testBundlePath

	for b.Loop() {
		var buf bytes.Buffer
		if err := Hello(b.Context(), &buf, cfg); err != nil {
			b.Fatalf("Hello: %v\n--- console ---\n%s", err, buf.String())
		}
	}
}
