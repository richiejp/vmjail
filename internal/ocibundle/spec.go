// Package ocibundle describes the config.json fields generated for crun.
package ocibundle

const Version = "1.0.2"

// Spec is the subset of the OCI runtime spec we generate. It
// is intentionally incomplete — only fields the agent's `crun run
// --no-pivot` cares about, mirroring contrib/hello-bundle/config.json.
//
// Marshalled as-is to config.json. JSON tags use the OCI spec field
// names so the result is a valid runtime-spec document.
type Spec struct {
	OCIVersion string  `json:"ociVersion"`
	Process    Process `json:"process"`
	Root       Root    `json:"root"`
	Hostname   string  `json:"hostname"`
	Mounts     []Mount `json:"mounts"`
	Linux      Linux   `json:"linux"`
}

type Process struct {
	Terminal        bool          `json:"terminal"`
	User            User          `json:"user"`
	Args            []string      `json:"args"`
	Env             []string      `json:"env,omitempty"`
	Cwd             string        `json:"cwd"`
	Capabilities    *Capabilities `json:"capabilities,omitempty"`
	Rlimits         []Rlimit      `json:"rlimits"`
	NoNewPrivileges bool          `json:"noNewPrivileges"`
}

// Rlimit sets the soft and hard limits for one process resource.
type Rlimit struct {
	Type string `json:"type"`
	Soft uint64 `json:"soft"`
	Hard uint64 `json:"hard"`
}

type User struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

type Capabilities struct {
	Bounding  []string `json:"bounding"`
	Effective []string `json:"effective"`
	Permitted []string `json:"permitted"`
	Ambient   []string `json:"ambient"`
}

type Root struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type Mount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Options     []string `json:"options,omitempty"`
}

type Linux struct {
	Namespaces    []Namespace `json:"namespaces"`
	Resources     *Resources  `json:"resources,omitempty"`
	Devices       []Device    `json:"devices,omitempty"`
	Seccomp       *Seccomp    `json:"seccomp,omitempty"`
	MaskedPaths   []string    `json:"maskedPaths,omitempty"`
	ReadonlyPaths []string    `json:"readonlyPaths,omitempty"`
}

// Device is one OCI `linux.devices` entry: it tells the runtime
// to mknod the device node INSIDE the container (in its /dev tmpfs)
// with the given FileMode/UID/GID, rather than bind-mounting the host's
// node. Used to expose /dev/kvm with a 0666 FileMode so a non-root
// container can open it — the VM's own node is 0600 root:root (no udev
// in the initramfs) and a bind would preserve that. FileMode/UID/GID
// are pointers so the OCI default (inherit) is expressed by omission.
type Device struct {
	Type     string  `json:"type"`
	Path     string  `json:"path"`
	Major    int64   `json:"major"`
	Minor    int64   `json:"minor"`
	FileMode *uint32 `json:"fileMode,omitempty"`
	UID      *uint32 `json:"uid,omitempty"`
	GID      *uint32 `json:"gid,omitempty"`
}

// Seccomp is the container's syscall filter. v0 uses it for one
// narrow job: deny socket(AF_VSOCK) so a confused workload can't open
// its own vsock channel to the host. DefaultAction is ALLOW — this is
// an accidental-breakage guard, not a security sandbox (the VM is the
// boundary). See PLAN.md "accidental-clobber surface".
type Seccomp struct {
	DefaultAction string           `json:"defaultAction"`
	Architectures []string         `json:"architectures,omitempty"`
	Syscalls      []SeccompSyscall `json:"syscalls,omitempty"`
}

// SeccompSyscall is one rule: when any name in Names is called
// and every Args predicate matches, take Action (returning ErrnoRet
// for SCMP_ACT_ERRNO).
type SeccompSyscall struct {
	Names    []string     `json:"names"`
	Action   string       `json:"action"`
	ErrnoRet *uint        `json:"errnoRet,omitempty"`
	Args     []SeccompArg `json:"args,omitempty"`
}

// SeccompArg matches one syscall argument: arg[Index] Op Value.
type SeccompArg struct {
	Index uint64 `json:"index"`
	Value uint64 `json:"value"`
	Op    string `json:"op"`
}

// Resources carries the cgroup resource limits. We only populate
// the device controller, to let the container open the bind-mounted
// /dev/kvm — crun's default cgroup-v2 eBPF device filter denies anything
// outside the standard set, so a bind mount alone is not enough.
type Resources struct {
	Devices []DeviceCgroup `json:"devices,omitempty"`
}

// DeviceCgroup is one cgroup device rule. Type/Major/Minor are
// omitted to mean "all" per the OCI spec.
type DeviceCgroup struct {
	Allow  bool   `json:"allow"`
	Type   string `json:"type,omitempty"`
	Major  *int64 `json:"major,omitempty"`
	Minor  *int64 `json:"minor,omitempty"`
	Access string `json:"access,omitempty"`
}

type Namespace struct {
	Type string `json:"type"`
}
