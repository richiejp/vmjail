//go:build linux

package observability

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// collectProcSample reads /proc/<pid>/{stat,status,io} and returns
// a populated ProcSample. The three files are read in a single
// tick — they are not transactional with each other, but the
// counters they expose are cumulative so even a partial
// mid-update read still yields a monotonic series across samples.
//
// Errors from any individual file abort the whole sample and are
// returned to the caller. /proc/<pid>/io requires the same UID as
// the target or CAP_SYS_PTRACE; when neither holds, the read
// returns "permission denied" and the sample is recorded with an
// Err field by the Observer.
func collectProcSample(pid int) (*ProcSample, error) {
	out := &ProcSample{}
	if err := readProcStat(pid, out); err != nil {
		return nil, err
	}
	if err := readProcStatus(pid, out); err != nil {
		return nil, err
	}
	if err := readProcIO(pid, out); err != nil {
		return nil, err
	}
	return out, nil
}

// readProcStat parses /proc/<pid>/stat. The format is documented
// in proc(5); the field layout has been stable since 2.6.
func readProcStat(pid int, out *ProcSample) error {
	path := fmt.Sprintf("/proc/%d/stat", pid)
	// #nosec G304 — path is built from a caller-supplied pid; reading
	// /proc/<pid>/stat is the entire point of this function.
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return parseProcStat(b, out)
}

// parseProcStat is the bytes-in side of readProcStat, split out so
// tests can exercise malformed-input paths without writing fake
// /proc trees. comm (field 2) is in parentheses and may contain
// spaces, so we split on the last ')' rather than naive
// whitespace.
func parseProcStat(b []byte, out *ProcSample) error {
	s := string(b)
	commOpen := strings.IndexByte(s, '(')
	commClose := strings.LastIndexByte(s, ')')
	if commOpen < 0 || commClose < 0 || commClose < commOpen {
		return errors.New("observability: bad /proc/<pid>/stat: missing comm parens")
	}
	out.Comm = s[commOpen+1 : commClose]
	rest := strings.TrimSpace(s[commClose+1:])
	fields := strings.Fields(rest)
	// After ')', remaining fields per proc(5) start at field 3
	// (state). We want: state(0), utime(11), stime(12),
	// num_threads(17). Indices below are 0-based into `fields`,
	// not into the proc(5) numbering — proc(5) field N is at
	// index N-3.
	if len(fields) < 18 {
		return fmt.Errorf("observability: bad /proc/<pid>/stat: %d fields after comm", len(fields))
	}
	out.State = fields[0]
	u, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return fmt.Errorf("observability: parse utime: %w", err)
	}
	out.UtimeJiffies = u
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return fmt.Errorf("observability: parse stime: %w", err)
	}
	out.StimeJiffies = stime
	nthr, err := strconv.Atoi(fields[17])
	if err != nil {
		return fmt.Errorf("observability: parse num_threads: %w", err)
	}
	out.NumThreads = nthr
	return nil
}

// readProcStatus pulls VmRSS and VmSize out of /proc/<pid>/status.
// We don't use stat's rss field because it's in pages and would
// need PAGE_SIZE math; status reports kB directly.
func readProcStatus(pid int, out *ProcSample) error {
	path := fmt.Sprintf("/proc/%d/status", pid)
	// #nosec G304 — same justification as readProcStat.
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		// Best-effort close on a read-only fd; nothing we could
		// recover from. Matched against the project's
		// errcheck-via-comment convention (CONTRIBUTING "Handle
		// every error").
		_ = f.Close()
	}()
	return parseProcStatus(f, out)
}

// parseProcStatus is the io.Reader-driven half of readProcStatus,
// split for testing.
func parseProcStatus(r io.Reader, out *ProcSample) error {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "VmRSS:"):
			v, err := parseKBField(line)
			if err != nil {
				return fmt.Errorf("observability: parse VmRSS: %w", err)
			}
			out.VMRSSKB = v
		case strings.HasPrefix(line, "VmSize:"):
			v, err := parseKBField(line)
			if err != nil {
				return fmt.Errorf("observability: parse VmSize: %w", err)
			}
			out.VMSizeKB = v
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("observability: scan /proc/<pid>/status: %w", err)
	}
	return nil
}

// readProcIO parses /proc/<pid>/io into the byte/syscall counters.
// Each line is `<key>: <uint>`.
func readProcIO(pid int, out *ProcSample) error {
	path := fmt.Sprintf("/proc/%d/io", pid)
	// #nosec G304 — same justification as readProcStat.
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Close() // best-effort; read-only fd.
	}()
	return parseProcIO(f, out)
}

// parseProcIO is the io.Reader-driven half of readProcIO, split
// for testing.
func parseProcIO(r io.Reader, out *ProcSample) error {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val, err := strconv.ParseUint(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			return fmt.Errorf("observability: parse %s in /proc/<pid>/io: %w", key, err)
		}
		switch key {
		case "rchar":
			out.Rchar = val
		case "wchar":
			out.Wchar = val
		case "syscr":
			out.Syscr = val
		case "syscw":
			out.Syscw = val
		case "read_bytes":
			out.ReadBytes = val
		case "write_bytes":
			out.WriteBytes = val
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("observability: scan /proc/<pid>/io: %w", err)
	}
	return nil
}

// errReader returns err on first Read; used by tests to drive
// parseProcStatus / parseProcIO into their scan-error branches.
type errReader struct{ err error }

func (r errReader) Read(_ []byte) (int, error) { return 0, r.err }

// parseKBField extracts the integer from a line like
// "VmRSS:      12345 kB". status.c always emits a trailing " kB"
// so we strip both sides and parse the middle.
func parseKBField(line string) (uint64, error) {
	_, rest, ok := strings.Cut(line, ":")
	if !ok {
		return 0, fmt.Errorf("missing colon in %q", line)
	}
	rest = strings.TrimSpace(rest)
	rest = strings.TrimSuffix(rest, " kB")
	rest = strings.TrimSpace(rest)
	return strconv.ParseUint(rest, 10, 64)
}
