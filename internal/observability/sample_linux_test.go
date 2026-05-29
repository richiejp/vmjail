//go:build linux

package observability

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestCollectProcSample_SelfPID(t *testing.T) {
	// Reading our own /proc entries is the cheapest deterministic
	// integration with the real kernel: format guaranteed by
	// proc(5), data nontrivially nonzero (we're alive), permission
	// always granted to the owner.
	s, err := collectProcSample(os.Getpid())
	if err != nil {
		t.Fatalf("collectProcSample(self): %v", err)
	}
	if s.Comm == "" {
		t.Error("Comm empty")
	}
	if s.State == "" {
		t.Error("State empty")
	}
	if s.NumThreads <= 0 {
		t.Errorf("NumThreads = %d, want > 0", s.NumThreads)
	}
	if s.VMSizeKB == 0 {
		t.Error("VMSizeKB = 0, want > 0 (this process is alive)")
	}
	// VmRSS can be 0 for some kernel-thread-like contexts but
	// never for our user-mode test process; assert positive.
	if s.VMRSSKB == 0 {
		t.Error("VMRSSKB = 0, want > 0")
	}
}

func TestCollectProcSample_NonexistentPIDReturnsNotExist(t *testing.T) {
	// PID 1 is init and might exist; PID 0 is invalid; pick a
	// PID well above the kernel's pid_max default of 4194304.
	_, err := collectProcSample(1 << 30)
	if err == nil {
		t.Fatal("want error for nonexistent PID, got nil")
	}
	if !strings.Contains(err.Error(), "no such file") &&
		!os.IsNotExist(err) {
		t.Errorf("err = %v, want a not-exist error", err)
	}
}

func TestParseProcStat_Good(t *testing.T) {
	// Real-ish /proc/<pid>/stat line. Fields after ")" per
	// proc(5): state utime stime ... at known indices. Padded
	// with zeros for any field we don't care about, sized to
	// exactly meet the 18-field minimum (state through
	// num_threads).
	in := []byte("1234 (cmd name) S 1 1 1 0 -1 0 0 0 0 0 4321 2107 0 0 20 0 8\n")
	var s ProcSample
	if err := parseProcStat(in, &s); err != nil {
		t.Fatalf("parseProcStat: %v", err)
	}
	if s.Comm != "cmd name" {
		t.Errorf("Comm = %q, want %q", s.Comm, "cmd name")
	}
	if s.State != "S" {
		t.Errorf("State = %q, want %q", s.State, "S")
	}
	if s.UtimeJiffies != 4321 {
		t.Errorf("UtimeJiffies = %d, want 4321", s.UtimeJiffies)
	}
	if s.StimeJiffies != 2107 {
		t.Errorf("StimeJiffies = %d, want 2107", s.StimeJiffies)
	}
	if s.NumThreads != 8 {
		t.Errorf("NumThreads = %d, want 8", s.NumThreads)
	}
}

func TestParseProcStat_CommWithEmbeddedParens(t *testing.T) {
	// LastIndexByte requirement: comm "weird) name" — the inner
	// ')' must not terminate comm. Real-world: rust binaries
	// sometimes have parens in their comm.
	in := []byte("1 (weird) name) R 1 1 1 0 -1 0 0 0 0 0 1 2 0 0 20 0 3\n")
	var s ProcSample
	if err := parseProcStat(in, &s); err != nil {
		t.Fatalf("parseProcStat: %v", err)
	}
	if s.Comm != "weird) name" {
		t.Errorf("Comm = %q, want %q", s.Comm, "weird) name")
	}
}

func TestParseProcStat_Malformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"no parens", "1 no-parens-here R 1 1 1 0 -1 0 0 0 0 0 1 2 0 0 20 0 3"},
		{"missing fields", "1 (cmd) R 1 2 3"},
		{"bad utime", "1 (cmd) R 1 1 1 0 -1 0 0 0 0 0 NaN 2 0 0 20 0 3"},
		{"bad stime", "1 (cmd) R 1 1 1 0 -1 0 0 0 0 0 1 BAD 0 0 20 0 3"},
		{"bad nthr", "1 (cmd) R 1 1 1 0 -1 0 0 0 0 0 1 2 0 0 20 0 BAD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s ProcSample
			err := parseProcStat([]byte(tc.in), &s)
			if err == nil {
				t.Fatalf("parseProcStat(%q): want error, got nil", tc.in)
			}
		})
	}
}

func TestParseProcStatus(t *testing.T) {
	// Realistic /proc/<pid>/status snippet; only VmRSS and
	// VmSize are extracted, other lines must be ignored cleanly.
	in := strings.NewReader(`Name:	cloud-hyperviso
State:	S (sleeping)
Pid:	1234
VmSize:	  1048576 kB
VmRSS:	   268432 kB
Other:	garbage
`)
	var s ProcSample
	if err := parseProcStatus(in, &s); err != nil {
		t.Fatalf("parseProcStatus: %v", err)
	}
	if s.VMRSSKB != 268432 {
		t.Errorf("VMRSSKB = %d, want 268432", s.VMRSSKB)
	}
	if s.VMSizeKB != 1048576 {
		t.Errorf("VMSizeKB = %d, want 1048576", s.VMSizeKB)
	}
}

func TestParseProcStatus_BadField(t *testing.T) {
	in := strings.NewReader("VmRSS: garbage kB\n")
	var s ProcSample
	if err := parseProcStatus(in, &s); err == nil {
		t.Fatal("want error on garbage VmRSS, got nil")
	}
	in2 := strings.NewReader("VmSize: garbage kB\n")
	if err := parseProcStatus(in2, &s); err == nil {
		t.Fatal("want error on garbage VmSize, got nil")
	}
}

func TestParseProcStatus_ScanError(t *testing.T) {
	r := errReader{err: errors.New("synthetic read failure")}
	var s ProcSample
	err := parseProcStatus(r, &s)
	if err == nil {
		t.Fatal("want error from scanner, got nil")
	}
	if !strings.Contains(err.Error(), "scan") {
		t.Errorf("err = %v, want scan-prefixed", err)
	}
}

func TestParseProcIO(t *testing.T) {
	in := strings.NewReader(`rchar: 100
wchar: 200
syscr: 30
syscw: 40
read_bytes: 5
write_bytes: 6
cancelled_write_bytes: 7
no-colon-line
`)
	var s ProcSample
	if err := parseProcIO(in, &s); err != nil {
		t.Fatalf("parseProcIO: %v", err)
	}
	want := ProcSample{
		Rchar: 100, Wchar: 200,
		Syscr: 30, Syscw: 40,
		ReadBytes: 5, WriteBytes: 6,
	}
	if s != want {
		t.Errorf("got %+v, want %+v", s, want)
	}
}

func TestParseProcIO_BadField(t *testing.T) {
	in := strings.NewReader("rchar: garbage\n")
	var s ProcSample
	if err := parseProcIO(in, &s); err == nil {
		t.Fatal("want error on garbage rchar, got nil")
	}
}

func TestParseProcIO_ScanError(t *testing.T) {
	r := errReader{err: errors.New("synthetic read failure")}
	var s ProcSample
	err := parseProcIO(r, &s)
	if err == nil {
		t.Fatal("want error from scanner, got nil")
	}
}

func TestParseKBField(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"VmRSS:     12345 kB", 12345, true},
		{"VmSize:\t99 kB", 99, true},
		{"NoColon 1 kB", 0, false},
		{"VmRSS: notanumber kB", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseKBField(tc.in)
			if tc.ok && err != nil {
				t.Fatalf("parseKBField(%q): %v", tc.in, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("parseKBField(%q): want error, got nil", tc.in)
			}
			if got != tc.want {
				t.Errorf("parseKBField(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
