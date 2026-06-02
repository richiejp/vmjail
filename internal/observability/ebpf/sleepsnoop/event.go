package sleepsnoop

// Event is one decoded sleep observation: who slept (pid + comm) and the
// requested duration. It is the public shape the linux drain renders into the
// guest-side trace and folds into the Aggregator.
//
// It lives in a build-constraint-free file (unlike the BPF decode/attach
// path) so the pure Aggregator that consumes it can be built and tested
// cross-platform, and so the agent's cross-platform gadget wiring can name the
// type without pulling in cilium/ebpf.
type Event struct {
	// PID is the caller's tgid. The raw trace carries it for drill-down; the
	// aggregator ranks by Comm, since PIDs are ephemeral (a poll loop is often
	// many short-lived "sh" processes) while the comm names the culprit.
	PID uint32
	// Comm is the caller's command name.
	Comm string
	// ReqNs is the requested sleep duration in nanoseconds, or 0 when it is
	// not a meaningful duration — a clock_nanosleep(TIMER_ABSTIME) sleeps
	// until an absolute deadline, which the gadget records as 0 rather than a
	// garbage wall-clock value.
	ReqNs uint64
}
