package agent

import (
	"fmt"
	"io"
	"sort"
	"strings"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
)

// SyscallsReportPrefix labels the boot-time syscall-dump's console status line.
const SyscallsReportPrefix = "syscalls: "

// syscallDumpDeps is the test-substitutable surface for the boot-time
// "what syscalls does this guest kernel expose" dump. At every boot the agent
// enumerates the kernel's generated syscall tracepoints and writes the sorted
// list to the sandbox's observe state folder, so a user can always discover
// what is traceable inside the microvm without booting it interactively. It is
// not a gadget — it loads no BPF, runs once, and never blocks the boot.
//
// A zero value (listEnter == nil) disables it, so unit tests of runWith that
// don't wire it skip the dump. Off Linux it stays disabled (the stub).
type syscallDumpDeps struct {
	// mountTracefs ensures tracefs is mounted so events/syscalls is readable
	// (shared with the gadgets — an already-mounted tracefs is success).
	mountTracefs func() error
	// listEnter returns the entry names under tracefs events/syscalls (a mix of
	// sys_enter_<name> / sys_exit_<name> dirs plus a couple of non-syscall
	// control files); syscallNamesFromEntries distils the syscall set from it.
	listEnter func() ([]string, error)
	// openLog creates/truncates the destination file at the guest path.
	openLog func(path string) (io.WriteCloser, error)
}

// startSyscallDump resolves the syscall-list path from the GuestConfig's
// observe.syscalls and, when set,
// enumerates the kernel's syscall tracepoints and writes the sorted list to the
// given guest path (on the observe share, so the host can read it back). Like
// the gadget start helpers it reports one line per outcome and never fails the
// boot — it is a diagnostic, not a correctness invariant. One-shot: no
// goroutine, no BPF.
func startSyscallDump(w io.Writer, d runDeps) {
	if d.syscalls.listEnter == nil {
		return
	}
	path, ok := resolveObservePath(d.cfg, (*agentv1.GuestConfig_Observe).GetSyscalls)
	if !ok {
		return
	}
	if err := d.syscalls.mountTracefs(); err != nil {
		writeReport(w, SyscallsReportPrefix, err.Error())
		return
	}
	entries, err := d.syscalls.listEnter()
	if err != nil {
		writeReport(w, SyscallsReportPrefix, err.Error())
		return
	}
	names := syscallNamesFromEntries(entries)
	f, err := d.syscalls.openLog(path)
	if err != nil {
		writeReport(w, SyscallsReportPrefix, err.Error())
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := io.WriteString(f, strings.Join(names, "\n")+"\n"); err != nil {
		writeReport(w, SyscallsReportPrefix, err.Error())
		return
	}
	writeReport(w, SyscallsReportPrefix, fmt.Sprintf("wrote %d available syscalls → %s", len(names), path))
}

// syscallNamesFromEntries distils the available syscall names from the entry
// names under tracefs events/syscalls. Each syscall has a sys_enter_<name>
// directory (and a sys_exit_<name> one); we key off the enter form so each
// syscall appears once, strip the prefix, and return them sorted and
// deduplicated so the on-disk list is stable and diffable. Non-syscall entries
// (the dir's enable/filter control files) are ignored.
func syscallNamesFromEntries(entries []string) []string {
	const prefix = "sys_enter_"
	seen := make(map[string]struct{}, len(entries))
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name, ok := strings.CutPrefix(e, prefix)
		if !ok || name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
