package imagestore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	Devices       []runtimeDevice    `json:"devices,omitempty"`
	Seccomp       *runtimeSeccomp    `json:"seccomp,omitempty"`
	MaskedPaths   []string           `json:"maskedPaths"`
	ReadonlyPaths []string           `json:"readonlyPaths"`
}

// runtimeDevice is one OCI `linux.devices` entry: it tells the runtime
// to mknod the device node INSIDE the container (in its /dev tmpfs)
// with the given FileMode/UID/GID, rather than bind-mounting the host's
// node. Used to expose /dev/kvm with a 0666 FileMode so a non-root
// container can open it — the VM's own node is 0600 root:root (no udev
// in the initramfs) and a bind would preserve that. FileMode/UID/GID
// are pointers so the OCI default (inherit) is expressed by omission.
type runtimeDevice struct {
	Type     string  `json:"type"`
	Path     string  `json:"path"`
	Major    int64   `json:"major"`
	Minor    int64   `json:"minor"`
	FileMode *uint32 `json:"fileMode,omitempty"`
	UID      *uint32 `json:"uid,omitempty"`
	GID      *uint32 `json:"gid,omitempty"`
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
func buildRuntimeConfig(img v1.Image, opts BundleOptions, rootfs string) (runtimeSpec, error) {
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

	user, err := resolveImageUser(imgCfg.User, rootfs)
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
// it has the runtime mknod /dev/kvm INSIDE the container (an OCI
// linux.devices entry) with a 0666 FileMode, and adds the 10:232
// cgroup device-allow so crun's default eBPF device filter permits the
// open. The two must travel together — the device without the allow
// EPERMs, the allow without the device has nothing to open.
//
// We mknod rather than bind-mount the VM's own /dev/kvm: that node is
// 0600 root:root (devtmpfs default — the initramfs has no udev to widen
// it), and a bind preserves the source mode, locking out any container
// that runs as a non-root user. mknod lets us set 0666 so the image's
// declared uid can open it; the node still routes to the same kvm
// driver by its 10:232 identity. Returns fresh slices so the
// unconditional defaults stay unprivileged for every non-nested
// workload.
func applyNestedVirt(mounts []runtimeMount, linux runtimeLinuxConfig) ([]runtimeMount, runtimeLinuxConfig) {
	kvmMode := uint32(0o666)
	linux.Devices = append(linux.Devices, runtimeDevice{
		Type: "c", Path: devKVMPath, Major: 10, Minor: 232, FileMode: &kvmMode,
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

// resolveImageUser projects the OCI image-config `User` field onto the
// runtime spec's numeric uid/gid pair, resolving symbolic names against the
// UNPACKED rootfs's /etc/passwd and /etc/group — never the host's, matching
// the Docker/OCI rule that a container username is meaningful relative to the
// image (every image ships its own passwd db, so the same `USER nonroot` is
// portable across hosts). Empty string → 0:0 (root), matching the historical
// pre-image behaviour. Pure-numeric forms need no rootfs lookup, so a
// `scratch` image with `USER 1000` still resolves with no /etc/passwd present.
//
// Supported forms (a uid or a name on either side of the colon):
//
//	"" | "uid" | "uid:gid" | "name" | "name:group" | "uid:group" | "name:gid"
//
// A numeric user with no group defaults the gid to 0 (unchanged from the
// numeric-only predecessor); a symbolic user with no group takes that user's
// primary gid from /etc/passwd. Errors (missing passwd/group, unknown name,
// malformed record) surface here, before any disk write.
func resolveImageUser(u, rootfs string) (runtimeUser, error) {
	if u == "" {
		return runtimeUser{UID: 0, GID: 0}, nil
	}
	userPart, groupPart, hasGroup := strings.Cut(u, ":")
	uid, primaryGID, err := resolveUserField(userPart, rootfs)
	if err != nil {
		return runtimeUser{}, err
	}
	gid := primaryGID
	if hasGroup {
		gid, err = resolveGroupField(groupPart, rootfs)
		if err != nil {
			return runtimeUser{}, err
		}
	}
	return runtimeUser{UID: uid, GID: gid}, nil
}

// resolveUserField turns the user side of a `User` string into a uid plus the
// primary gid to use when no group is given. Numeric → (n, 0). A name is
// looked up in <rootfs>/etc/passwd, yielding its uid and primary gid.
func resolveUserField(s, rootfs string) (uid, primaryGID int, err error) {
	if s == "" {
		return 0, 0, fmt.Errorf("imagestore: image User: empty uid")
	}
	if n, derr := strconv.Atoi(s); derr == nil {
		return n, 0, nil
	}
	// passwd record: name:passwd:uid:gid:gecos:home:shell
	rec, lerr := lookupColonRecord(filepath.Join(rootfs, "etc", "passwd"), s, 4)
	if lerr != nil {
		return 0, 0, fmt.Errorf("imagestore: image User %q: %w", s, lerr)
	}
	uid, err = strconv.Atoi(rec[2])
	if err != nil {
		return 0, 0, fmt.Errorf("imagestore: image User %q: passwd uid %q not numeric", s, rec[2])
	}
	primaryGID, err = strconv.Atoi(rec[3])
	if err != nil {
		return 0, 0, fmt.Errorf("imagestore: image User %q: passwd gid %q not numeric", s, rec[3])
	}
	return uid, primaryGID, nil
}

// resolveGroupField turns the group side of a `User` string into a gid.
// Numeric → n. A name is looked up in <rootfs>/etc/group.
func resolveGroupField(s, rootfs string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("imagestore: image User: empty group")
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	// group record: name:passwd:gid:members
	rec, err := lookupColonRecord(filepath.Join(rootfs, "etc", "group"), s, 3)
	if err != nil {
		return 0, fmt.Errorf("imagestore: image User group %q: %w", s, err)
	}
	gid, err := strconv.Atoi(rec[2])
	if err != nil {
		return 0, fmt.Errorf("imagestore: image User group %q: gid %q not numeric", s, rec[2])
	}
	return gid, nil
}

// lookupColonRecord scans a colon-separated database file in the
// /etc/passwd or /etc/group format for the record whose first field equals
// name, returning its split fields. minFields drops malformed short lines.
// Comments (#…) and blank lines are skipped.
func lookupColonRecord(path, name string, minFields int) ([]string, error) {
	// #nosec G304 — path is <rootfs>/etc/{passwd,group}; the rootfs is the
	// image's own unpacked tree, trusted to the same degree as its argv.
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("no %s in image (a symbolic User needs it)", filepath.Base(path))
		}
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < minFields {
			continue
		}
		if fields[0] == name {
			return fields, nil
		}
	}
	return nil, fmt.Errorf("name %q not found", name)
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
			{Type: mountTypeCgroup},
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
	mountOptNoexec  = "noexec"
	mountTypeTmpfs  = "tmpfs"
	mountTypeDevpts = "devpts"
	mountTypeBind   = "bind"
	mountOptBind    = "bind"
	mountTypeCgroup = "cgroup"
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
			Options: []string{mountOptNosuid, mountOptNoexec, "newinstance", "ptmxmode=0666", "mode=0620"},
		},
		{
			Destination: "/sys", Type: "sysfs", Source: "sysfs",
			Options: []string{mountOptNosuid, mountOptNoexec, mountOptNodev, "ro"},
		},
		// /sys/fs/cgroup is mounted rw as a cgroup-v2 hierarchy so a
		// workload can manage its own cgroup subtree from inside the VM —
		// most importantly a nested daft-sandboxd creating per-VM
		// accounting cgroups (createVMCgroup). Without it the container
		// sees only the read-only /sys sysfs, so /sys/fs/cgroup is an empty
		// dir and the mkdir ENOENTs. The mount is cgroupns-relative (the
		// container already gets a cgroup namespace), so crun exposes only
		// the container's own subtree — never the host's, and never the
		// guest's root hierarchy. Safe to delegate by default in a
		// single-tenant VM: the container is already namespace-root in its
		// own guest kernel, so a writable cgroup subtree grants nothing it
		// couldn't otherwise reach. (This is purely in-guest; it has no
		// dependence on whether the host process's cgroup is delegated —
		// the VM boundary separates the two hierarchies entirely.)
		{
			Destination: "/sys/fs/cgroup", Type: mountTypeCgroup, Source: mountTypeCgroup,
			Options: []string{mountOptNosuid, mountOptNoexec, mountOptNodev, "relatime", "rw"},
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
