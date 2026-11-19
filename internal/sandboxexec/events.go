package sandboxexec

import sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"

// LiveSandbox is a local snapshot of an executor workload.
type LiveSandbox struct {
	UID      string
	Phase    string
	ExitCode int32
	Message  string
}

// SandboxWatchEvent requests a local executor lifecycle transition.
// It retains the existing dispatch interface while the foreground CLI is wired.
type SandboxWatchEvent struct {
	Type    sandboxv1.EventType
	Sandbox *sandboxv1.Sandbox
}
