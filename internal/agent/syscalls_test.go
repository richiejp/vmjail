package agent

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
)

// Tracefs syscall-events entry names reused across the table cases.
const (
	tpEnterRead = "sys_enter_read"
	tpEnable    = "enable" // a non-syscall control file in events/syscalls
)

// capLog is a capturing trace file: Write appends to buf, Close is a no-op.
type capLog struct{ buf *bytes.Buffer }

func (c capLog) Write(p []byte) (int, error) { return c.buf.Write(p) }
func (c capLog) Close() error                { return nil }

// errWriteLog is a trace file whose Write always fails, to drive the
// dump's write-error branch.
type errWriteLog struct{}

func (errWriteLog) Write([]byte) (int, error) { return 0, errors.New("disk full") }
func (errWriteLog) Close() error              { return nil }

func TestSyscallNamesFromEntries(t *testing.T) {
	// A realistic mix: sys_enter_/sys_exit_ pairs, an out-of-order pair, a
	// duplicate enter, and non-syscall control entries that must be ignored.
	entries := []string{
		"sys_enter_write", "sys_exit_write",
		tpEnterRead, "sys_exit_read",
		"sys_enter_openat", "sys_exit_openat",
		tpEnterRead, // duplicate
		tpEnable, "filter",
	}
	got := syscallNamesFromEntries(entries)
	want := []string{"openat", "read", "write"} // sorted, deduped, exit/control dropped
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("syscallNamesFromEntries = %v, want %v", got, want)
	}
}

func TestSyscallNamesFromEntriesEmpty(t *testing.T) {
	if got := syscallNamesFromEntries([]string{tpEnable, "filter"}); len(got) != 0 {
		t.Fatalf("want no syscalls from control-only entries, got %v", got)
	}
}

func syscallTestDeps(list func() ([]string, error), openLog func(string) (io.WriteCloser, error)) runDeps {
	return runDeps{syscalls: syscallDumpDeps{
		mountTracefs: func() error { return nil },
		listEnter:    list,
		openLog:      openLog,
	}}
}

// syscallCfg builds a runDeps cfg carrying the given syscalls observe path.
func syscallCfg(path string) *agentv1.GuestConfig {
	return &agentv1.GuestConfig{Observe: &agentv1.GuestConfig_Observe{Syscalls: path}}
}

func TestStartSyscallDumpDisabledWhenListNil(t *testing.T) {
	var console bytes.Buffer
	startSyscallDump(&console, runDeps{
		cfg:      syscallCfg("/share/syscalls.txt"),
		syscalls: syscallDumpDeps{listEnter: nil},
	})
	if console.Len() != 0 {
		t.Fatalf("disabled dump produced console output: %q", console.String())
	}
}

func TestStartSyscallDumpAbsentToken(t *testing.T) {
	called := false
	d := syscallTestDeps(func() ([]string, error) { called = true; return nil, nil },
		func(string) (io.WriteCloser, error) { return capLog{&bytes.Buffer{}}, nil })
	var console bytes.Buffer
	startSyscallDump(&console, d)
	if called || console.Len() != 0 {
		t.Fatalf("absent token did work: called=%v console=%q", called, console.String())
	}
}

// TestStartSyscallDumpReportsWriteError covers the io.WriteString failure
// arm: the list is gathered and the log opened, but writing the syscall
// names out fails, which is reported (and the dump aborts).
func TestStartSyscallDumpReportsWriteError(t *testing.T) {
	d := syscallTestDeps(
		func() ([]string, error) { return []string{tpEnterRead}, nil },
		func(string) (io.WriteCloser, error) { return errWriteLog{}, nil },
	)
	d.cfg = syscallCfg("/share/syscalls.txt")
	var console bytes.Buffer
	startSyscallDump(&console, d)
	if !strings.Contains(console.String(), SyscallsReportPrefix+"disk full") {
		t.Fatalf("missing write-error report: %q", console.String())
	}
}

func TestStartSyscallDumpReportsTracefsError(t *testing.T) {
	d := runDeps{
		cfg: syscallCfg("/share/syscalls.txt"),
		syscalls: syscallDumpDeps{
			mountTracefs: func() error { return errors.New("boom-tracefs") },
			listEnter:    func() ([]string, error) { return nil, nil },
			openLog:      func(string) (io.WriteCloser, error) { return capLog{&bytes.Buffer{}}, nil },
		},
	}
	var console bytes.Buffer
	startSyscallDump(&console, d)
	if !strings.Contains(console.String(), SyscallsReportPrefix+"boom-tracefs") {
		t.Fatalf("missing tracefs error report: %q", console.String())
	}
}

func TestStartSyscallDumpReportsListError(t *testing.T) {
	d := syscallTestDeps(func() ([]string, error) { return nil, errors.New("boom-list") },
		func(string) (io.WriteCloser, error) { return capLog{&bytes.Buffer{}}, nil })
	d.cfg = syscallCfg("/share/syscalls.txt")
	var console bytes.Buffer
	startSyscallDump(&console, d)
	if !strings.Contains(console.String(), SyscallsReportPrefix+"boom-list") {
		t.Fatalf("missing list error report: %q", console.String())
	}
}

func TestStartSyscallDumpReportsOpenError(t *testing.T) {
	d := syscallTestDeps(func() ([]string, error) { return []string{tpEnterRead}, nil },
		func(string) (io.WriteCloser, error) { return nil, errors.New("boom-open") })
	d.cfg = syscallCfg("/share/syscalls.txt")
	var console bytes.Buffer
	startSyscallDump(&console, d)
	if !strings.Contains(console.String(), SyscallsReportPrefix+"boom-open") {
		t.Fatalf("missing open error report: %q", console.String())
	}
}

func TestStartSyscallDumpHappyPath(t *testing.T) {
	var written bytes.Buffer
	var opened string
	d := syscallTestDeps(
		func() ([]string, error) {
			return []string{"sys_enter_write", "sys_exit_write", tpEnterRead, tpEnable}, nil
		},
		func(path string) (io.WriteCloser, error) { opened = path; return capLog{&written}, nil },
	)
	d.cfg = syscallCfg("/share/syscalls.txt")
	var console bytes.Buffer
	startSyscallDump(&console, d)
	if opened != "/share/syscalls.txt" {
		t.Fatalf("openLog path = %q", opened)
	}
	if written.String() != "read\nwrite\n" {
		t.Fatalf("written = %q, want \"read\\nwrite\\n\"", written.String())
	}
	if !strings.Contains(console.String(), SyscallsReportPrefix+"wrote 2 available syscalls → /share/syscalls.txt") {
		t.Fatalf("missing wrote report: %q", console.String())
	}
}
