// Command daft is the darkaifactory command-line entry point.
//
// All real logic lives in github.com/richiejp/darkaifactory/internal/cli;
// main.go exists only to wire that package to the process's stdio and exit
// status. Keeping main thin makes the CLI surface testable without
// shelling out to a built binary.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/richiejp/darkaifactory/internal/cli"
)

// osExit is the indirection point tests use to verify main()
// wiring without forking a subprocess.
var osExit = os.Exit

func main() {
	osExit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main() with os-level dependencies hoisted into arguments
// so it's straightforwardly unit-testable. Returns the exit code
// main() will hand to osExit.
func run(args []string, stdout, stderr io.Writer) int {
	if err := cli.Execute(args, stdout, stderr); err != nil {
		// If even reporting the error fails, the user has no
		// possible feedback channel — return a distinct exit
		// code (2) so callers can distinguish a CLI error from
		// a stderr write failure.
		if _, werr := fmt.Fprintln(stderr, "daft:", err); werr != nil {
			return 2
		}
		return 1
	}
	return 0
}
