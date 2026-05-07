// Command daft is the darkaifactory command-line entry point.
//
// All real logic lives in github.com/richiejp/darkaifactory/internal/cli;
// main.go exists only to wire that package to the process's stdio and exit
// status. Keeping main thin makes the CLI surface testable without
// shelling out to a built binary.
package main

import (
	"fmt"
	"os"

	"github.com/richiejp/darkaifactory/internal/cli"
)

func main() {
	if err := cli.Execute(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "daft:", err)
		os.Exit(1)
	}
}
