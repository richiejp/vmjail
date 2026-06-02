//go:build linux

package agent

import (
	"io"
	"os"
	"path/filepath"
)

// syscallEventsDir is the tracefs subtree the kernel populates with one
// sys_enter_<name> / sys_exit_<name> directory per syscall (generated because
// CONFIG_FTRACE_SYSCALLS=y). Listing it is the runtime source of truth for
// which syscalls this guest kernel actually exposes — exactly the set a
// tracepoint can attach to.
func syscallEventsDir() string {
	return filepath.Join(tracefsMountpoint, "events", "syscalls")
}

// listSyscallEntriesAt returns the entry names directly under dir. The testable
// core of the real listEnter (dir is a parameter so a test can point it at a
// temp dir of fake sys_enter_* entries instead of the root-only tracefs).
func listSyscallEntriesAt(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names, nil
}

// defaultSyscallDumpDeps wires the real boot-time dump: mount tracefs, read the
// syscall-events dir, and create/truncate the destination file.
func defaultSyscallDumpDeps() syscallDumpDeps {
	return syscallDumpDeps{
		mountTracefs: ensureTracefs,
		listEnter:    func() ([]string, error) { return listSyscallEntriesAt(syscallEventsDir()) },
		openLog: func(path string) (io.WriteCloser, error) {
			//nolint:gosec // G304: path is the host-operator-set daft.observe.syscalls cmdline value — same trust boundary as daft.bundle.
			return os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		},
	}
}
