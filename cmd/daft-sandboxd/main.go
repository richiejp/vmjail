// Command daft-sandboxd is the single-executable sandbox-host. It
// embeds the VMM, the guest kernel, and the sandbox initramfs; extracts
// them to the user cache directory on first run; and boots sandbox VMs
// from them. See internal/sandboxdcli for the command tree.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/richiejp/darkaifactory/internal/cgroupwrap"
	"github.com/richiejp/darkaifactory/internal/observability"
	"github.com/richiejp/darkaifactory/internal/sandbox"
	"github.com/richiejp/darkaifactory/internal/sandboxdcli"
	"github.com/richiejp/darkaifactory/internal/userns"
)

// envDisableCgroupLimits, when set to "1", turns the fail-closed per-VM
// cgroup-limit precondition into a warning so the host boots without enforced
// limits. The conscious opt-out for an environment that can't enable the
// controllers (e.g. a pre-delegated shared scope) — never the default.
const envDisableCgroupLimits = "DAFT_DISABLE_CGROUP_LIMITS"

//nolint:gochecknoglobals // test seams.
var (
	osExit       = os.Exit
	usernsReexec = userns.Reexec
	cgroupReexec = cgroupwrap.EnsureDelegated
	// prepareCgroupSubtree moves the sandbox-host into a leaf of its
	// delegated scope and enables the memory/cpu/io/pids controllers so
	// per-VM cgroups can carry limits + accounting, returning the scope
	// (the per-VM parent). A test seam so main()'s success path doesn't
	// touch the real cgroup tree.
	prepareCgroupSubtree = func() (string, error) {
		return observability.PrepareDelegatedSubtree(sandbox.SupervisorLeaf)
	}
)

func main() {
	// Identity bootstrap and release inspection must also work on a fresh host
	// whose service manager has not yet created a delegated cgroup/userns. They
	// never boot or manipulate a sandbox, so running the VM preconditions for
	// them would make installation impossible on non-systemd hosts.
	if isHostSetupCommand(os.Args[1:]) {
		osExit(run(os.Args[1:], os.Stdout, os.Stderr))
		return
	}

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

	// With a delegated scope in hand, enable the memory/cpu controllers on
	// it so per-VM cgroups can carry resource limits (and full accounting).
	// This MUST run while a single process owns the scope — before the
	// userns re-exec below forks a sibling into it — so it goes here,
	// between the two re-execs.
	//
	// Fail-closed: per-VM limits are a blast-radius cap, not an optimisation,
	// so a host that can't enforce them refuses to start rather than silently
	// boot unprotected. The cgroupwrap re-exec above already guarantees a
	// PRIVATE delegated scope, so the controller-enable here succeeds in every
	// supported deployment; a failure means an unusual environment (e.g. a
	// pre-delegated SHARED scope whose sibling processes block subtree_control)
	// where the operator must consciously opt out via DAFT_DISABLE_CGROUP_LIMITS.
	vmParent, err := prepareCgroupSubtree()
	if err != nil {
		if os.Getenv(envDisableCgroupLimits) != "1" {
			fmt.Fprintln(os.Stderr, "daft-sandboxd: per-VM cgroup limits cannot be enforced:", err)
			fmt.Fprintln(os.Stderr, "  set "+envDisableCgroupLimits+"=1 to boot without them (no per-VM blast-radius cap).")
			osExit(1)
			return
		}
		fmt.Fprintln(os.Stderr, "daft-sandboxd: per-VM cgroup limits disabled via "+envDisableCgroupLimits+":", err)
	}
	sandbox.SetVMCgroupParent(vmParent)

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

func isHostSetupCommand(args []string) bool {
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "auth", "version", "help", "--help", "-h":
		return true
	default:
		return false
	}
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
