// Command daft-orchestrator is the central control-plane for daft.
// It authenticates clients and sandbox-hosts via mTLS (TOFU peer
// approvals from internal/identity) and serves Connect-RPC over
// HTTP/2. See internal/orchestratorcli for the command tree and
// internal/orchestrator for the server itself.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/localai-org/darkaifactory/internal/orchestratorcli"
)

var osExit = os.Exit

func main() {
	osExit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if err := orchestratorcli.Execute(args, stdout, stderr); err != nil {
		if _, werr := fmt.Fprintln(stderr, "daft-orchestrator:", err); werr != nil {
			return 2
		}
		return 1
	}
	return 0
}
