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
	"os"

	"github.com/richiejp/darkaifactory/internal/agent"
)

func main() {
	fmt.Println(agent.ContainerBanner)
	os.Exit(0)
}
