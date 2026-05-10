// daft-hello-container is the workload binary in the v0 container
// bundle. It prints a single line and exits 0 — that's the smoke
// test for "we got a container running inside the sandbox VM".
//
// The agent's banner-scanner on the host watches for this exact
// string (see internal/agent.ContainerBanner), so don't change it
// without updating that constant.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/richiejp/darkaifactory/internal/agent"
)

var (
	osExit           = os.Exit
	stdout io.Writer = os.Stdout
)

func main() {
	osExit(run(stdout))
}

func run(out io.Writer) int {
	if _, err := fmt.Fprintln(out, agent.ContainerBanner); err != nil {
		return 1
	}
	return 0
}
