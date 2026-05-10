// Command daft-sandboxd is the single-executable sandbox-host. It
// embeds the VMM, the guest kernel, and the sandbox initramfs; extracts
// them to the user cache directory on first run; and boots sandbox VMs
// from them. See internal/sandboxdcli for the command tree.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/richiejp/darkaifactory/internal/sandboxdcli"
)

var osExit = os.Exit

func main() {
	osExit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if err := sandboxdcli.Execute(args, stdout, stderr); err != nil {
		if _, werr := fmt.Fprintln(stderr, "daft-sandboxd:", err); werr != nil {
			return 2
		}
		return 1
	}
	return 0
}
