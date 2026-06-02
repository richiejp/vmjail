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

	"github.com/localai-org/darkaifactory/internal/agent"
	"github.com/localai-org/darkaifactory/internal/observability/progress"
)

// testTagDaftshare is the virtio-fs tag the integration tests use
// when wiring a single share into the VM. Lives here because only
// the tagged integration tests reference it; unit tests pick their
// own tag names per-test.
const testTagDaftshare = "daftshare"

// resolveArtifacts builds a sandbox.Config from a) DAFT_* env vars
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
			filepath.Join(repoRoot, "result-kernel", "bzImage")),
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
		Tag:        testTagDaftshare,
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
// container slice: the host shares a bundle dir over virtiofs, sets
// daft.bundle on the cmdline, the agent mounts the share and execs
// crun, and the host scanner sees the container's stdout (with the
// container: prefix) before the agent banner.
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
		Tag:        testTagDaftshare,
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

// TestHelloIntegrationOpensnoopCapturesOpenat is the e2e proof of the
// Tier-2 open-snoop eBPF gadget: boot a container VM with
// daft.observe.opensnoop pointed at a path on a writable virtiofs share,
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
		Tag:        testTagDaftshare,
		Mountpoint: testMountpoint,
	}}
	cfg.Bundle = testBundlePath
	// Guest path on the writable share; host reads it at <shareDir>/<base>.
	const guestLogName = "opensnoop.log"
	cfg.OpensnoopLogPath = testMountpoint + "/" + guestLogName
	hostLog := filepath.Join(shareDir, guestLogName)
	// The build-progress summary (#6b) writes alongside the raw trace.
	const guestSummaryName = "progress.json"
	cfg.ObserveSummaryPath = testMountpoint + "/" + guestSummaryName
	hostSummary := filepath.Join(shareDir, guestSummaryName)

	// Surface the captured trace + summary on failure, alongside the Tier-1 dump.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		if data, err := os.ReadFile(hostLog); err == nil {
			t.Logf("opensnoop trace (%s):\n%s", hostLog, data)
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
	// captured, while the agent's own opens (comm "daft-agent") must not.
	// Both checks together prove the filter is on and selective, not just
	// that *something* was traced.
	if !strings.Contains(string(data), `comm="hello"`) {
		t.Fatalf("opensnoop trace missing the container workload (comm=\"hello\"); filter may have dropped everything\n--- trace ---\n%s\n--- console ---\n%s",
			data, buf.String())
	}
	if strings.Contains(string(data), `comm="daft-agent"`) {
		t.Fatalf("opensnoop trace contains agent opens (comm=\"daft-agent\"); the container-workload filter is not excluding the agent's namespace\n--- trace ---\n%s",
			data)
	}
	if !strings.Contains(buf.String(), agent.OpensnoopReportPrefix+"tracing container openat") {
		t.Fatalf("agent did not report the container-scoped filter; console:\n%s", buf.String())
	}

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
	t.Logf("build-progress summary: total=%d rate=%.1f/s last-event-age=%dms top_dirs=%v",
		summary.Opensnoop.Total, summary.Opensnoop.OpenatRate,
		summary.Opensnoop.LastEventAgeMs, summary.Opensnoop.TopDirs)
}

// TestHelloIntegrationBootsVMWithImagestoreBundle proves the
// `daft-sandboxd run`-style flow without touching the CLI: pull a
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
		Tag:        testTagDaftshare,
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
		Tag:        testTagDaftshare,
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
