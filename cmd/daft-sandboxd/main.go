// Command daft-sandboxd is the single-executable sandbox-host. It
// embeds the VMM, the guest kernel, and the sandbox initramfs; extracts
// them to the user cache directory on first run; and boots sandbox VMs
// from them. See internal/sandboxdcli for the command tree.
package main

import (
	"fmt"
	"os"

	"github.com/richiejp/darkaifactory/internal/sandboxdcli"
)

func main() {
	if err := sandboxdcli.Execute(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "daft-sandboxd:", err)
		os.Exit(1)
	}
}
