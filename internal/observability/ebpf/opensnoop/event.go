package opensnoop

// Event is one decoded openat(2) observation: who called it, the dirfd and
// flags, and the path. It is the public shape the linux drain renders into
// the guest-side observer trace and folds into the Aggregator.
//
// It lives in a build-constraint-free file (unlike the BPF decode/attach
// path) so the pure Aggregator that consumes it can be built and tested
// cross-platform, and so the agent's cross-platform gadget wiring can name
// the type without pulling in cilium/ebpf.
type Event struct {
	PID   uint32
	UID   uint32
	DFD   int32
	Flags int32
	// Ret is the openat(2) return value captured at sys_exit: a
	// non-negative file descriptor on success, or a negative errno (e.g.
	// -2 = -ENOENT) on failure — the cache-miss / missing-file signal.
	Ret      int32
	Comm     string
	Filename string
}
