// Command daft-sandboxd is the single-executable sandbox-host. It
// embeds the VMM, the guest kernel, and the sandbox initramfs; extracts
// them to the user cache directory on first run; and boots sandbox VMs
// from them. See internal/sandboxdcli for the command tree.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/localai-org/darkaifactory/internal/cgroupwrap"
	"github.com/localai-org/darkaifactory/internal/sandboxdcli"
	"github.com/localai-org/darkaifactory/internal/userns"
)

//nolint:gochecknoglobals // test seams.
var (
	osExit       = os.Exit
	usernsReexec = userns.Reexec
	cgroupReexec = cgroupwrap.EnsureDelegated
)

func main() {
	// Guarantee a delegated cgroup v2 subtree BEFORE anything else: if
	// our inherited cgroup isn't delegated, re-exec under a systemd-run
	// scope (which hands back a delegated subtree) so the per-VM
	// accounting cgroups every boot requires can be created. This must
	// precede the userns re-exec below so the systemd-run wrap is the
	// outermost layer and owns the scope for our whole lifetime. On the
	// wrap path this never returns; a host that can't be made to
	// delegate is a hard startup failure, not a /proc-only degrade (see
	// internal/cgroupwrap + CONTRIBUTING "Probe preconditions at
	// startup").
	if err := cgroupReexec(os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "daft-sandboxd: cgroup:", err)
		osExit(1)
		return
	}

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
