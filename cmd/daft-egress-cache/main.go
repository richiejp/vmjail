// Command daft-egress-cache packs/pushes and pulls/unpacks the e2e
// suite's recorded egress-proxy cache as a single-layer OCI image:
//
//	daft-egress-cache pack-push   <srcDir> <ref>   # publish (needs DAFT_GHCR_TOKEN)
//	daft-egress-cache pull-unpack <ref> <destDir>  # fetch (anonymous for a public ref)
//
// It's build-time tooling, driven by `make update-egress-cache`
// (publish) and `make integration-build` (fetch). All logic lives in
// internal/egresscache; this binary is the thin CLI shell.
package main

import (
	"fmt"
	"os"

	"github.com/richiejp/darkaifactory/internal/egresscache"
)

// Indirection points so the in-package test drives run() without forking
// or actually exiting — the project's testable-main convention (see
// cmd/daft-lint).
var (
	osExit   = os.Exit
	dispatch = egresscache.Dispatch
)

func main() { osExit(run(os.Args[1:])) }

// run dispatches the CLI and maps an error to exit code 1.
func run(args []string) int {
	if err := dispatch(args, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
