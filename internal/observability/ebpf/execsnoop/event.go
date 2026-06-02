package execsnoop

// Event is one decoded execve(2) observation: who exec'd (pid/ppid/uid +
// the caller's comm), the full argv, and the return value. It is the public
// shape the linux drain renders into the guest-side trace and folds into the
// Aggregator.
//
// It lives in a build-constraint-free file (unlike the BPF decode/attach
// path) so the pure Aggregator that consumes it can be built and tested
// cross-platform, and so the agent's cross-platform gadget wiring can name
// the type without pulling in cilium/ebpf.
type Event struct {
	// PID is the caller's tgid; PPID its parent's tgid (the process tree).
	PID  uint32
	PPID uint32
	UID  uint32
	// Ret is the execve(2) return value: 0 on success (the new image is
	// now running), or a negative errno on failure (e.g. -2 = -ENOENT, a
	// command that does not exist).
	Ret int32
	// Comm is the caller's command name at the moment of exec — the
	// program doing the exec, not the one being exec'd (that's Argv[0]).
	Comm string
	// Argv is the program path (Argv[0]) plus its arguments. Truncated is
	// set when the argv was longer than the gadget's capture cap, so the
	// tail is missing.
	Argv      []string
	Truncated bool
}
