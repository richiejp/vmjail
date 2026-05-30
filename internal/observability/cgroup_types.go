package observability

// CgroupSample is the per-tick projection of the
// {cpu,memory,io,pids}.* files under the workload's cgroup v2
// directory. All fields are optional — a cgroup whose controllers
// are not enabled in cgroup.subtree_control of its parent will
// have only `cpu.stat` and `cgroup.*` files; memory.current,
// io.stat etc. won't exist and the corresponding Sample sub-struct
// is left zero-valued.
//
// Lives in its own non-platform-tagged file so the JSON schema is
// visible on every build target (Linux for runtime, macOS/etc. for
// developer unit-test builds). The collection side
// (collectCgroupSample, parseCgroup*) is in cgroup_linux.go since
// it reads /sys/fs/cgroup directly.
type CgroupSample struct {
	Path   string        `json:"path"`
	CPU    CgroupCPU     `json:"cpu,omitzero"`
	Memory CgroupMemory  `json:"memory,omitzero"`
	IO     []CgroupIODev `json:"io,omitempty"`
	Pids   CgroupPids    `json:"pids,omitzero"`
}

// CgroupCPU mirrors cpu.stat fields. Values are cumulative
// microseconds and counts; deltas reconstructed in post.
type CgroupCPU struct {
	UsageUsec     uint64 `json:"usage_usec,omitempty"`
	UserUsec      uint64 `json:"user_usec,omitempty"`
	SystemUsec    uint64 `json:"system_usec,omitempty"`
	NRPeriods     uint64 `json:"nr_periods,omitempty"`
	NRThrottled   uint64 `json:"nr_throttled,omitempty"`
	ThrottledUsec uint64 `json:"throttled_usec,omitempty"`
}

// CgroupMemory mirrors memory.{current,peak,max,swap.current}.
// Max is a signed int because the file holds the literal "max"
// when no limit is set — we encode that as -1 so the JSON consumer
// doesn't have to special-case a string.
type CgroupMemory struct {
	CurrentBytes     uint64 `json:"current_bytes,omitempty"`
	PeakBytes        uint64 `json:"peak_bytes,omitempty"`
	MaxBytes         int64  `json:"max_bytes,omitempty"`
	SwapCurrentBytes uint64 `json:"swap_current_bytes,omitempty"`
}

// CgroupIODev is one device-keyed io.stat line.
type CgroupIODev struct {
	Device string `json:"device"`
	RBytes uint64 `json:"rbytes,omitempty"`
	WBytes uint64 `json:"wbytes,omitempty"`
	RIOs   uint64 `json:"rios,omitempty"`
	WIOs   uint64 `json:"wios,omitempty"`
	DBytes uint64 `json:"dbytes,omitempty"`
	DIOs   uint64 `json:"dios,omitempty"`
}

// CgroupPids mirrors pids.current. Per-cgroup PID counter.
type CgroupPids struct {
	Current uint64 `json:"current,omitempty"`
}
