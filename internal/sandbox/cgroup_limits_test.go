package sandbox

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/localai-org/darkaifactory/internal/observability"
)

func TestParseSizeBytes(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{defaultMemory, 256 << 20, true},
		{"4096M", 4096 << 20, true},
		{"1G", 1 << 30, true},
		{"2g", 2 << 30, true},
		{"512K", 512 << 10, true},
		{"1073741824", 1073741824, true},
		{"  256M  ", 256 << 20, true},
		{"", 0, false},
		{"abc", 0, false},
		{"12X", 0, false},       // X is not a suffix → "12X" fails to parse
		{"99999999T", 0, false}, // parses but n×TiB overflows uint64
	}
	for _, c := range cases {
		got, ok := parseSizeBytes(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseSizeBytes(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestLimitsForConfig(t *testing.T) {
	lim, ok := limitsForConfig(Config{Memory: defaultMemory, CPUs: 2})
	if !ok {
		t.Fatal("want ok for 256M")
	}
	if lim.memHigh != (256+128)<<20 { // +max(32M, 128M)
		t.Errorf("memHigh = %d, want %d", lim.memHigh, uint64(384)<<20)
	}
	if lim.memMax != (256+384)<<20 { // +max(64M, 384M)
		t.Errorf("memMax = %d, want %d", lim.memMax, uint64(640)<<20)
	}
	if lim.memMax <= lim.memHigh {
		t.Error("hard cap must exceed soft cap")
	}
	if lim.cpuWeight != 200 {
		t.Errorf("cpuWeight = %d, want 200", lim.cpuWeight)
	}
	if lim.cpuMax != "200000 100000" {
		t.Errorf("cpuMax = %q, want %q", lim.cpuMax, "200000 100000")
	}

	// Large guest: the fractions dominate the floors; cpu.weight clamps.
	big, _ := limitsForConfig(Config{Memory: "8G", CPUs: 200})
	if big.memHigh != 9<<30 { // 8G + 8G/8
		t.Errorf("8G memHigh = %d, want %d", big.memHigh, uint64(9)<<30)
	}
	if big.memMax != 10<<30 { // 8G + 8G/4
		t.Errorf("8G memMax = %d, want %d", big.memMax, uint64(10)<<30)
	}
	if big.cpuWeight != cpuWeightMax {
		t.Errorf("cpuWeight = %d, want clamp %d", big.cpuWeight, cpuWeightMax)
	}

	// Zero vCPUs floors to one core.
	one, _ := limitsForConfig(Config{Memory: defaultMemory, CPUs: 0})
	if one.cpuMax != "100000 100000" || one.cpuWeight != 100 {
		t.Errorf("CPUs=0 → cpuMax=%q cpuWeight=%d, want one core", one.cpuMax, one.cpuWeight)
	}

	if _, ok := limitsForConfig(Config{Memory: "garbage"}); ok {
		t.Error("want !ok for an unparseable memory string")
	}
}

func TestApplyVMCgroupLimits(t *testing.T) {
	root := t.TempDir()
	prev := observability.SwapCgroupRoot(root)
	t.Cleanup(func() { observability.SwapCgroupRoot(prev) })

	dir := filepath.Join(root, "vm")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	applyVMCgroupLimits(&buf, dir, Config{Memory: defaultMemory, CPUs: 2})
	for _, f := range []string{"memory.max", "memory.high", "cpu.max", "cpu.weight"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s not written: %v", f, err)
		}
	}
	got, _ := os.ReadFile(filepath.Join(dir, "cpu.max")) //nolint:gosec // test reads a file under the temp cgroup root.
	if string(got) != "200000 100000" {
		t.Errorf("cpu.max = %q", got)
	}

	// Empty dir is a no-op (the stub-cgroup unit path).
	applyVMCgroupLimits(&buf, "", Config{Memory: defaultMemory})

	// Unparseable memory logs and writes nothing.
	var buf2 bytes.Buffer
	applyVMCgroupLimits(&buf2, dir, Config{Memory: "nope"})
	if !strings.Contains(buf2.String(), "unparseable") {
		t.Errorf("want an 'unparseable' log, got %q", buf2.String())
	}

	// A dir outside the cgroup root makes every write fail; each is logged
	// best-effort rather than aborting.
	var buf3 bytes.Buffer
	applyVMCgroupLimits(&buf3, filepath.Join(t.TempDir(), "outside"), Config{Memory: defaultMemory, CPUs: 2})
	if !strings.Contains(buf3.String(), "set cgroup") {
		t.Errorf("want a write-failure log, got %q", buf3.String())
	}
}

// TestCreateVMCgroupUsesSetParent covers the SetVMCgroupParent path: when a
// parent is configured (the post-PrepareDelegatedSubtree state) per-VM
// cgroups are created under it rather than the caller's own cgroup.
func TestCreateVMCgroupUsesSetParent(t *testing.T) {
	root := t.TempDir()
	prev := observability.SwapCgroupRoot(root)
	t.Cleanup(func() { observability.SwapCgroupRoot(prev) })

	parent := filepath.Join(root, "scope")
	if err := os.MkdirAll(parent, 0o750); err != nil {
		t.Fatal(err)
	}
	SetVMCgroupParent(parent)
	t.Cleanup(func() { SetVMCgroupParent("") })

	dir, err := createVMCgroup("daft-vm-setparent")
	if err != nil {
		t.Fatalf("createVMCgroup: %v", err)
	}
	t.Cleanup(func() { _ = observability.RemoveCgroup(dir) })
	if filepath.Dir(dir) != parent {
		t.Errorf("per-VM cgroup parent = %q, want the set parent %q", filepath.Dir(dir), parent)
	}
}
