// Command daft-sandboxd is the single-executable sandbox-host. It
// embeds the VMM, the guest kernel, and the sandbox initramfs; extracts
// them to the user cache directory on first run; and boots sandbox VMs
// from them. See internal/sandboxdcli for the command tree.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/localai-org/darkaifactory/internal/sandboxdcli"
	"github.com/localai-org/darkaifactory/internal/userns"
)

//nolint:gochecknoglobals // test seams.
var (
	osExit       = os.Exit
	usernsReexec = userns.Reexec
)

func main() {
	// Re-exec ourselves inside a user namespace populated from
	// the calling user's /etc/subuid + /etc/subgid before any
	// sandboxd work begins. Past this point the process has
	// CAP_SYS_ADMIN, CAP_CHOWN and CAP_SETUID over its
	// subordinate id range, which is what lets imagestore
	// retarget bundle ownership and virtiofsd serve a multi-uid
	// view of those files. On the parent side of the re-exec
	// this call never returns — the parent waits on the child
	// and exits with the child's status — so any error here is
	// a setup failure we should treat as fatal.
	if err := usernsReexec(os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "daft-sandboxd: userns:", err)
		osExit(1)
		return
	}
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
