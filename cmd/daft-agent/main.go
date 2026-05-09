// Command daft-agent is the in-VM sandbox agent. It is launched by the
// VM's init (u-root) via -uinitcmd and becomes the long-running supervisor
// for the workload container.
//
// All the lifecycle logic lives in github.com/richiejp/darkaifactory/internal/agent;
// main exists only to wire process stdio, signals, and exit status.
package main

import (
	"context"
	"os"
	"syscall"

	"github.com/richiejp/darkaifactory/internal/agent"
)

func main() {
	os.Exit(agent.Main(context.Background(), os.Stdout, os.Stderr, syscall.SIGINT, syscall.SIGTERM))
}
