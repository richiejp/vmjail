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
	"hash/fnv"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/localai-org/darkaifactory/internal/agent"
	"github.com/localai-org/darkaifactory/internal/assert"
	"github.com/localai-org/darkaifactory/internal/observability"
	"github.com/localai-org/darkaifactory/internal/oops"
	"github.com/localai-org/darkaifactory/internal/trace"
)

// HelloTimeout caps how long we wait for the in-VM agent to print its
// banner. The full boot path on a warm host should take well under a
// second; the generous default leaves headroom for first-run kernel
// decompression and sandbox-host load.
const HelloTimeout = 30 * time.Second

// traceAttrShares is the trace-attribute key for the share count
// stamped on the Hello / Serve / startShares spans. Factored as a
// const so goconst doesn't flag the three nearby call sites.
const traceAttrShares = "shares"

// virtiofsd id-translation flag names (see translateArgs). Factored as
// consts so goconst doesn't flag the repeated literals.
const (
	flagTranslateUID = "--translate-uid"
	flagTranslateGID = "--translate-gid"
)

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

	// PortForwards declares the guest-side ports the in-VM agent
	// should listen on for orchestrator-driven ingress (Slice C1).
	// Each entry travels in the GuestConfig over the vsock control
	// channel; the agent vsock-listens on VsockPort and pumps accepted
	// connections to the container's `127.0.0.1:ContainerPort`.
	//
	// VsockPort must be unique across the slice and disjoint from any
	// enabled control-channel feature's fixed port (trace/attach/net),
	// otherwise CH rejects the duplicate guest-side bind.
	PortForwards []PortForward

	// DenyEgress switches the per-VM netstack to default-deny: the
	// guest may dial only the host loopback target (the egress
	// proxy), everything else is dropped + logged (Phase J1). Set by
	// the executor when an egress proxy is configured, so all
	// legitimate egress goes through the proxy lane.
	DenyEgress bool

	// ProxyHandler, when non-nil, is served in-stack on the per-VM
	// netstack's gateway:ProxyPort (Phase J6): the guest's https_proxy
	// points there and reaches the WAN only through this terminating
	// MITM/caching hop, which runs in sandboxd and dials upstream
	// itself. Set by the executor alongside DenyEgress when an egress
	// proxy is configured. nil leaves the netstack without an in-stack
	// proxy. Forwarded verbatim to sandboxnet.Config.
	ProxyHandler http.Handler
	// ProxyPort is the gateway TCP port ProxyHandler listens on; zero
	// lets sandboxnet pick its DefaultProxyPort.
	ProxyPort uint16

	// ObserverLogPath, when non-empty, enables per-VM host-side
	// telemetry: a goroutine samples the cloud-hypervisor process's
	// /proc/<pid>/{stat,status,io} at 1 Hz and writes one JSON line
	// per sample to this absolute path. Survives the VM dying so
	// post-mortem inspection still works. See
	// internal/observability and docs/observability.md for the data
	// schema and rotation behaviour. Empty disables the observer
	// (only for callers that have a reason — every sandbox-test
	// and user-facing daft-sandboxd launch should set this).
	ObserverLogPath string
	// ObserverMaxBytes caps the on-disk footprint at roughly this
	// many bytes (one current file + one rotated backup). Zero
	// uses observability.DefaultMaxBytes (50 MB); negative
	// disables rotation. Ignored when ObserverLogPath is empty.
	ObserverMaxBytes int64

	// OpensnoopLogPath, when non-empty, is the *in-guest* absolute path
	// the agent writes the Tier-2 open-snoop eBPF trace to (one line per
	// openat(2): pid/uid/comm/dirfd/flags/path). Set it to a path on a
	// writable virtiofs share so the host can read the trace after the
	// VM exits — symmetric to ObserverLogPath but guest-side, since the
	// gadget runs inside the VM under the guest's CAP_BPF. Empty (the
	// default) leaves the gadget off. See OBSERVABILITY.md "Tier 2".
	OpensnoopLogPath string

	// ExecsnoopLogPath, when non-empty, is the *in-guest* absolute path the
	// agent writes the Tier-2 exec-snoop eBPF trace to (one line per
	// execve(2): pid/ppid/uid/comm/argv/ret). Same contract as
	// OpensnoopLogPath. Empty (the default) leaves the gadget off.
	ExecsnoopLogPath string

	// SleepsnoopLogPath, when non-empty, is the *in-guest* absolute path the
	// agent writes the Tier-2 sleep-snoop eBPF trace to (one line per
	// nanosleep(2) / clock_nanosleep(2): pid/comm/requested-duration). Same
	// contract as OpensnoopLogPath. Empty (the default) leaves the gadget off.
	SleepsnoopLogPath string

	// IosnoopLogPath, when non-empty, is the *in-guest* absolute path the
	// agent writes the Tier-2 io-snoop eBPF trace to (per-fd byte/call counts
	// resolved to a target). Same contract as OpensnoopLogPath. Empty (the
	// default) leaves the gadget off.
	IosnoopLogPath string

	// SyscallListPath, when non-empty, is the *in-guest* absolute path the
	// agent writes the guest kernel's available-syscall list to at boot
	// (enumerated from tracefs; one syscall name per line, sorted). Not a
	// gadget — a one-shot boot diagnostic so a user can discover what is
	// traceable inside the microvm. Empty (the default) skips the dump.
	SyscallListPath string

	// ObserveSummaryPath, when non-empty, is the *in-guest* absolute path
	// the agent atomically rewrites the live build-progress summary to
	// (~1 Hz JSON: openat total/rate/last-activity-age + top processes,
	// paths, and dirs). Set it to a path on a writable virtiofs share so
	// the host-side `daft sandbox observe` can read it while the workload
	// runs. The summary is fed by the open-snoop gadget, so it only
	// produces output when OpensnoopLogPath is also set. Empty (the
	// default) leaves it off. See OBSERVABILITY.md roadmap #6.
	ObserveSummaryPath string
}

// PortForward declares one Slice C1 port-forward: traffic the
// sandbox-host (via DialGuestVsock + the CH hybrid-vsock CONNECT
// proxy) sends to VsockPort on the guest side gets relayed by the
// in-VM agent to 127.0.0.1:ContainerPort inside the container.
type PortForward struct {
	// VsockPort is the AF_VSOCK port the agent listens on inside
	// the VM. Must be non-zero.
	VsockPort uint32
	// ContainerPort is the TCP port the workload container is
	// already bound to on its loopback. Must be non-zero.
	ContainerPort uint32
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

	// Trace, Attach, and Net toggle the three optional control-channel
	// features. Each runs on a fixed AF_VSOCK port (a protocol constant:
	// agent.DefaultTraceVsockPort 8000 / DefaultAttachVsockPort 9000 /
	// DefaultNetVsockPort 9500), and the host arranges a listener at
	// <HostSocket>_<that constant>. The GuestConfig delivered over the
	// control channel carries the matching enable-bit so the in-VM agent
	// brings the feature up on the same constant port — the port number
	// itself is never per-VM data.
	Trace  bool // ship the agent's span buffer to the host trace sink
	Attach bool // vsock-listen for inbound exec-attach sessions
	Net    bool // dial sandboxd's per-VM userspace TCP/IP stack
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
	// Translate, when non-nil, asks virtiofsd to present this share's
	// files to the guest as owned by Translate.UID:GID — rather than
	// identity-passing the host owner through sandboxd's userns — and
	// to map guest-side writes back to that host owner. This lets a
	// non-root container, running as the image's USER uid:gid, read
	// and write a host-backed workspace whose files belong to the dev
	// who launched daft. It assumes the share's host files are owned
	// by the uid that maps to ns-uid 0 (the sandboxd user), which is
	// the common "dev runs daft on their own files" case.
	//
	// nil (the default) is identity passthrough, which is correct for
	// the bundle/rootfs share whose tree legitimately carries many
	// uids. Requires virtiofsd >= 1.13 (the --translate-uid/-gid
	// software id-mapping).
	Translate *ShareTranslate
}

// ShareTranslate carries the container uid/gid a Share is presented as.
// See ShareConfig.Translate.
type ShareTranslate struct {
	UID int
	GID int
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
	if err := c.validatePortForwards(); err != nil {
		return err
	}
	if c.ObserverLogPath != "" && !strings.HasPrefix(c.ObserverLogPath, "/") {
		return fmt.Errorf("sandbox: ObserverLogPath %q must be absolute", c.ObserverLogPath)
	}
	return nil
}

// validatePortForwards enforces the per-PortForward invariants
// (non-zero ports, unique guest-side VsockPort, no collision with an
// enabled feature's fixed port). Cloud Hypervisor would otherwise
// surface a duplicate bind as an opaque virtio-vsock error at boot.
func (c *Config) validatePortForwards() error {
	if len(c.PortForwards) == 0 {
		return nil
	}
	seen := make(map[uint32]bool, len(c.PortForwards))
	// Each enabled control-channel feature occupies its fixed constant
	// port; a port-forward may not reuse it (CH would surface the
	// duplicate bind as an opaque virtio-vsock error at boot).
	reserved := map[uint32]string{}
	if v := c.Vsock; v != nil {
		if v.Trace {
			reserved[agent.DefaultTraceVsockPort] = "the trace channel"
		}
		if v.Attach {
			reserved[agent.DefaultAttachVsockPort] = "the attach channel"
		}
		if v.Net {
			reserved[agent.DefaultNetVsockPort] = "the net channel"
		}
	}
	for i, pf := range c.PortForwards {
		if pf.VsockPort == 0 {
			return fmt.Errorf("sandbox: PortForwards[%d].VsockPort must be non-zero", i)
		}
		if pf.ContainerPort == 0 {
			return fmt.Errorf("sandbox: PortForwards[%d].ContainerPort must be non-zero", i)
		}
		if seen[pf.VsockPort] {
			return fmt.Errorf("sandbox: duplicate PortForwards[*].VsockPort %d", pf.VsockPort)
		}
		seen[pf.VsockPort] = true
		if who, ok := reserved[pf.VsockPort]; ok {
			return fmt.Errorf("sandbox: PortForwards[%d].VsockPort %d collides with %s", i, pf.VsockPort, who)
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
	// The trace/attach/net feature ports are distinct compile-time
	// constants, so they can no longer collide with each other — there's
	// nothing left to validate here beyond the socket + CID.
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
	if s.Translate != nil {
		if s.Translate.UID < 0 || s.Translate.GID < 0 {
			return fmt.Errorf(
				"sandbox: Share.Translate uid/gid must be non-negative, got %d:%d",
				s.Translate.UID, s.Translate.GID,
			)
		}
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
	run(ctx context.Context, cfg Config, shareSockets []string, cgroupDir string, collectNet func() *observability.NetSample) (io.ReadCloser, func(), error)
}

// shareSidecar abstracts the virtiofsd subprocess. Lifecycle: start
// before the VMM (cloud-hypervisor connects to its socket(s) on
// boot), stop after the VMM exits. Returns one socket path per
// cfg.Shares entry, in the same order. Like runner, swappable in
// tests.
type shareSidecar interface {
	start(ctx context.Context, cfg Config, cgroupDir string) (socketPaths []string, stop func(), err error)
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
const kernelCmdlineBase = "console=hvc0 reboot=t panic=-1 cgroup_no_v1=all"

// defaultMemory is the guest RAM size used when Config.Memory is empty.
const defaultMemory = "256M"

// virtiofsdReadyTimeout caps how long start waits for virtiofsd's
// socket to appear before giving up. Should be near-instant on a warm
// host; the generous default leaves room for cold-start.
const virtiofsdReadyTimeout = 5 * time.Second

// start spawns one virtiofsd per cfg.Shares entry, returning a slice
// of socket paths (parallel to cfg.Shares). The returned stop tears
// down every spawned process and removes the temp dirs.
//
// cgroupDir, when non-empty, fork-places every virtiofsd into the
// shared per-VM cgroup via CLONE_INTO_CGROUP so memory.peak / io.stat
// account for the share sidecar's contributions alongside CH's.
// Empty is best-effort: virtiofsd boots in the parent cgroup and the
// observer's cgroup view simply doesn't see its bytes — same fallback
// as the rest of the cgroup wrap.
func (virtiofsdSidecar) start(ctx context.Context, cfg Config, cgroupDir string) (sockets []string, stop func(), err error) {
	sockets = make([]string, 0, len(cfg.Shares))
	stops := make([]func(), 0, len(cfg.Shares))

	stopAll := func() {
		// Reap every virtiofsd concurrently so one wedged on a stuck
		// backing fs (D-state) doesn't strand the reap of its healthy
		// siblings — same no-stranding rationale as vmTeardown.run.
		var wg sync.WaitGroup
		for _, s := range stops {
			wg.Go(s)
		}
		wg.Wait()
	}

	for i, share := range cfg.Shares {
		sock, oneStop, oneErr := startOneVirtiofsd(ctx, cfg.Virtiofsd, share, cgroupDir)
		if oneErr != nil {
			stopAll()
			return nil, nil, fmt.Errorf("sandbox: share %d (%s): %w", i, share.Tag, oneErr)
		}
		sockets = append(sockets, sock)
		stops = append(stops, oneStop)
	}
	return sockets, stopAll, nil
}

// translateArgs builds virtiofsd's bidirectional id-translation flags
// for a Share's Translate config (nil → no flags, identity passthrough).
// The four rules pin a single host owner (the uid/gid that maps to
// ns-0, i.e. the sandboxd user) to the container's uid/gid in both
// directions:
//
//   - host:0:UID:1 — getattr/readdir presents host-0's files to the
//     guest as owned by the container uid, so a non-root container sees
//     them as its own.
//   - guest:UID:0:1 — create/chown writes the container uid back to
//     host-0, so files persist owned by the dev (not a userns subuid).
//
// Both directions are required: with only the host→guest rule, guest
// writes land as the userns-mapped subordinate id instead of the dev.
// uid 0:0 (a root container) yields identity rules — the caller skips
// Translate entirely in that case, so this only runs for non-root.
func translateArgs(t *ShareTranslate) []string {
	if t == nil {
		return nil
	}
	return []string{
		flagTranslateUID, fmt.Sprintf("host:0:%d:1", t.UID),
		flagTranslateUID, fmt.Sprintf("guest:%d:0:1", t.UID),
		flagTranslateGID, fmt.Sprintf("host:0:%d:1", t.GID),
		flagTranslateGID, fmt.Sprintf("guest:%d:0:1", t.GID),
	}
}

// startOneVirtiofsd spawns a single virtiofsd against share, writes
// its socket into a fresh temp dir, and waits for the socket to
// appear before returning. stop tears down the process + temp dir.
// cgroupDir, when non-empty, places the spawned process in the shared
// per-VM cgroup (CLONE_INTO_CGROUP) so its resource use rolls up
// into the same accounting as CH; empty is a no-op.
func startOneVirtiofsd(ctx context.Context, binary string, share *ShareConfig, cgroupDir string) (socket string, stop func(), err error) {
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

	// --sandbox=none keeps virtiofsd from trying to chroot/setuid;
	// we run it inside daft-sandboxd's user namespace (set up by
	// the userns re-exec scaffold at startup), where it has the
	// CAP_CHOWN it needs to satisfy container-side chowns into the
	// subordinate id range. No `--uid-map` / `--gid-map` is needed:
	// the kernel translates uids/gids through sandboxd's userns,
	// and virtiofsd's identity passthrough lands the translated
	// host ids on the underlying fs. A workspace share additionally
	// carries --translate-uid/-gid rules (see translateArgs) so a
	// non-root container sees and writes it as its own user.
	args := make([]string, 0, 5+8)
	args = append(
		args,
		"--socket-path", socket,
		"--shared-dir", share.HostDir,
		"--sandbox=none",
	)
	args = append(args, translateArgs(share.Translate)...)
	// #nosec G204 — `binary` is cfg.Virtiofsd, an absolute path passed
	// by the host operator who's already trusted to spawn the VMM, and
	// every argv element is either a const flag or a daft-derived path /
	// numeric id. There's no untrusted user input on the argv here.
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	// Setpgid + Pdeathsig + WaitDelay + group-kill-on-cancel so a
	// wedged or orphaned virtiofsd can't hang our reaper (see
	// hardenSpawn).
	hardenSpawn(cmd)

	closeCgroupFD := placeInCgroup(cmd, cgroupDir)

	if err := cmd.Start(); err != nil {
		closeCgroupFD()
		cleanup()
		return "", nil, fmt.Errorf("spawn virtiofsd: %w", err)
	}
	closeCgroupFD()

	stop = func() {
		// Best-effort teardown. Group-kill on an already-exited
		// process returns ESRCH (normalised to nil by
		// killProcessGroup) — fine, race between us and the VM.
		// Anything else (e.g. EPERM) is a real problem we can't
		// recover from, so let oops.Skip exit on it.
		pid := 0
		if cmd.Process != nil {
			pid = cmd.Process.Pid
			oops.Skip("kill virtiofsd group",
				killProcessGroup(cmd), os.ErrProcessDone)
		}
		// Wait returns *exec.ExitError after the kill — expected and
		// informational; benignReapError also tolerates a WaitDelay
		// overrun or ctx-cancel race. Other error types (ECHILD,
		// runtime broken) signal a real problem. The watchdog keeps
		// the wait loud if virtiofsd is wedged in D-state on a stuck
		// backing fs.
		reapWithWatchdog("virtiofsd", pid, procWatchdogFirst, procWatchdogMax, func() {
			if waitErr := cmd.Wait(); waitErr != nil && !benignReapError(waitErr) {
				oops.Must("wait virtiofsd", waitErr)
			}
		})
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

// buildKernelCmdline returns the cmdline string CH boots with. As of the
// slice-3 cutover it carries **zero `daft.*` tokens**: every per-VM
// operational setting (shares, bundle, port-forwards, overlays, observe
// paths, and the trace/attach/net feature toggles) now travels over the
// vsock control channel as a GuestConfig (see buildGuestConfig), which —
// unlike the world-readable, un-namespaceable kernel cmdline — a nested
// container can't read. The cmdline is therefore just the static base
// (console/init plumbing). Kept as a function (not inlined) so the boot
// path has one obvious place to add a genuinely kernel-level parameter.
func buildKernelCmdline(_ Config) string {
	return kernelCmdlineBase
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

// startObserverIfConfigured constructs and starts a per-VM
// observability.Observer for pid if cfg.ObserverLogPath is set.
// Errors are reported via oops.Log and the function returns nil
// so callers can treat "no observer" and "observer failed to
// start" the same way — observability is a debugging surface, not
// a correctness invariant of the sandbox launch.
//
// cgroupDir is the path of the per-VM cgroup the workload runs
// under, or "" if the cgroup wrap step failed (no delegation, etc.).
// Empty just disables cgroup sampling; the /proc sampler still
// runs and produces a useful (if more limited) trace.
//
// collectNet, when non-nil, is the per-VM sandboxnet byte-counter
// snapshot; the observer pulls it once per tick to record rx/tx
// bytes through the userspace netstack. nil disables the network
// block — appropriate for sandboxes without a vsock net device.
func startObserverIfConfigured(cfg Config, pid int, cgroupDir string, collectNet func() *observability.NetSample) *observability.Observer {
	if cfg.ObserverLogPath == "" {
		return nil
	}
	obs, err := observability.New(pid, cfg.ObserverLogPath,
		observability.Options{
			MaxBytes:   cfg.ObserverMaxBytes,
			CgroupDir:  cgroupDir,
			CollectNet: collectNet,
		})
	if err != nil {
		oops.Log("observability.New", err)
		return nil
	}
	if err := obs.Start(); err != nil {
		oops.Log("observability.Start", err)
		return nil
	}
	return obs
}

// removeVMCgroupBestEffort wraps observability.RemoveCgroup with
// the project's "log and proceed" pattern. Empty path is a no-op
// so callers can use it unconditionally even when the cgroup
// wrap was skipped. Factored out of run()'s three cleanup sites
// so the error-handling branch is covered once instead of three
// times.
func removeVMCgroupBestEffort(path string) {
	if path == "" {
		return
	}
	if err := observability.RemoveCgroup(path); err != nil {
		oops.Log("observability.RemoveCgroup", err)
	}
}

// createVMCgroupFn is the per-VM cgroup constructor as a package var so
// the package's unit tests — which run without cgroup delegation and
// with fake VMM runners — can substitute a stub that skips real cgroup
// creation. In production and in the (delegated) integration tests it is
// the real createVMCgroup.
//
//nolint:gochecknoglobals // test seam.
var createVMCgroupFn = createVMCgroup

// createVMCgroup creates a per-VM cgroup v2 sub-directory and
// returns its absolute path. It is a HARD precondition: any failure
// (no cgroup v2, no delegation, "Permission denied" on an
// un-delegated session) returns an error and the caller aborts the
// boot. Per CONTRIBUTING "Probe preconditions at startup; no silent
// fallbacks", the per-VM cgroup — which bounds a runaway VM's blast
// radius and collects its resource trace — is required infrastructure,
// not a best-effort optimisation, so we never boot a VM we can't
// account for. Delegation itself is guaranteed earlier, at process
// startup, by the systemd-run self-wrap (internal/cgroupwrap); by the
// time we reach here a failure means a genuinely broken environment.
//
// `name` should be a unique identifier for this VM; the caller is
// responsible for ensuring uniqueness within the parent cgroup.
// The lifecycle pairs with removeVMCgroupBestEffort in the
// cleanup closure.
//
// Returning the path (rather than an O_PATH fd) lets multiple
// spawn-helpers (cloud-hypervisor + every virtiofsd sidecar) open
// their own fd at fork time via placeInCgroup, so the whole VM
// process tree lands in one accounting unit without coordinating
// fd ownership across helpers.
//
// Before returning the path, createVMCgroup probes that an O_PATH
// fd can actually be opened on the new directory and closes it
// immediately. This keeps the all-or-nothing precondition the old
// single-shot setupVMCgroup enforced: either every spawn-helper
// can place into the cgroup, or the boot aborts. Without the probe,
// a cgroup that's creatable-but-not-openable would let some
// processes place and others not, silently corrupting the cgroup
// accounting view (memory.peak / io.stat missing whichever helper
// failed to open).
func createVMCgroup(name string) (string, error) {
	// Per-VM cgroups go under the prepared scope (a sibling of the
	// supervisor leaf, with the memory/cpu controllers enabled) when
	// startup set it; otherwise the caller's own cgroup, the pre-limits
	// fallback for unit tests and unprepared paths.
	parent := vmCgroupParent
	if parent == "" {
		var err error
		parent, err = observability.CurrentCgroup()
		if err != nil {
			return "", fmt.Errorf("sandbox: locate own cgroup for per-VM accounting: %w", err)
		}
	}
	dir, err := observability.CreateSubCgroup(parent, name)
	if err != nil {
		return "", fmt.Errorf("sandbox: create per-VM cgroup: %w", err)
	}
	fd, err := observability.OpenCgroupFD(dir)
	if err != nil {
		removeVMCgroupBestEffort(dir)
		return "", fmt.Errorf("sandbox: open per-VM cgroup for placement: %w", err)
	}
	oops.Skip("close cgroup probe fd", syscall.Close(fd), nil)
	return dir, nil
}

// placeInCgroup wires cmd.SysProcAttr so cmd.Start fork-places the
// child into the cgroup at cgroupDir via CLONE_INTO_CGROUP. The
// returned closeFD must be called AFTER cmd.Start (whether it
// succeeded or not) to release the O_PATH fd; the kernel keeps
// its own reference via the clone arg. cgroupDir == "" is a
// no-op (no delegation, no wrap) and the closeFD is also a no-op.
//
// Any failure to open the fd is best-effort: logged via oops and
// the cmd is left without cgroup placement, matching createVMCgroup's
// "sandbox still boots without observability" contract.
func placeInCgroup(cmd *exec.Cmd, cgroupDir string) (closeFD func()) {
	if cgroupDir == "" {
		return func() {}
	}
	fd, err := observability.OpenCgroupFD(cgroupDir)
	if err != nil {
		oops.Log("observability.OpenCgroupFD", err)
		return func() {}
	}
	cmd.SysProcAttr = withCgroupFD(cmd.SysProcAttr, fd)
	return func() {
		oops.Skip("close cgroup fd", syscall.Close(fd), nil)
	}
}

// procWaitDelay bounds how long cmd.Wait tolerates the child's stdio
// pipes staying open after the process itself has exited — e.g. an
// orphaned grandchild that inherited them and escaped the process
// group. Past this, Wait force-closes the pipes and returns instead
// of blocking forever. It does NOT bound a process still alive in
// uninterruptible kernel sleep (D-state); reapWithWatchdog covers
// that case.
const procWaitDelay = 5 * time.Second

// Watchdog escalation for a reap that won't complete: warn after
// procWatchdogFirst, then back off (doubling) up to procWatchdogMax
// between warnings, so a permanently-wedged VM keeps signalling
// without flooding the log. The first interval is deliberately longer
// than procWaitDelay so the orphan-pipe case (already bounded by
// WaitDelay) never trips the watchdog — a warning here means the
// genuine D-state case.
const (
	procWatchdogFirst = 10 * time.Second
	procWatchdogMax   = 60 * time.Second
)

// hardenSpawn configures a VMM/sidecar subprocess so a wedged or
// orphaned child can neither outlive its supervisor nor stall its
// reaper:
//
//   - Setpgid puts the child in its own process group, so stop() can
//     reap the whole tree (the VMM plus any helper it forks — vhost-
//     user backends, virtiofsd's own children) with one negative-PID
//     kill. Without it, an orphan that inherited our stdio pipe keeps
//     the pipe open and hangs cmd.Wait forever.
//   - Pdeathsig (Linux only) SIGKILLs the child if sandboxd dies, so a
//     supervisor crash doesn't strand running VMMs.
//   - WaitDelay bounds the post-exit wait for those pipes to close
//     (the rare orphan that escaped the process group).
//   - Cancel group-kills on ctx cancellation; CommandContext's default
//     kills only the leader PID, missing the group.
//
// It does not address a child wedged in uninterruptible kernel sleep
// (D-state) — nothing in userspace can kill one; reapWithWatchdog
// keeps that case loud and attributable instead.
func hardenSpawn(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	setParentDeathSignal(cmd.SysProcAttr)
	cmd.WaitDelay = procWaitDelay
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
}

// killProcessGroup SIGKILLs cmd's entire process group (the negative
// PID that Setpgid made distinct). Safe only before cmd.Wait reaps the
// child: until then the zombie pins the PID/PGID so the negative-PID
// target can't be recycled onto an unrelated group. ESRCH (the group
// is already gone) is normalised to nil, and an unstarted cmd is a
// no-op.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil &&
		!errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// benignReapError reports whether a cmd.Wait error during teardown is
// expected rather than a problem worth crashing on. A killed process
// surfaces as *exec.ExitError; an orphan holding a pipe past WaitDelay
// surfaces as exec.ErrWaitDelay; a ctx cancellation that raced ahead of
// a clean exit surfaces as context.Canceled / DeadlineExceeded.
// Anything else (ECHILD, a broken runtime) is real and reaches
// oops.Must.
func benignReapError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) ||
		errors.Is(err, exec.ErrWaitDelay) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// reapWithWatchdog runs wait — a blocking cmd.Wait teardown — and,
// until it returns, emits an escalating loud warning naming what we're
// stuck reaping. It never abandons the wait. A child wedged in
// uninterruptible kernel sleep (D-state) can't be killed, and giving up
// reclaims nothing: the cgroup rmdir EBUSYs while the task lingers and
// the PID stays unreapable, so abandoning would only discard the one
// signal that points at daft as the holder of those resources while the
// host slowly leaks them. So daft keeps waiting and keeps complaining,
// loudly enough that the cause is attributable instead of mysterious.
func reapWithWatchdog(what string, pid int, first, maxInterval time.Duration, wait func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait()
	}()
	interval := first
	waited := time.Duration(0)
	timer := time.NewTimer(interval)
	defer timer.Stop()
	//daft:lint:disable=forloopbound reason="returns when the reap goroutine closes done; intentionally unbounded otherwise — a D-state reap is meant to be waited on forever (see doc comment)"
	for {
		select {
		case <-done:
			return
		case <-timer.C:
			waited += interval
			oops.Log("sandbox: reap stalled", fmt.Errorf(
				"still waiting to reap %s (pid %d) after %s — likely wedged in "+
					"uninterruptible kernel sleep (D-state); its cgroup, memory and "+
					"vCPU threads stay held until the kernel releases it, and daft "+
					"will keep waiting", what, pid, waited,
			))
			if interval < maxInterval {
				interval *= 2
				if interval > maxInterval {
					interval = maxInterval
				}
			}
			timer.Reset(interval)
		}
	}
}

// vmTeardown collects a VM's shutdown steps so they run in the right
// order regardless of which setup step failed: the pure-Go receivers
// first, the process children reaped concurrently, the cgroup last.
type vmTeardown struct {
	cgroupDir string
	// funcStops are the pure-Go receiver shutdowns (vsock, sandboxnet).
	// They're fast and never block on a D-state task; closing their
	// listeners + tracked conns also unblocks per-conn goroutines still
	// pumping to the about-to-die VMM.
	funcStops []func()
	// procStops reap spawned processes (the VMM, each virtiofsd). Each
	// kills its own process group, drains, and Waits via
	// reapWithWatchdog.
	procStops []func()
}

func (td *vmTeardown) run() {
	for i := len(td.funcStops) - 1; i >= 0; i-- {
		td.funcStops[i]()
	}
	// Reap concurrently so a child wedged in uninterruptible sleep
	// can't strand the reap of a healthy sibling. run() still blocks
	// until every reap returns, so a wedged VMM keeps daft visibly
	// waiting (and, via reapWithWatchdog, loudly complaining) rather
	// than silently leaking.
	var wg sync.WaitGroup
	for _, stop := range td.procStops {
		wg.Go(stop)
	}
	wg.Wait()
	// The cgroup can only be reclaimed once every task is gone — rmdir
	// EBUSYs otherwise, which is also why a wedged reap is worth
	// blocking on rather than abandoning.
	removeVMCgroupBestEffort(td.cgroupDir)
}

func (cloudHypervisorRunner) run(ctx context.Context, cfg Config, shareSockets []string, cgroupDir string, collectNet func() *observability.NetSample) (io.ReadCloser, func(), error) {
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

	// Setpgid + Pdeathsig + WaitDelay + group-kill-on-cancel so a
	// wedged or orphaned CH can't hang our reaper (see hardenSpawn).
	hardenSpawn(cmd)

	// Per-VM cgroup wrap is best-effort and owned by the
	// orchestration layer (helloWith/serveWith) so that virtiofsd
	// and CH share the same accounting unit. placeInCgroup is a
	// no-op when cgroupDir == "" — i.e. on hosts without delegation.
	closeCgroupFD := placeInCgroup(cmd, cgroupDir)

	if err := cmd.Start(); err != nil {
		closeCgroupFD()
		return nil, nil, fmt.Errorf("sandbox: start cloud-hypervisor: %w", err)
	}
	closeCgroupFD()

	// Per-VM observer is best-effort: a failed observer must not
	// fail the sandbox launch, so the helper logs via oops and
	// returns nil on any error.
	var obs *observability.Observer
	if cmd.Process != nil {
		obs = startObserverIfConfigured(cfg, cmd.Process.Pid, cgroupDir, collectNet)
	}

	stop := func() {
		if obs != nil {
			oops.Log("observability.Stop", obs.Stop())
		}
		pid := 0
		if cmd.Process != nil {
			pid = cmd.Process.Pid
			oops.Skip("kill cloud-hypervisor group",
				killProcessGroup(cmd), os.ErrProcessDone)
		}
		reapWithWatchdog("cloud-hypervisor", pid, procWatchdogFirst, procWatchdogMax, func() {
			// Drain stdout before Wait so the kernel buffer doesn't
			// keep the writer parked. Either Wait or this drain may
			// have already raced past EOF — io.Copy returns nil on
			// EOF, and a closed pipe surfaces as os.ErrClosed.
			if _, copyErr := io.Copy(io.Discard, stdout); copyErr != nil {
				oops.Skip("drain cloud-hypervisor stdout", copyErr,
					io.EOF, os.ErrClosed)
			}
			if waitErr := cmd.Wait(); waitErr != nil && !benignReapError(waitErr) {
				oops.Must("wait cloud-hypervisor", waitErr)
			}
		})
		// The cgroup itself is removed at the orchestration layer
		// after stopShare reaps the virtiofsd sidecars too — rmdir
		// only succeeds when every task in the cgroup is gone.
	}
	return stdout, stop, nil
}

// vmCgroupName returns the cgroup directory name for this VM. We
// don't have a sandbox UUID at this layer (the sandboxd reconciler
// owns that and doesn't propagate it down) so we derive a unique
// name from the cloud-hypervisor PID plus a process-lifetime
// counter — collisions are impossible within a sandboxd process
// because PIDs don't repeat and the counter is monotonic.
// vmCgroupSeq is mutated atomically.
//
//nolint:gochecknoglobals // process-lifetime monotonic counter, intentional package singleton.
var vmCgroupSeq atomic.Uint64

func vmCgroupName(cfg Config) string {
	// cfg.ObserverLogPath, when set, makes a perfectly good seed
	// for naming: it already encodes the test's tempdir / the
	// caller's identifier. Hash it into 8 hex chars + seq. Empty
	// path → name based only on seq + a fixed prefix so the
	// no-observer no-cgroup case still works for the
	// theoretical future caller that wants the wrap without the
	// observer.
	seq := vmCgroupSeq.Add(1)
	h := fnv.New64a()
	_, _ = h.Write([]byte(cfg.ObserverLogPath))
	return fmt.Sprintf("daft-vm-%016x-%d", h.Sum64(), seq)
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

// Serve is the long-running variant of Hello used by the
// orchestrator-driven sandboxd path. It boots the VMM, mirrors
// stdout to w in a background goroutine, and blocks until either
// ctx is cancelled or the VMM exits on its own. The VMM is killed
// before Serve returns regardless of outcome.
//
// Unlike Hello there is no banner gate — the agent inside the VM
// starts its vsock listeners early in boot, so attach + port-
// forward become reachable as soon as those listeners are bound
// regardless of any console-side handshake. The orchestrator
// signals teardown via ctx cancellation; a workload that exits on
// its own (PID 1 returns, kernel panics, VMM crashes) bubbles up
// as a normal Serve return without an error.
func Serve(ctx context.Context, w io.Writer, cfg Config) error {
	return serveWith(ctx, w, cfg, cloudHypervisorRunner{}, virtiofsdSidecar{})
}

// RunToCompletion boots cfg, waits for the agent's BOOT_COMPLETE event —
// which the agent sends immediately after the container's entrypoint
// process exits (internal/agent: runContainer blocks on the container,
// then signalBootComplete) — and tears the VM down. It is the
// one-shot-workload runner: unlike Hello it imposes no deadline of its
// own, so a long build (minutes) is bounded only by the caller's ctx;
// unlike Serve it returns when the workload finishes rather than running
// until cancellation. The executor's in-sandbox image builder (#101
// slice 3c) uses it to run a buildah build and know exactly when the
// build container has exited so it can read back the output tarball.
func RunToCompletion(ctx context.Context, w io.Writer, cfg Config) error {
	return helloWithTimeout(ctx, w, cfg, cloudHypervisorRunner{}, virtiofsdSidecar{}, 0)
}

// contextWithOptionalTimeout derives a child ctx that either inherits the
// parent's deadline alone (timeout == 0) or adds one of its own. The
// returned cancel doubles as the "kill the VM" lever waitForBootComplete
// pulls on ctx expiry, so it must always be called.
func contextWithOptionalTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout == 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// serveWith is Serve parameterised on its subprocess drivers so
// tests can swap in fakes. Production callers go through Serve.
func serveWith(ctx context.Context, w io.Writer, cfg Config, r runner, sc shareSidecar) error {
	ctx, span := trace.Start(ctx, "sandbox.Serve",
		trace.WithAttributes(
			trace.KV{Key: "kernel", Value: cfg.Kernel},
			trace.KV{Key: traceAttrShares, Value: strconv.Itoa(len(cfg.Shares))},
			trace.KV{Key: "bundle", Value: cfg.Bundle},
		))
	defer span.End()

	if err := cfg.validate(); err != nil {
		return trace.RecordError(ctx, err)
	}
	cfg = cfg.withDefaults()

	// Create the per-VM cgroup BEFORE startShares so virtiofsd
	// fork-places into it alongside cloud-hypervisor. HARD precondition:
	// a failure aborts the boot rather than degrading to /proc-only —
	// delegation is guaranteed at process startup (cgroupwrap), so a
	// failure here is a broken environment, not an expected condition. A
	// single vmTeardown orders shutdown (pure-Go receivers, then
	// processes reaped concurrently, then the cgroup last) on every
	// return path, including the early error returns below.
	cgroupDir, err := createVMCgroupFn(vmCgroupName(cfg))
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	td := &vmTeardown{cgroupDir: cgroupDir}
	defer td.run()
	// Bound the VM's host resource use: soft + hard memory and CPU caps on
	// the per-VM cgroup, derived from its guest-RAM / vCPU allocation.
	applyVMCgroupLimitsFn(w, cgroupDir, cfg)

	shareSockets, stopShare, err := startShares(ctx, cfg, sc, cgroupDir)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	td.procStops = append(td.procStops, stopShare)

	stopVsock, err := startVsockTraceReceiver(ctx, cfg)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	td.funcStops = append(td.funcStops, stopVsock)

	// serveWith runs until ctx/VMM-exit, so it doesn't wait on the
	// boot-complete signal (that's the one-shot Hello harness's job);
	// the config/event server still runs so the agent gets its config
	// and the event read loop drains cleanly.
	stopConfig, _, err := startVsockConfigServer(ctx, cfg)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	td.funcStops = append(td.funcStops, stopConfig)

	stopNet, collectNet, err := startVsockNetReceiver(ctx, cfg)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	td.funcStops = append(td.funcStops, stopNet)

	stdout, stop, err := runVMM(ctx, cfg, shareSockets, r, cgroupDir, collectNet)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	td.procStops = append(td.procStops, stop)

	// Mirror stdout for the VMM's full lifetime in a background
	// goroutine. The drain inside stop() catches any residual
	// bytes after we return.
	mirrored := make(chan struct{})
	go func() {
		defer close(mirrored)
		if _, copyErr := io.Copy(w, stdout); copyErr != nil {
			oops.Skip("sandbox: mirror Serve stdout", copyErr,
				io.EOF, os.ErrClosed)
		}
	}()

	// Block on whichever happens first — caller-side cancellation
	// or VMM exit (which causes the stdout pipe to close).
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-mirrored:
		return nil
	}
}

// helloWith is Hello parameterised on its subprocess drivers so tests
// can swap in fakes. Production code calls Hello, which threads the
// real cloudHypervisorRunner / virtiofsdSidecar.
func helloWith(ctx context.Context, w io.Writer, cfg Config, r runner, sc shareSidecar) error {
	return helloWithTimeout(ctx, w, cfg, r, sc, HelloTimeout)
}

// helloWithTimeout is helloWith parameterised on the boot-completion
// deadline. timeout == 0 means "no deadline of our own" — the wait is
// bounded only by ctx, which is what RunToCompletion needs for one-shot
// workloads (image builds) that legitimately run longer than HelloTimeout.
func helloWithTimeout(ctx context.Context, w io.Writer, cfg Config, r runner, sc shareSidecar, timeout time.Duration) error {
	ctx, span := trace.Start(ctx, "sandbox.Hello",
		trace.WithAttributes(
			trace.KV{Key: "kernel", Value: cfg.Kernel},
			trace.KV{Key: traceAttrShares, Value: strconv.Itoa(len(cfg.Shares))},
			trace.KV{Key: "bundle", Value: cfg.Bundle},
		))
	defer span.End()

	if err := cfg.validate(); err != nil {
		return trace.RecordError(ctx, err)
	}
	cfg = cfg.withDefaults()

	runCtx, cancel := contextWithOptionalTimeout(ctx, timeout)
	defer cancel()

	// Same cgroup + teardown choreography as serveWith — see the long
	// comment there. Hard precondition: a failure aborts the boot.
	cgroupDir, err := createVMCgroupFn(vmCgroupName(cfg))
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	td := &vmTeardown{cgroupDir: cgroupDir}
	defer td.run()
	applyVMCgroupLimitsFn(w, cgroupDir, cfg)

	shareSockets, stopShare, err := startShares(runCtx, cfg, sc, cgroupDir)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	td.procStops = append(td.procStops, stopShare)

	stopVsock, err := startVsockTraceReceiver(ctx, cfg)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	td.funcStops = append(td.funcStops, stopVsock)

	stopConfig, bootComplete, err := startVsockConfigServer(ctx, cfg)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	td.funcStops = append(td.funcStops, stopConfig)

	stopNet, collectNet, err := startVsockNetReceiver(ctx, cfg)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	td.funcStops = append(td.funcStops, stopNet)

	stdout, stop, err := runVMM(runCtx, cfg, shareSockets, r, cgroupDir, collectNet)
	if err != nil {
		return trace.RecordError(ctx, err)
	}
	td.procStops = append(td.procStops, stop)

	// Wait for the agent to report boot-complete over the vsock control
	// channel (bootComplete != nil means the VM has one). Fall back to
	// scanning the console banner only when there's no channel — the
	// banner is no longer a status signal except in that degraded case.
	if bootComplete != nil {
		waitCtx, waitSpan := trace.Start(runCtx, "sandbox.wait_boot_complete")
		defer waitSpan.End()
		return waitForBootComplete(waitCtx, cancel, w, stdout, bootComplete)
	}
	bannerCtx, bannerSpan := trace.Start(runCtx, "sandbox.scan_for_banner")
	defer bannerSpan.End()
	if err := scanForBanner(w, stdout); err != nil {
		return trace.RecordError(bannerCtx, err)
	}
	return nil
}

// waitForBootComplete waits for the agent to report boot-complete over
// the vsock control channel — the VM-status signal — then mirrors the
// console up to the banner so w holds the whole boot output before the
// caller tears the VM down. The two channels are independent: the agent
// prints the banner (slow virtio-console path) just before sending the
// event (fast vsock path), so killing the VM on the bare event would
// race the banner off the console. Instead scanForBanner is the single
// console reader; on the event we let it finish draining to the banner.
//
// If the banner arrives first (the agent printed it but the control
// channel never delivered — a degraded boot) that completes the wait as
// a fallback. On ctx timeout it kills the VM to unblock the scan and
// returns the ctx error.
func waitForBootComplete(ctx context.Context, cancelVM context.CancelFunc, w io.Writer, stdout io.Reader, bootComplete <-chan struct{}) error {
	scanned := make(chan error, 1)
	go func() { scanned <- scanForBanner(w, stdout) }()

	select {
	case <-bootComplete:
		select {
		case err := <-scanned:
			return err
		case <-ctx.Done():
			cancelVM()
			<-scanned
			return ctx.Err()
		}
	case err := <-scanned:
		return err
	case <-ctx.Done():
		cancelVM()
		<-scanned
		return ctx.Err()
	}
}

// startShares spawns virtiofsd for every share in cfg. Returns a
// no-op stopShare and empty sockets when cfg has no shares so the
// caller can defer stopShare unconditionally. Wrapped in its own
// span so the cold-boot trace breaks down virtiofsd-spawn time
// separately from VMM-spawn time.
func startShares(ctx context.Context, cfg Config, sc shareSidecar, cgroupDir string) (sockets []string, stop func(), err error) {
	if len(cfg.Shares) == 0 {
		return nil, func() {}, nil
	}
	ctx, span := trace.Start(ctx, "sandbox.start_virtiofsd",
		trace.WithAttributes(trace.KV{Key: traceAttrShares, Value: strconv.Itoa(len(cfg.Shares))}))
	defer span.End()
	sockets, stop, err = sc.start(ctx, cfg, cgroupDir)
	if err != nil {
		return nil, func() {}, trace.RecordError(ctx,
			fmt.Errorf("sandbox: start virtiofsd: %w", err))
	}
	return sockets, stop, nil
}

// runVMM spawns the configured VMM (Cloud Hypervisor today) and
// returns its stdout pipe. Wrapped in its own span so the trace
// distinguishes "VMM spawn" from "wait for guest banner".
func runVMM(ctx context.Context, cfg Config, shareSockets []string, r runner, cgroupDir string, collectNet func() *observability.NetSample) (stdout io.ReadCloser, stop func(), err error) {
	ctx, span := trace.Start(ctx, "sandbox.run_cloud_hypervisor")
	defer span.End()
	stdout, stop, err = r.run(ctx, cfg, shareSockets, cgroupDir, collectNet)
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
