// Command daft-agent is the in-VM sandbox agent. It is launched by the
// VM's init (u-root) via -uinitcmd and becomes the long-running supervisor
// for the workload container.
//
// All the lifecycle logic lives in github.com/localai-org/darkaifactory/internal/agent;
// main exists only to wire process stdio, signals, and exit status.
package main

import (
	"context"
	"os"
	"syscall"

	"github.com/localai-org/darkaifactory/internal/agent"
)

// osExit and agentMain are package-level indirection points so
// main_test.go can verify wiring without forking a subprocess and
// without actually running the agent's PID-1 lifecycle (which
// requires a microVM environment). Both are plain function values
// (no closure body) so there are no extra statements to cover.
var (
	osExit    = os.Exit
	agentMain = agent.Main
)

func main() {
	osExit(agentMain(context.Background(), os.Stdout, os.Stderr,
		syscall.SIGINT, syscall.SIGTERM))
}
