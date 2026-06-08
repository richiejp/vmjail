//go:build linux

package observability

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func closeFD(fd int) error { return syscall.Close(fd) }

// fakeSessionPath is repeated across parse tests; lifted to a
// const to satisfy goconst without dispersing the literal across
// the file.
const fakeSessionPath = "/user.slice/user-1000.slice/session-12.scope"

// errInvalidCgroupName is the substring CreateSubCgroup reports
// for empty / slash / nul names. Lifted out to satisfy goconst.
const errInvalidCgroupName = "invalid cgroup name"

// Cgroup v2 file names referenced from multiple tests.
const (
	cpuStatFile       = "cpu.stat"
	memoryCurrentFile = "memory.current"
	memoryPeakFile    = "memory.peak"
	memoryMaxFile     = "memory.max"
	memorySwapFile    = "memory.swap.current"
	ioStatFile        = "io.stat"
	pidsCurrentFile   = "pids.current"
)

func TestParseSelfCgroup_Good(t *testing.T) {
	in := []byte("0::" + fakeSessionPath + "\n")
	got, err := parseSelfCgroup(in)
	if err != nil {
		t.Fatalf("parseSelfCgroup: %v", err)
	}
	want := cgroupV2Root + fakeSessionPath
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseSelfCgroup_Hybrid(t *testing.T) {
	// Hybrid v1+v2 system — only v2-style 0:: lines parse,
	// and an absent 0:: line yields errNoCgroupV2.
	in := []byte("1:cpu:/user.slice\n2:memory:/user.slice\n")
	_, err := parseSelfCgroup(in)
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestParseSelfCgroup_Bad(t *testing.T) {
	cases := []string{
		"0::\n",
		"0::relative\n",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if _, err := parseSelfCgroup([]byte(in)); err == nil {
				t.Fatalf("want error for %q, got nil", in)
			}
		})
	}
}

func TestCurrentCgroup_OnRealHost(t *testing.T) {
	// Cheapest integration with the running kernel: our own
	// cgroup. We don't assert a specific path — just that the
	// result is under /sys/fs/cgroup and the directory exists.
	got, err := CurrentCgroup()
	if err != nil {
		t.Fatalf("CurrentCgroup: %v", err)
	}
	if !strings.HasPrefix(got, cgroupV2Root) {
		t.Errorf("CurrentCgroup() = %q, want prefix %q", got, cgroupV2Root)
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("stat %s: %v", got, err)
	}
}

func TestCreateSubCgroup_Validation(t *testing.T) {
	cases := []struct {
		name    string
		parent  string
		sub     string
		wantErr string
	}{
		{"bad parent", "/tmp", "x", "must be under"},
		{"empty name", cgroupV2Root, "", errInvalidCgroupName},
		{"slash in name", cgroupV2Root, "a/b", errInvalidCgroupName},
		{"nul in name", cgroupV2Root, "a\x00b", errInvalidCgroupName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateSubCgroup(tc.parent, tc.sub)
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestRemoveCgroup_RefusesOutsideCgroupRoot(t *testing.T) {
	// Safety rail: RemoveCgroup is an rmdir; the path must be
	// under /sys/fs/cgroup or we refuse to act. Catches the
	// "we passed it the wrong string" class of bug at runtime
	// before it becomes a wider blast.
	err := RemoveCgroup("/tmp/not-a-cgroup")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "outside") {
		t.Errorf("err = %v, want 'outside' substring", err)
	}
}

func TestParseCgroupCPU(t *testing.T) {
	in := []byte(`usage_usec 1234567
user_usec 800000
system_usec 434567
nr_periods 5
nr_throttled 1
throttled_usec 100
core_sched.force_idle_usec 0
`)
	var out CgroupCPU
	if err := parseCgroupCPU(in, &out); err != nil {
		t.Fatalf("parseCgroupCPU: %v", err)
	}
	want := CgroupCPU{
		UsageUsec:     1234567,
		UserUsec:      800000,
		SystemUsec:    434567,
		NRPeriods:     5,
		NRThrottled:   1,
		ThrottledUsec: 100,
	}
	if out != want {
		t.Errorf("got %+v, want %+v", out, want)
	}
}

func TestParseCgroupCPU_BadField(t *testing.T) {
	in := []byte("usage_usec NaN\n")
	var out CgroupCPU
	if err := parseCgroupCPU(in, &out); err == nil {
		t.Fatal("want error on garbage value, got nil")
	}
}

func TestParseCgroupIO(t *testing.T) {
	in := []byte(`8:0 rbytes=1024 wbytes=2048 rios=10 wios=20 dbytes=512 dios=5
253:0 rbytes=99 wbytes=88

`)
	got, err := parseCgroupIO(in)
	if err != nil {
		t.Fatalf("parseCgroupIO: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d devices, want 2", len(got))
	}
	if got[0].Device != "8:0" || got[0].RBytes != 1024 || got[0].WIOs != 20 ||
		got[0].DBytes != 512 || got[0].DIOs != 5 {
		t.Errorf("device 0 = %+v", got[0])
	}
	if got[1].Device != "253:0" || got[1].RBytes != 99 || got[1].WBytes != 88 {
		t.Errorf("device 1 = %+v", got[1])
	}
}

func TestParseCgroupIO_BadField(t *testing.T) {
	in := []byte("8:0 rbytes=garbage\n")
	if _, err := parseCgroupIO(in); err == nil {
		t.Fatal("want error on garbage value, got nil")
	}
}

func TestParseUintLine(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"42\n", 42, true},
		{"  100  ", 100, true},
		{cgroupMaxSentinel, 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseUintLine([]byte(tc.in))
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseCgroupMax(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{cgroupMaxSentinel, -1, true},
		{cgroupMaxSentinel + "\n", -1, true},
		{"4194304\n", 4194304, true},
		{"NaN", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseCgroupMax([]byte(tc.in))
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCollectCgroupSample_MissingDir(t *testing.T) {
	_, err := collectCgroupSample(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestCollectCgroupSample_FakeTree(t *testing.T) {
	// Write a synthetic cgroup tree under t.TempDir() so we can
	// drive collectCgroupSample's full success path without
	// needing real delegation. cgroup files are just text;
	// collectCgroupSample only does os.ReadFile, no ioctl.
	dir := t.TempDir()
	files := map[string]string{
		cpuStatFile:       "usage_usec 100\nuser_usec 60\nsystem_usec 40\n",
		memoryCurrentFile: "1048576\n",
		memoryPeakFile:    "2097152\n",
		memoryMaxFile:     cgroupMaxSentinel + "\n",
		memorySwapFile:    "0\n",
		ioStatFile:        "8:0 rbytes=1024 wbytes=2048 rios=10 wios=20\n",
		pidsCurrentFile:   "7\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	sample, err := collectCgroupSample(dir)
	if err != nil {
		t.Fatalf("collectCgroupSample: %v", err)
	}
	if sample.Path != dir {
		t.Errorf("Path = %q, want %q", sample.Path, dir)
	}
	if sample.CPU.UsageUsec != 100 {
		t.Errorf("CPU.UsageUsec = %d, want 100", sample.CPU.UsageUsec)
	}
	if sample.Memory.CurrentBytes != 1048576 {
		t.Errorf("Memory.CurrentBytes = %d, want 1048576", sample.Memory.CurrentBytes)
	}
	if sample.Memory.PeakBytes != 2097152 {
		t.Errorf("Memory.PeakBytes = %d, want 2097152", sample.Memory.PeakBytes)
	}
	if sample.Memory.MaxBytes != -1 {
		t.Errorf("Memory.MaxBytes = %d, want -1 (max)", sample.Memory.MaxBytes)
	}
	if len(sample.IO) != 1 {
		t.Fatalf("got %d IO devices, want 1", len(sample.IO))
	}
	if sample.Pids.Current != 7 {
		t.Errorf("Pids.Current = %d, want 7", sample.Pids.Current)
	}
}

func TestCollectCgroupSample_ParseErrors(t *testing.T) {
	// Each per-file parse failure aborts the whole sample and
	// is reported back to the caller with a tagged error. We
	// run one case per file so a future kernel version change
	// that breaks one format is easy to localise.
	const garbage = "garbage\n"
	cases := []struct {
		filename string
		body     string
		wantErr  string
	}{
		{cpuStatFile, "usage_usec NaN\n", cpuStatFile},
		{memoryCurrentFile, garbage, memoryCurrentFile},
		{memoryPeakFile, garbage, memoryPeakFile},
		{memoryMaxFile, garbage, memoryMaxFile},
		{memorySwapFile, garbage, memorySwapFile},
		{ioStatFile, "8:0 rbytes=garbage\n", ioStatFile},
		{pidsCurrentFile, garbage, pidsCurrentFile},
	}
	for _, tc := range cases {
		t.Run(tc.filename, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tc.filename),
				[]byte(tc.body), 0o600); err != nil {
				t.Fatalf("write %s: %v", tc.filename, err)
			}
			_, err := collectCgroupSample(dir)
			if err == nil {
				t.Fatalf("want error on bad %s, got nil", tc.filename)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestCollectCgroupSample_PartialTree(t *testing.T) {
	// Cgroups whose parent doesn't have the relevant controller
	// in subtree_control will only have a subset of files. The
	// collector must succeed and just leave missing pieces zero-
	// valued.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, cpuStatFile),
		[]byte("usage_usec 42\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", cpuStatFile, err)
	}
	sample, err := collectCgroupSample(dir)
	if err != nil {
		t.Fatalf("collectCgroupSample: %v", err)
	}
	if sample.CPU.UsageUsec != 42 {
		t.Errorf("CPU.UsageUsec = %d, want 42", sample.CPU.UsageUsec)
	}
	if sample.Memory.CurrentBytes != 0 {
		t.Errorf("Memory.CurrentBytes = %d, want 0 (file missing)", sample.Memory.CurrentBytes)
	}
}

func TestOpenCgroupFD_Roundtrip(t *testing.T) {
	// Open our own cgroup as O_PATH and verify the fd is valid.
	// We can't use it to call clone3 here, but we can stat
	// through it to prove the cgroup is the directory we asked
	// for. Run last so a failing test leaves no leaked fd
	// (Cleanup closes).
	path, err := CurrentCgroup()
	if err != nil {
		t.Fatalf("CurrentCgroup: %v", err)
	}
	fd, err := OpenCgroupFD(path)
	if err != nil {
		t.Fatalf("OpenCgroupFD: %v", err)
	}
	t.Cleanup(func() {
		if cerr := closeFD(fd); cerr != nil {
			t.Errorf("close fd: %v", cerr)
		}
	})
	if fd < 0 {
		t.Errorf("fd = %d, want >= 0", fd)
	}
}

func TestOpenCgroupFD_NoSuchPath(t *testing.T) {
	_, err := OpenCgroupFD(filepath.Join(t.TempDir(), "nonexistent"))
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

// withFakeCgroupRoot points cgroupV2Root at a tempdir for the
// duration of the test, so create/remove paths can be exercised
// without needing real cgroup delegation. Restores via Cleanup.
func withFakeCgroupRoot(t *testing.T) string {
	t.Helper()
	fake := t.TempDir()
	orig := cgroupV2Root
	cgroupV2Root = fake
	t.Cleanup(func() { cgroupV2Root = orig })
	return fake
}

func TestCreateSubCgroup_Success(t *testing.T) {
	root := withFakeCgroupRoot(t)
	path, err := CreateSubCgroup(root, "test-sub")
	if err != nil {
		t.Fatalf("CreateSubCgroup: %v", err)
	}
	want := filepath.Join(root, "test-sub")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("not a directory: %s", path)
	}
}

func TestCreateSubCgroup_MkdirFails(t *testing.T) {
	root := withFakeCgroupRoot(t)
	// Pre-create the target so mkdir returns EEXIST.
	existing := filepath.Join(root, "already-here")
	if err := os.Mkdir(existing, 0o750); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := CreateSubCgroup(root, "already-here")
	if err == nil {
		t.Fatal("want EEXIST error, got nil")
	}
}

func TestDelegationOK_TrueWhenParentWritable(t *testing.T) {
	withFakeCgroupRoot(t)
	// Seed the session-scope parent so the probe child can be created.
	parent, err := CurrentCgroup()
	if err != nil {
		t.Fatalf("CurrentCgroup: %v", err)
	}
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatalf("seed parent: %v", err)
	}
	if !DelegationOK() {
		t.Error("DelegationOK = false with a writable seeded parent; want true")
	}
	// The probe must clean up after itself — no leftover probe cgroup.
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("readdir parent: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("DelegationOK left %d entries under parent; want 0 (probe not removed)", len(entries))
	}
}

func TestDelegationOK_FalseWhenParentMissing(t *testing.T) {
	// Point the cgroup root at a path that does NOT exist, so the probe
	// child's mkdir fails ENOENT and DelegationOK reports "not
	// delegated". A bare t.TempDir() is not enough: inside a cgroup
	// namespace (e.g. the dogfood self-build container) /proc/self/cgroup
	// is `0::/`, so CurrentCgroup() joins to the root itself — which
	// exists — and the probe would succeed. Joining under a missing
	// subdir keeps the parent absent whether the ambient cgroup path is
	// the container's `/` or a deep host path.
	orig := cgroupV2Root
	cgroupV2Root = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { cgroupV2Root = orig })
	if DelegationOK() {
		t.Error("DelegationOK = true with a missing cgroup root; want false")
	}
}

func TestRemoveCgroup_Success(t *testing.T) {
	root := withFakeCgroupRoot(t)
	path, err := CreateSubCgroup(root, "remove-me")
	if err != nil {
		t.Fatalf("CreateSubCgroup: %v", err)
	}
	if err := RemoveCgroup(path); err != nil {
		t.Fatalf("RemoveCgroup: %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("dir still exists after Remove: %v", statErr)
	}
}

func TestRemoveCgroup_RmdirFails(t *testing.T) {
	root := withFakeCgroupRoot(t)
	// Path under cgroupV2Root but doesn't exist — rmdir errors.
	if err := RemoveCgroup(filepath.Join(root, "absent")); err == nil {
		t.Fatal("want ENOENT error, got nil")
	}
}
