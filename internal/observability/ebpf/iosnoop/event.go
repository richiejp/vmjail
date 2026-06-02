package iosnoop

// Role bits mirror iosnoop_bpf.c: a fd touched only by zero-byte reads/writes
// (EOF, an empty socket) is still classified, so the trace can say "this fd
// was read from" even when no bytes moved.
const (
	roleRead  = 1
	roleWrite = 2
)

// FDStat is one decoded per-(pid,fd) accumulator from the BPF fdstats map: who
// (pid) used which descriptor (fd) and the bytes/calls/errors charged to it.
// It is the raw, target-unresolved observation; the linux drain resolves the
// (PID, FD) into a target name via /proc and folds it into the Aggregator.
//
// It lives in a build-constraint-free file (unlike the BPF map-iterate path)
// so the pure Aggregator that consumes it can be built and tested
// cross-platform, and so the agent's cross-platform gadget wiring can name the
// type without pulling in cilium/ebpf.
type FDStat struct {
	PID     uint32
	FD      int32
	RdBytes uint64
	WrBytes uint64
	Calls   uint64
	Errors  uint32
	Role    uint32
}

// Totals is the monotonic global counter decoded from the BPF totals map
// (summed across CPUs). It is the trustworthy Total / error source for the
// summary — unlike the per-fd map it is never evicted, so it does not dip when
// a cold fd's entry is reclaimed.
type Totals struct {
	Calls   uint64
	RdBytes uint64
	WrBytes uint64
	Errors  uint64
}

// roleString renders a role bitmask as "r", "w", "rw", or "-" (a fd recorded
// calls-only — connect/accept/vmsplice/mmsg — has no read/write bit).
func roleString(role uint32) string {
	switch role & (roleRead | roleWrite) {
	case roleRead:
		return "r"
	case roleWrite:
		return "w"
	case roleRead | roleWrite:
		return "rw"
	default:
		return "-"
	}
}
