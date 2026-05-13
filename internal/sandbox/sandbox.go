// Package sandbox boots and supervises sandbox VMs by driving the
// configured VMM (Cloud Hypervisor in v1) and ferrying their console
// output back to the caller. The host-side `daft sandbox` commands in
// internal/cli call into this package.
//
// The public surface is deliberately VMM-agnostic so we can swap the
// backend behind the same interface (Firecracker, QEMU q35) — see PLAN.md
// "MicroVM stack".
package sandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/richiejp/darkaifactory/internal/agent"
	"github.com/richiejp/darkaifactory/internal/assert"
	"github.com/richiejp/darkaifactory/internal/oops"
	"github.com/richiejp/darkaifactory/internal/trace"
)

// HelloTimeout caps how long we wait for the in-VM agent to print its
// banner. The full boot path on a warm host should take well under a
// second; the generous default leaves headroom for first-run kernel
// decompression and sandbox-host load.
const HelloTimeout = 30 * time.Second

// Config selects the VMM and the artifacts it boots. All paths are
// absolute on the sandbox-host. The struct is exported so tests and
// callers can build one without going through the env.
type Config struct {
	// CloudHypervisor is the path to the cloud-hypervisor binary.
	CloudHypervisor string
	// Kernel is the path to the bzImage to boot.
	Kernel string
	// Initramfs is the path to the cpio archive to load as initrd.
	Initramfs string
	// Memory is the guest RAM size, e.g. "256M". Defaults to 256M when
	// empty so simple smoke tests don't need to think about it. When
	// Shares is non-empty the value is silently upgraded to include
	// shared=on, which virtio-fs requires for guest mappings.
	Memory string
	// CPUs is the number of vCPUs to assign. Defaults to 1 when zero.
	CPUs int
	// Virtiofsd is the path to the virtiofsd binary. Required when
	// Shares is non-empty, ignored otherwise.
	Virtiofsd string
	// Shares lists host directories to expose to the guest via
	// virtio-fs. One virtiofsd is spawned per entry; the agent
	// mounts each one at its Mountpoint inside the VM.
	//
	// Order is meaningful only for the v0 marker-demo: when Bundle
	// is empty, the agent reads hello.txt from Shares[0].Mountpoint
	// and reports the contents on the console. With Bundle set,
	// every share is mount-only.
	Shares []*ShareConfig
	// Bundle is the *in-guest* absolute path of an OCI bundle the
	// agent should crun-run. Usually a sub-path under one of the
	// Shares' Mountpoint so the host can ship the bundle into the
	// VM via virtiofs. Empty (the default) means no container is
	// run.
	Bundle string

	// Vsock, when non-nil, exposes a Cloud Hypervisor hybrid-vsock
	// device to the guest so the in-VM agent can stream data back
	// to the sandbox-host without going through the serial console.
	// Currently used for trace span shipping; future channels (LSP
	// stdio, container exec, control RPC) will reuse the same UDS.
	Vsock *VsockConfig
}

// VsockConfig describes a Cloud Hypervisor hybrid-vsock device.
// The guest sees it as a normal virtio-vsock; the host side is a
// Unix-domain socket the VMM proxies through. Per-port host
// listeners bind to <HostSocket>_<port> (firecracker-style hybrid
// vsock). Reused: the same UDS path serves every guest→host port.
type VsockConfig struct {
	// HostSocket is the absolute UDS path Cloud Hypervisor opens.
	// Per-port listeners append _<port> to it (e.g. /tmp/v.sock_8000).
	HostSocket string
	// GuestCID is the AF_VSOCK context ID assigned to the guest.
	// Must be ≥ 3 (0/1/2 are reserved by the AF_VSOCK ABI).
	GuestCID uint32
	// TracePort, if non-zero, instructs the in-VM agent (via the
	// kernel cmdline) to ship its span buffer to this AF_VSOCK
	// port on the host. The sandbox-host arranges a listener at
	// <HostSocket>_<TracePort> before booting the VM.
	TracePort uint32
}

// ShareConfig describes one host→guest virtiofs mount.
type ShareConfig struct {
	// HostDir is the absolute path on the sandbox-host that virtiofsd
	// will export. Must exist and be a directory.
	HostDir string
	// Tag is the virtio-fs device tag the guest mounts by name.
	// Must be unique across the Shares slice.
	Tag string
	// Mountpoint is the absolute path inside the guest where the agent
	// mounts the share. Must start with `/`.
	Mountpoint string
	// Overlay, when true, wraps the share in a tmpfs-backed
	// overlayfs inside the VM. The share is mounted at
	// `<Mountpoint>.lower`; the container sees a writable view at
	// `<Mountpoint>`. Container writes go to an in-VM tmpfs and
	// vanish with the VM. Useful when the underlying share is
	// either daft-fuse (read-only by construction) or a host dir
	// we don't want the container mutating directly.
	Overlay bool
}

// validate returns an error listing every missing required field.
// Missing fields are user-actionable, so we check before doing anything
// that has side effects (process spawn, file I/O).
func (c *Config) validate() error {
	if c.CloudHypervisor == "" {
		return fmt.Errorf("sandbox: CloudHypervisor path is required")
	}
	if c.Kernel == "" {
		return fmt.Errorf("sandbox: Kernel path is required")
	}
	if c.Initramfs == "" {
		return fmt.Errorf("sandbox: Initramfs path is required")
	}
	if len(c.Shares) > 0 {
		if c.Virtiofsd == "" {
			return fmt.Errorf("sandbox: Virtiofsd path is required when Shares is non-empty")
		}
		seenTag := make(map[string]bool, len(c.Shares))
		for i, s := range c.Shares {
			if err := s.validate(); err != nil {
				return fmt.Errorf("sandbox: Shares[%d]: %w", i, err)
			}
			if seenTag[s.Tag] {
				return fmt.Errorf("sandbox: duplicate Share.Tag %q", s.Tag)
			}
			seenTag[s.Tag] = true
		}
	}
	if c.Bundle != "" && !strings.HasPrefix(c.Bundle, "/") {
		return fmt.Errorf("sandbox: Bundle %q must be an absolute in-guest path", c.Bundle)
	}
	if c.Vsock != nil {
		if err := c.Vsock.validate(); err != nil {
			return err
		}
	}
	return nil
}

// validate enforces the AF_VSOCK ABI's CID rules and our own path
// hygiene before the VMM is spawned.
func (v *VsockConfig) validate() error {
	if v.HostSocket == "" {
		return fmt.Errorf("sandbox: Vsock.HostSocket is required")
	}
	if !filepath.IsAbs(v.HostSocket) {
		return fmt.Errorf("sandbox: Vsock.HostSocket %q must be absolute", v.HostSocket)
	}
	if v.GuestCID < 3 {
		return fmt.Errorf("sandbox: Vsock.GuestCID %d must be >= 3 (0/1/2 reserved)", v.GuestCID)
	}
	return nil
}

// validate checks the Share-only invariants. HostDir existence is
// checked here because virtiofsd would otherwise fail with a less
// helpful error after we've started spawning processes.
func (s *ShareConfig) validate() error {
	if s.HostDir == "" {
		return fmt.Errorf("sandbox: Share.HostDir is required")
	}
	if !filepath.IsAbs(s.HostDir) {
		return fmt.Errorf("sandbox: Share.HostDir %q must be absolute", s.HostDir)
	}
	if s.Tag == "" {
		return fmt.Errorf("sandbox: Share.Tag is required")
	}
	if !strings.HasPrefix(s.Mountpoint, "/") {
		return fmt.Errorf("sandbox: Share.Mountpoint %q must be absolute", s.Mountpoint)
	}
	info, err := os.Stat(s.HostDir)
	if err != nil {
		return fmt.Errorf("sandbox: Share.HostDir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("sandbox: Share.HostDir %q is not a directory", s.HostDir)
	}
	return nil
}

// withDefaults returns a copy of c with empty optional fields filled in.
func (c Config) withDefaults() Config {
	if c.Memory == "" {
		c.Memory = defaultMemory
	}
	if c.CPUs == 0 {
		c.CPUs = 1
	}
	return c
}

// ErrBannerNotSeen is returned when the VMM exits or the timeout fires
// before the agent prints its banner on the console.
var ErrBannerNotSeen = errors.New("sandbox: agent banner not seen on console")

// runner abstracts the VMM subprocess so the orchestration code in Hello
// can be unit-tested with a fake. Real callers use cloudHypervisorRunner;
// tests inject a stub that returns a pre-canned stream.
//
// shareSockets carries the Unix-socket paths of every virtiofsd
// sidecar to wire up via `--fs tag=...,socket=...`, parallel to
// cfg.Shares. Empty (or nil) when no share is configured. Threading
// it as an explicit parameter (rather than smuggling it through
// Config) keeps Config a description-of-intent and the runner
// stateless.
type runner interface {
	run(ctx context.Context, cfg Config, shareSockets []string) (io.ReadCloser, func(), error)
}

// shareSidecar abstracts the virtiofsd subprocess. Lifecycle: start
// before the VMM (cloud-hypervisor connects to its socket(s) on
// boot), stop after the VMM exits. Returns one socket path per
// cfg.Shares entry, in the same order. Like runner, swappable in
// tests.
type shareSidecar interface {
	start(ctx context.Context, cfg Config) (socketPaths []string, stop func(), err error)
}

// cloudHypervisorRunner runs cloud-hypervisor as a child process and
// captures its console (hvc0) on stdout. It implements runner.
type cloudHypervisorRunner struct{}

// virtiofsdSidecar runs virtiofsd as a child process listening on a
// per-VM Unix socket inside ctx-scoped temp dir. It implements
// shareSidecar.
type virtiofsdSidecar struct{}

// kernelCmdlineBase is the static prefix of the boot cmdline we pass to
// every sandbox VM. console drives kernel logs and our agent's stdout
// to the host's controlling terminal; reboot=t/panic=-1 ensures the
// guest doesn't hang on a panic. helloWith appends `daft.share=...` on
// top when Share is configured.
const kernelCmdlineBase = "console=hvc0 reboot=t panic=-1"

// defaultMemory is the guest RAM size used when Config.Memory is empty.
const defaultMemory = "256M"

// virtiofsdReadyTimeout caps how long start waits for virtiofsd's
// socket to appear before giving up. Should be near-instant on a warm
// host; the generous default leaves room for cold-start.
const virtiofsdReadyTimeout = 5 * time.Second

// start spawns one virtiofsd per cfg.Shares entry, returning a slice
// of socket paths (parallel to cfg.Shares). The returned stop tears
// down every spawned process and removes the temp dirs.
func (virtiofsdSidecar) start(ctx context.Context, cfg Config) (sockets []string, stop func(), err error) {
	sockets = make([]string, 0, len(cfg.Shares))
	stops := make([]func(), 0, len(cfg.Shares))

	stopAll := func() {
		// Tear down in reverse spawn order, mostly cosmetic.
		for i := len(stops) - 1; i >= 0; i-- {
			stops[i]()
		}
	}

	for i, share := range cfg.Shares {
		sock, oneStop, oneErr := startOneVirtiofsd(ctx, cfg.Virtiofsd, share)
		if oneErr != nil {
			stopAll()
			return nil, nil, fmt.Errorf("sandbox: share %d (%s): %w", i, share.Tag, oneErr)
		}
		sockets = append(sockets, sock)
		stops = append(stops, oneStop)
	}
	return sockets, stopAll, nil
}

// startOneVirtiofsd spawns a single virtiofsd against share, writes
// its socket into a fresh temp dir, and waits for the socket to
// appear before returning. stop tears down the process + temp dir.
func startOneVirtiofsd(ctx context.Context, binary string, share *ShareConfig) (socket string, stop func(), err error) {
	dir, err := os.MkdirTemp("", "daft-virtiofs-")
	if err != nil {
		return "", nil, fmt.Errorf("tmpdir: %w", err)
	}
	cleanup := func() {
		// dir is a fresh os.MkdirTemp result, so a RemoveAll failure
		// is unexpected — log it via oops which exits on real
		// errors, swallows fs.ErrNotExist.
		oops.Skip("sandbox: remove virtiofsd tmpdir",
			os.RemoveAll(dir), fs.ErrNotExist)
	}

	socket = filepath.Join(dir, "vhost.sock")

	// --sandbox=none keeps virtiofsd from trying to chroot/setuid; we
	// run it as the same user that owns the share dir, so the
	// production hardening (PLAN.md "rootless agent") is layered on
	// at the orchestration boundary, not inside virtiofsd.
	//
	// #nosec G204 — `binary` is cfg.Virtiofsd, an absolute path
	// passed by the host operator who's already trusted to spawn
	// the VMM. There's no untrusted user input on the argv here.
	cmd := exec.CommandContext(
		ctx, binary,
		"--socket-path", socket,
		"--shared-dir", share.HostDir,
		"--sandbox=none",
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("spawn virtiofsd: %w", err)
	}

	stop = func() {
		// Best-effort teardown. Kill on an already-exited process
		// returns os.ErrProcessDone — fine, race between us and the
		// VM. Anything else (e.g. EPERM) is a real problem we can't
		// recover from, so let oops.Skip exit on it.
		if cmd.Process != nil {
			oops.Skip("kill virtiofsd", cmd.Process.Kill(), os.ErrProcessDone)
		}
		// Wait returns *exec.ExitError after we Kill'd the process —
		// that's expected and informational. Other error types
		// (e.g. ECHILD, runtime broken) signal a real problem.
		if waitErr := cmd.Wait(); waitErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(waitErr, &exitErr) {
				oops.Must("wait virtiofsd", waitErr)
			}
		}
		cleanup()
	}

	if err := waitForSocket(ctx, socket, virtiofsdReadyTimeout); err != nil {
		stop()
		return "", nil, err
	}
	return socket, stop, nil
}

// waitForSocket polls path until it exists as a socket node, ctx is
// cancelled, or timeout elapses. Returns a wrapped error on timeout
// for the operator's logs.
func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	done := ctx.Done()
	assert.True(done != nil,
		"waitForSocket requires a cancellable context (Background/TODO would only bound on the timeout, missing the operator-cancel signal)")
	deadline := time.Now().Add(timeout)
	//daft:lint:disable=forloopbound reason="exits when the socket appears, the deadline passes, or ctx.Done() fires; ctx cancellability asserted at entry"
	for {
		if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sandbox: virtiofsd socket %s did not appear within %s", path, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// buildKernelCmdline returns the cmdline string CH boots with. Emits
// one `daft.share=<tag>:<mp>` token per share plus `daft.bundle=
// <path>` when Bundle is set, so the in-VM agent picks up the mount
// + container-launch instructions.
func buildKernelCmdline(cfg Config) string {
	parts := []string{kernelCmdlineBase}
	for _, s := range cfg.Shares {
		parts = append(parts, agent.FormatShareCmdline(agent.ShareSpec{
			Tag:        s.Tag,
			Mountpoint: s.Mountpoint,
		}))
	}
	if cfg.Bundle != "" {
		parts = append(parts, agent.FormatBundleCmdline(cfg.Bundle))
	}
	if cfg.Vsock != nil && cfg.Vsock.TracePort != 0 {
		parts = append(parts, agent.FormatVsockTracePortCmdline(cfg.Vsock.TracePort))
	}
	for _, s := range cfg.Shares {
		if s.Overlay {
			parts = append(parts, agent.FormatWorkspaceOverlayCmdline(s.Mountpoint))
		}
	}
	return strings.Join(parts, " ")
}

// buildMemoryArg returns the value for `--memory size=...`. virtio-fs
// requires `shared=on` so the guest's mappings can resolve to host
// pages; we force it whenever any Share is configured so callers
// don't have to know the detail.
func buildMemoryArg(cfg Config) string {
	if len(cfg.Shares) == 0 {
		return "size=" + cfg.Memory
	}
	return "size=" + cfg.Memory + ",shared=on"
}

// shareSocketArg renders a single virtiofsd socket as cloud-
// hypervisor's `--fs tag=...,socket=...` argument value.
func shareSocketArg(tag, socket string) string {
	return fmt.Sprintf("tag=%s,socket=%s", tag, socket)
}

// buildCloudHypervisorArgs returns the argv cloudHypervisorRunner
// passes to the cloud-hypervisor binary. Pure function so the
// argv shape (especially the variadic --fs) is unit-testable
// without spawning a real VMM.
//
// Cloud Hypervisor's --fs flag is variadic: one --fs followed by
// N "tag=...,socket=..." tokens. Repeating the flag (--fs A --fs
// B) is rejected by clap with "cannot be used multiple times" —
// a real regression that landed before this test caught it.
func buildCloudHypervisorArgs(cfg Config, shareSockets []string) []string {
	// 14 = the always-present base args; +1 for the --fs flag
	// (when shares > 0) +N for the socket tokens. Pre-sized so the
	// inner loop's appends don't realloc.
	args := make([]string, 0, 14+1+len(shareSockets))
	args = append(
		args,
		"--kernel", cfg.Kernel,
		"--initramfs", cfg.Initramfs,
		"--cmdline", buildKernelCmdline(cfg),
		"--console", "tty",
		"--serial", "off",
		"--cpus", fmt.Sprintf("boot=%d", cfg.CPUs),
		"--memory", buildMemoryArg(cfg),
	)
	if len(shareSockets) > 0 {
		args = append(args, "--fs")
		for i, sock := range shareSockets {
			args = append(args, shareSocketArg(cfg.Shares[i].Tag, sock))
		}
	}
	if cfg.Vsock != nil {
		args = append(args, "--vsock",
			fmt.Sprintf("cid=%d,socket=%s", cfg.Vsock.GuestCID, cfg.Vsock.HostSocket))
	}
	return args
}

func (cloudHypervisorRunner) run(ctx context.Context, cfg Config, shareSockets []string) (io.ReadCloser, func(), error) {
	args := buildCloudHypervisorArgs(cfg, shareSockets)
	// #nosec G204 — cfg.CloudHypervisor is the operator-supplied
	// VMM path; argv elements are derived from validated Config
	// fields (Kernel/Initramfs/Memory) plus our own emitted
	// cmdline. No untrusted user input.
	cmd := exec.CommandContext(ctx, cfg.CloudHypervisor, args...)

	// Cloud Hypervisor sends both kernel logs and our agent's banner
	// over the virtio-console (hvc0 → its stdout) when --console tty
	// is set. Hijacking stdout lets us scan for the banner. Stderr is
	// discarded — CH's own logs are noisy and not what Hello tests.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("sandbox: stdout pipe: %w", err)
	}
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("sandbox: start cloud-hypervisor: %w", err)
	}

	stop := func() {
		if cmd.Process != nil {
			oops.Skip("kill cloud-hypervisor", cmd.Process.Kill(), os.ErrProcessDone)
		}
		// Drain stdout before Wait so the kernel buffer doesn't keep
		// the writer parked. Either Wait or this drain may have already
		// raced past EOF — io.Copy returns nil on EOF, and a closed
		// pipe surfaces as os.ErrClosed which is fine here.
		if _, copyErr := io.Copy(io.Discard, stdout); copyErr != nil {
			oops.Skip("drain cloud-hypervisor stdout", copyErr,
				io.EOF, os.ErrClosed)
		}
		if waitErr := cmd.Wait(); waitErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(waitErr, &exitErr) {
				oops.Must("wait cloud-hypervisor", waitErr)
			}
		}
	}
	return stdout, stop, nil
}

// Hello boots a minimal sandbox VM with the given Config, mirrors its
// console output to w, and returns nil once the agent prints its banner.
// The VM is killed before Hello returns regardless of outcome.
//
// This is the smoke test for the microVM stack — kernel decompresses,
// initramfs unpacks, /init (daft-agent) runs and prints, we see it.
// When Config.Share is set, virtiofsd is started alongside CH so the
// guest can mount the share.
func Hello(ctx context.Context, w io.Writer, cfg Config) error {
	return helloWith(ctx, w, cfg, cloudHypervisorRunner{}, virtiofsdSidecar{})
}

// helloWith is Hello parameterised on its subprocess drivers so tests
// can swap in fakes. Production code calls Hello, which threads the
// real cloudHypervisorRunner / virtiofsdSidecar.
func helloWith(ctx context.Context, w io.Writer, cfg Config, r runner, sc shareSidecar) error {
	ctx, span := trace.Start(ctx, "sandbox.Hello",
		trace.WithAttributes(
			trace.KV{Key: "kernel", Value: cfg.Kernel},
			trace.KV{Key: "shares", Value: strconv.Itoa(len(cfg.Shares))},
			trace.KV{Key: "bundle", Value: cfg.Bundle},
		))
	defer span.End()

	if err := cfg.validate(); err != nil {
		return trace.RecordError(ctx, err)
	}
	cfg = cfg.withDefaults()

	runCtx, cancel := context.WithTimeout(ctx, HelloTimeout)
	defer cancel()

	shareSockets, stopShare, err := startShares(runCtx, cfg, sc)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	defer stopShare()

	stopVsock, err := startVsockTraceReceiver(ctx, cfg)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	defer stopVsock()

	stdout, stop, err := runVMM(runCtx, cfg, shareSockets, r)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	defer stop()

	bannerCtx, bannerSpan := trace.Start(runCtx, "sandbox.scan_for_banner")
	defer bannerSpan.End()
	if err := scanForBanner(w, stdout); err != nil {
		return trace.RecordError(bannerCtx, err)
	}
	return nil
}

// startShares spawns virtiofsd for every share in cfg. Returns a
// no-op stopShare and empty sockets when cfg has no shares so the
// caller can defer stopShare unconditionally. Wrapped in its own
// span so the cold-boot trace breaks down virtiofsd-spawn time
// separately from VMM-spawn time.
func startShares(ctx context.Context, cfg Config, sc shareSidecar) (sockets []string, stop func(), err error) {
	if len(cfg.Shares) == 0 {
		return nil, func() {}, nil
	}
	ctx, span := trace.Start(ctx, "sandbox.start_virtiofsd",
		trace.WithAttributes(trace.KV{Key: "shares", Value: strconv.Itoa(len(cfg.Shares))}))
	defer span.End()
	sockets, stop, err = sc.start(ctx, cfg)
	if err != nil {
		return nil, func() {}, trace.RecordError(ctx,
			fmt.Errorf("sandbox: start virtiofsd: %w", err))
	}
	return sockets, stop, nil
}

// runVMM spawns the configured VMM (Cloud Hypervisor today) and
// returns its stdout pipe. Wrapped in its own span so the trace
// distinguishes "VMM spawn" from "wait for guest banner".
func runVMM(ctx context.Context, cfg Config, shareSockets []string, r runner) (stdout io.ReadCloser, stop func(), err error) {
	ctx, span := trace.Start(ctx, "sandbox.run_cloud_hypervisor")
	defer span.End()
	stdout, stop, err = r.run(ctx, cfg, shareSockets)
	if err != nil {
		return nil, nil, trace.RecordError(ctx, err)
	}
	return stdout, stop, nil
}

// scanForBanner mirrors r line-by-line to w and returns nil the moment
// a line containing agent.Banner appears. Returns ErrBannerNotSeen on
// EOF, or a wrapped scanner error on read failure.
//
// Buffer is sized for kernel boot lines, which can be long.
func scanForBanner(w io.Writer, r io.Reader) error {
	scanner := bufio.NewScanner(r)
	// 64 KiB initial, 256 KiB cap. Real kernel boot lines are <1 KiB;
	// the cap is just a hostile-input ceiling.
	scanner.Buffer(make([]byte, 64*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if _, werr := fmt.Fprintln(w, line); werr != nil {
			// The caller's writer closed (e.g. user Ctrl-C'd).
			// Returning the wrapped error stops the scan and
			// surfaces the failure up the stack.
			return fmt.Errorf("sandbox: mirror console: %w", werr)
		}
		if strings.Contains(line, agent.Banner) {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("sandbox: read console: %w", err)
	}
	return ErrBannerNotSeen
}
