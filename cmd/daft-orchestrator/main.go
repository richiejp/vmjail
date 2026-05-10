// Command daft-orchestrator is the central control-plane for daft.
// It authenticates clients and sandbox-hosts via mTLS (TOFU peer
// approvals from internal/identity) and serves Connect-RPC over
// HTTP/2. See internal/orchestratorcli for the command tree and
// internal/orchestrator for the server itself.
package main

import (
	"fmt"
	"os"

	"github.com/richiejp/darkaifactory/internal/orchestratorcli"
)

func main() {
	if err := orchestratorcli.Execute(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "daft-orchestrator:", err)
		os.Exit(1)
	}
}
