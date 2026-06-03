package imagestore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// runtimeSpec is the subset of the OCI runtime spec we generate. It
// is intentionally incomplete — only fields the agent's `crun run
// --no-pivot` cares about, mirroring contrib/hello-bundle/config.json.
//
// Marshalled as-is to config.json. JSON tags use the OCI spec field
// names so the result is a valid runtime-spec document.
type runtimeSpec struct {
	OCIVersion string             `json:"ociVersion"`
	Process    runtimeProcess     `json:"process"`
	Root       runtimeRoot        `json:"root"`
	Hostname   string             `json:"hostname"`
	Mounts     []runtimeMount     `json:"mounts"`
	Linux      runtimeLinuxConfig `json:"linux"`
}

type runtimeProcess struct {
	Terminal        bool                `json:"terminal"`
	User            runtimeUser         `json:"user"`
	Args            []string            `json:"args"`
	Env             []string            `json:"env,omitempty"`
	Cwd             string              `json:"cwd"`
	Capabilities    runtimeCapabilities `json:"capabilities"`
	Rlimits         []any               `json:"rlimits"`
	NoNewPrivileges bool                `json:"noNewPrivileges"`
}

type runtimeUser struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

type runtimeCapabilities struct {
	Bounding  []string `json:"bounding"`
	Effective []string `json:"effective"`
	Permitted []string `json:"permitted"`
	Ambient   []string `json:"ambient"`
}

type runtimeRoot struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type runtimeMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Options     []string `json:"options,omitempty"`
}

type runtimeLinuxConfig struct {
	Namespaces    []runtimeNamespace `json:"namespaces"`
	Resources     *runtimeResources  `json:"resources,omitempty"`
	Seccomp       *runtimeSeccomp    `json:"seccomp,omitempty"`
	MaskedPaths   []string           `json:"maskedPaths"`
	ReadonlyPaths []string           `json:"readonlyPaths"`
}

// runtimeSeccomp is the container's syscall filter. v0 uses it for one
// narrow job: deny socket(AF_VSOCK) so a confused workload can't open
// its own vsock channel to the host. DefaultAction is ALLOW — this is
// an accidental-breakage guard, not a security sandbox (the VM is the
// boundary). See PLAN.md "accidental-clobber surface".
type runtimeSeccomp struct {
	DefaultAction string                  `json:"defaultAction"`
	Architectures []string                `json:"architectures,omitempty"`
	Syscalls      []runtimeSeccompSyscall `json:"syscalls,omitempty"`
}

// runtimeSeccompSyscall is one rule: when any name in Names is called
// and every Args predicate matches, take Action (returning ErrnoRet
// for SCMP_ACT_ERRNO).
type runtimeSeccompSyscall struct {
	Names    []string            `json:"names"`
	Action   string              `json:"action"`
	ErrnoRet *uint               `json:"errnoRet,omitempty"`
	Args     []runtimeSeccompArg `json:"args,omitempty"`
}

// runtimeSeccompArg matches one syscall argument: arg[Index] Op Value.
type runtimeSeccompArg struct {
	Index uint64 `json:"index"`
	Value uint64 `json:"value"`
	Op    string `json:"op"`
}

// runtimeResources carries the cgroup resource limits. We only populate
// the device controller, to let the container open the bind-mounted
// /dev/kvm — crun's default cgroup-v2 eBPF device filter denies anything
// outside the standard set, so a bind mount alone is not enough.
type runtimeResources struct {
	Devices []runtimeDeviceCgroup `json:"devices,omitempty"`
}

// runtimeDeviceCgroup is one cgroup device rule. Type/Major/Minor are
// omitted to mean "all" per the OCI spec.
type runtimeDeviceCgroup struct {
	Allow  bool   `json:"allow"`
	Type   string `json:"type,omitempty"`
	Major  *int64 `json:"major,omitempty"`
	Minor  *int64 `json:"minor,omitempty"`
	Access string `json:"access,omitempty"`
}

type runtimeNamespace struct {
	Type string `json:"type"`
}

// buildRuntimeConfig produces a runtime spec for img with overrides
// from opts applied. The image's Entrypoint+Cmd, Env, and WorkingDir
// are honoured by default; opts.{Args,Env,Cwd,Hostname,ExtraMounts}
// replace / extend each.
func buildRuntimeConfig(img v1.Image, opts BundleOptions) (runtimeSpec, error) {
	cfgFile, err := img.ConfigFile()
	if err != nil {
		return runtimeSpec{}, fmt.Errorf("imagestore: image config: %w", err)
	}
	imgCfg := cfgFile.Config

	args := mergeArgs(opts.Args, imgCfg.Entrypoint, imgCfg.Cmd)
	if len(args) == 0 {
		return runtimeSpec{}, fmt.Errorf(
			"imagestore: image has no entrypoint/cmd; supply BundleOptions.Args",
		)
	}

	mounts := defaultRuntimeMounts()
	for _, m := range opts.ExtraMounts {
		mounts = append(mounts, runtimeMount(m))
	}

	user, err := parseImageUser(imgCfg.User)
	if err != nil {
		return runtimeSpec{}, err
	}
	// No nested OCI userns: the container shares sandboxd's userns
	// (D7 slice 1). See PLAN.md "D7 — rootless storage translation".
	linux := defaultRuntimeLinuxConfig()
	caps := emptyCapabilities()
	if opts.Nested {
		mounts, linux = applyNestedVirt(mounts, linux)
	}
	if opts.PrivilegedFuseMounts {
		mounts, linux, caps = applyPrivilegedFuseMounts(mounts, linux, caps)
	}
	return runtimeSpec{
		OCIVersion: "1.0.2",
		Process: runtimeProcess{
			User:            user,
			Args:            args,
			Env:             mergeEnv(imgCfg.Env, opts.Env),
			Cwd:             firstNonEmpty(opts.Cwd, imgCfg.WorkingDir, "/"),
			Capabilities:    caps,
			Rlimits:         []any{},
			NoNewPrivileges: true,
		},
		// Rootfs is mounted writable. v0 design puts the VM at
		// the security boundary, so a writable container rootfs is
		// fine. Load-bearing for nix: nix 2.34's LocalStore tries
		// to mount-remount /nix/store rw when uid 0 and the store
		// is ro (which it is when rootfs is ro). With empty Linux
		// capabilities the remount fails. Writable rootfs means
		// statvfs(/nix/store) reports rw and nix skips the remount.
		Root:     runtimeRoot{Path: "rootfs", Readonly: false},
		Hostname: firstNonEmpty(opts.Hostname, "", "daft-container"),
		Mounts:   mounts,
		Linux:    linux,
	}, nil
}

// applyNestedVirt grants the container nested-virtualisation access:
// it binds the VM's /dev/kvm into the container and adds the 10:232
// cgroup device-allow so crun's default eBPF device filter permits
// the open. devtmpfs in the VM exposes /dev/kvm once the guest kernel
// has CONFIG_KVM built in; the container's /dev is a fresh tmpfs, so
// without the bind the node is absent. The two must travel together —
// the bind without the allow EPERMs, the allow without the bind has
// nothing to open. Returns fresh slices so the unconditional defaults
// stay unprivileged for every non-nested workload.
func applyNestedVirt(mounts []runtimeMount, linux runtimeLinuxConfig) ([]runtimeMount, runtimeLinuxConfig) {
	mounts = append(mounts, runtimeMount{
		Destination: devKVMPath, Type: mountTypeBind, Source: devKVMPath,
		Options: []string{mountOptBind, "rw"},
	})
	if linux.Resources == nil {
		linux.Resources = &runtimeResources{}
	}
	linux.Resources.Devices = append(linux.Resources.Devices,
		runtimeDeviceCgroup{Allow: true, Type: "c", Major: int64Ptr(10), Minor: int64Ptr(232), Access: "rwm"})
	return mounts, linux
}

// applyPrivilegedFuseMounts grants the container the ability to mount
// FUSE filesystems: it binds the VM's /dev/fuse into the container,
// adds the 10:229 cgroup device-allow (so crun's default eBPF device
// filter permits the open), and adds CAP_SYS_ADMIN to the capability
// set (the mount(2) syscall needs it — the container is neither
// userns-wrapped nor running the setuid fusermount helper, so a real
// capability is the only lever). All three must travel together: the
// device-allow without the bind has nothing to open, the bind without
// the cap EPERMs on mount. CAP_SYS_ADMIN is added to bounding,
// effective and permitted; the uid-0 container keeps it across execve
// via the kernel's root rule (it is already in permitted, so
// NoNewPrivileges does not strip it), reaching the deeply-exec'd test
// binaries `make check` spawns. Returns fresh slices so the
// unconditional defaults stay unprivileged for every other workload.
// See PLAN.md "accidental-clobber surface" / the PrivilegedFuseMounts
// proto for why this is a deliberate, danger-named opt-in.
func applyPrivilegedFuseMounts(
	mounts []runtimeMount, linux runtimeLinuxConfig, caps runtimeCapabilities,
) ([]runtimeMount, runtimeLinuxConfig, runtimeCapabilities) {
	mounts = append(mounts, runtimeMount{
		Destination: devFusePath, Type: mountTypeBind, Source: devFusePath,
		Options: []string{mountOptBind, "rw"},
	})
	if linux.Resources == nil {
		linux.Resources = &runtimeResources{}
	}
	linux.Resources.Devices = append(linux.Resources.Devices,
		runtimeDeviceCgroup{Allow: true, Type: "c", Major: int64Ptr(10), Minor: int64Ptr(229), Access: "rwm"})
	caps.Bounding = append(caps.Bounding, capSysAdmin)
	caps.Effective = append(caps.Effective, capSysAdmin)
	caps.Permitted = append(caps.Permitted, capSysAdmin)
	return mounts, linux, caps
}

// mergeArgs returns override unchanged when non-nil, otherwise
// concatenates entrypoint+cmd into a fresh slice (matching how the
// OCI runtime spec composes them).
func mergeArgs(override, entrypoint, cmd []string) []string {
	if override != nil {
		return override
	}
	out := make([]string, 0, len(entrypoint)+len(cmd))
	out = append(out, entrypoint...)
	out = append(out, cmd...)
	return out
}

// mergeEnv overlays overlay onto base, keyed by the NAME part of each
// "NAME=VALUE" entry: an overlay entry replaces a base entry with the
// same NAME in place, and a new NAME is appended. Base order is
// preserved so the image's own ordering is stable. Empty overlay
// returns base unchanged (the pre-J behaviour: image env as-is).
func mergeEnv(base, overlay []string) []string {
	if len(overlay) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(overlay))
	pos := make(map[string]int, len(base)+len(overlay))
	add := func(entry string) {
		name, _, _ := strings.Cut(entry, "=")
		if p, ok := pos[name]; ok {
			out[p] = entry
			return
		}
		pos[name] = len(out)
		out = append(out, entry)
	}
	for _, e := range base {
		add(e)
	}
	for _, e := range overlay {
		add(e)
	}
	return out
}

func int64Ptr(v int64) *int64 { return &v }

func firstNonEmpty(primary, secondary, fallback string) string {
	if primary != "" {
		return primary
	}
	if secondary != "" {
		return secondary
	}
	return fallback
}

// parseImageUser projects the OCI image-config `User` field onto
// the runtime spec's numeric uid/gid pair. Empty string defaults
// to 0:0 (root) which matches the historical pre-image behaviour.
// Only numeric uid[:gid] is supported — symbolic names would
// require /etc/passwd lookups inside the image, which we don't
// stage. Returns an error on a name-form User or a malformed
// integer; the bundler surfaces it before any disk write.
func parseImageUser(u string) (runtimeUser, error) {
	if u == "" {
		return runtimeUser{UID: 0, GID: 0}, nil
	}
	uidStr, gidStr, _ := strings.Cut(u, ":")
	uid, uerr := strconv.Atoi(uidStr)
	if uerr != nil {
		return runtimeUser{}, fmt.Errorf(
			"imagestore: image User %q: only numeric uid[:gid] supported", u,
		)
	}
	gid := 0
	if gidStr != "" {
		g, gerr := strconv.Atoi(gidStr)
		if gerr != nil {
			return runtimeUser{}, fmt.Errorf(
				"imagestore: image User %q gid not numeric", u,
			)
		}
		gid = g
	}
	return runtimeUser{UID: uid, GID: gid}, nil
}

func emptyCapabilities() runtimeCapabilities {
	return runtimeCapabilities{
		Bounding: []string{}, Effective: []string{},
		Permitted: []string{}, Ambient: []string{},
	}
}

func defaultRuntimeLinuxConfig() runtimeLinuxConfig {
	return runtimeLinuxConfig{
		// `network` is intentionally omitted: the v0 design is one
		// container per sandbox VM, and the VM is the security
		// boundary, so the container shares the VM's loopback
		// instead of getting its own netns. This is load-bearing
		// for `daft workspace forward`, where the agent dials the
		// container's port via 127.0.0.1 from inside the VM's
		// init namespace — a separate container netns would put
		// the container's listener out of reach of that dial.
		// Slice D5 (multi-container per VM) will need to revisit.
		Namespaces: []runtimeNamespace{
			{Type: "pid"},
			{Type: "ipc"},
			{Type: "uts"},
			{Type: "mount"},
			{Type: "cgroup"},
		},
		// Resources (the /dev/kvm 10:232 device-allow) are added only
		// for nested-virt workspaces — see applyNestedVirt. A plain
		// workload gets crun's default device filter with no extra holes.
		Seccomp: defaultRuntimeSeccomp(),
		MaskedPaths: []string{
			"/proc/kcore", "/proc/keys", "/proc/latency_stats",
			"/proc/timer_list", "/proc/sched_debug",
		},
		ReadonlyPaths: []string{
			"/proc/asound", "/proc/bus", "/proc/fs",
			"/proc/irq", "/proc/sys", "/proc/sysrq-trigger",
		},
	}
}

// Seccomp constants for the AF_VSOCK socket guard.
const (
	// afVsock is the AF_VSOCK address-family number (linux/socket.h),
	// matched against socket(2)'s domain argument (index 0).
	afVsock = 40
	// errnoEAFNOSUPPORT ("address family not supported") is returned
	// for a blocked socket(AF_VSOCK) — the same errno a kernel without
	// vsock would give, so probe-and-fallback workloads behave normally.
	errnoEAFNOSUPPORT = 97
)

// defaultRuntimeSeccomp returns the v0 syscall filter: allow everything
// except socket(AF_VSOCK), which ERRNOs with EAFNOSUPPORT. The container
// never needs vsock — egress is via tap0 in the VM netns and the agent
// (not the container) owns the vsock control/trace channels — so blocking
// the family stops a workload from accidentally opening its own host
// vsock conn without touching AF_INET/AF_UNIX. Native x86_64 only,
// matching daft's single-arch microVM target. The filter installs under
// the container's existing NoNewPrivileges=true (no CAP_SYS_ADMIN needed
// to load it). See PLAN.md "accidental-clobber surface".
func defaultRuntimeSeccomp() *runtimeSeccomp {
	errno := uint(errnoEAFNOSUPPORT)
	return &runtimeSeccomp{
		DefaultAction: "SCMP_ACT_ALLOW",
		Architectures: []string{"SCMP_ARCH_X86_64"},
		Syscalls: []runtimeSeccompSyscall{
			{
				Names:    []string{"socket"},
				Action:   "SCMP_ACT_ERRNO",
				ErrnoRet: &errno,
				Args: []runtimeSeccompArg{
					{Index: 0, Value: afVsock, Op: "SCMP_CMP_EQ"},
				},
			},
		},
	}
}

// Mount-option keywords reused across the default mount set.
const (
	mountOptNosuid  = "nosuid"
	mountOptNodev   = "nodev"
	mountTypeTmpfs  = "tmpfs"
	mountTypeDevpts = "devpts"
	mountTypeBind   = "bind"
	mountOptBind    = "bind"
	// devKVMPath is the host hypervisor device bound into a
	// nested-virt workload container (see applyNestedVirt).
	devKVMPath = "/dev/kvm"
	// devFusePath is the FUSE control device bound into a container
	// granted PrivilegedFuseMounts (see applyPrivilegedFuseMounts).
	devFusePath = "/dev/fuse"
	// capSysAdmin is the capability the FUSE mount(2) syscall needs;
	// it is "the new root" — see the PrivilegedFuseMounts proto doc.
	capSysAdmin = "CAP_SYS_ADMIN"
)

// defaultRuntimeMounts mirrors contrib/hello-bundle/config.json. Kept
// in code instead of an asset file because the runtime spec depends
// on per-call data (env, cwd) anyway, so the whole document is built
// programmatically.
//
// /tmp is mounted as a 256M tmpfs because most build tools (nix,
// gcc) need a writable scratch dir and the rootfs may be read-
// only depending on the OCI spec. Size capped so a runaway writer
// can't OOM the host.
func defaultRuntimeMounts() []runtimeMount {
	return []runtimeMount{
		{Destination: "/proc", Type: "proc", Source: "proc"},
		{
			Destination: "/dev", Type: mountTypeTmpfs, Source: mountTypeTmpfs,
			Options: []string{mountOptNosuid, "strictatime", "mode=755", "size=65536k"},
		},
		// /dev/pts gives the container a private devpts instance so
		// processes can allocate PTYs by opening /dev/ptmx — which
		// the runtime symlinks to pts/ptmx once this is mounted.
		// Without it /dev is a bare tmpfs with no ptmx node, and any
		// PTY allocation fails with ENOENT ("opening pseudoterminal
		// master: No such file or directory"). `nix develop
		// --command` hits exactly that when it spawns its build
		// shell. newinstance + ptmxmode=0666 keep it self-contained
		// and openable from the container's (possibly unprivileged,
		// userns-mapped) uid; gid=5 is omitted deliberately since
		// the tty group need not exist in the container's gid map.
		{
			Destination: "/dev/pts", Type: mountTypeDevpts, Source: mountTypeDevpts,
			Options: []string{mountOptNosuid, "noexec", "newinstance", "ptmxmode=0666", "mode=0620"},
		},
		{
			Destination: "/sys", Type: "sysfs", Source: "sysfs",
			Options: []string{mountOptNosuid, "noexec", mountOptNodev, "ro"},
		},
		{
			Destination: "/tmp", Type: mountTypeTmpfs, Source: mountTypeTmpfs,
			Options: []string{mountOptNosuid, mountOptNodev, "mode=1777", "size=268435456"},
		},
		// /nix/store/.links is a tmpfs because nix's LocalStore
		// unconditionally mkdirs this on first run, and the
		// virtiofs-served /nix/store can't be modified by the
		// container (the daemon executes as its own host uid).
		// Empty tmpfs is a no-op for non-nix images.
		{
			Destination: "/nix/store/.links", Type: mountTypeTmpfs, Source: mountTypeTmpfs,
			Options: []string{mountOptNosuid, mountOptNodev, "mode=0755", "size=16777216"},
		},
	}
}

// writeJSON marshals v to path, atomically (write-and-rename).
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("imagestore: marshal %s: %w", path, err)
	}
	// Skip the rewrite when the on-disk bytes already match: callers
	// re-Bundle the same image+opts and rely on config.json staying
	// untouched (TestBundleIsIdempotent), so an unconditional
	// write-and-rename would needlessly bump mtime.
	// #nosec G304 — path is an internal, caller-constructed bundle path.
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, b) {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
