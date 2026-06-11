package sandbox

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/localai-org/darkaifactory/internal/observability"
)

// SupervisorLeaf is the cgroup the sandbox-host process moves itself into
// at startup so its delegated scope has no internal processes and can
// enable the memory/cpu controllers per-VM cgroups need
// (observability.PrepareDelegatedSubtree). Per-VM cgroups are this leaf's
// siblings under the scope.
const SupervisorLeaf = "daft-supervisor"

// vmCgroupParent is the cgroup under which per-VM cgroups are created. Set
// once at startup (SetVMCgroupParent) to the delegated scope returned by
// observability.PrepareDelegatedSubtree, so per-VM cgroups are siblings of
// the supervisor leaf and inherit the memory/cpu/io/pids controllers.
// Empty falls back to the caller's own cgroup — the pre-limits behaviour,
// used by unit tests and any path that didn't prepare the subtree.
//
//nolint:gochecknoglobals // process-wide startup config, mirrors createVMCgroupFn/cgroupV2Root.
var vmCgroupParent string

// SetVMCgroupParent records the parent cgroup for per-VM cgroups. The
// sandbox-host calls it at startup with PrepareDelegatedSubtree's result.
func SetVMCgroupParent(path string) { vmCgroupParent = path }

// applyVMCgroupLimitsFn is the limit-application seam serveWith/helloWith
// call, as a package var so tests that drive the real createVMCgroup
// against a fake (regular-file) cgroup root can stub it out — there the
// limit writes would create real files that block the teardown rmdir,
// whereas a real cgroup's control files do not.
//
//nolint:gochecknoglobals // test seam, mirrors createVMCgroupFn.
var applyVMCgroupLimitsFn = applyVMCgroupLimits

// cgroup v2 CPU bandwidth knobs.
const (
	// cpuMaxPeriodUsec is the cpu.max scheduling period (100ms). The quota
	// is cpuMaxPeriodUsec × the vCPU count, so a VM is hard-capped to its
	// allocated cores' worth of host CPU.
	cpuMaxPeriodUsec = 100_000
	// cpuWeightPerCore scales the soft cpu.weight with the vCPU count so a
	// larger VM gets a proportionally larger share under contention.
	cpuWeightPerCore = 100
	// cpuWeightMax is the cgroup v2 cpu.weight ceiling.
	cpuWeightMax = 10_000
)

// Memory headroom the cgroup adds over the guest RAM size. The per-VM
// cgroup holds the VMM (cloud-hypervisor, which maps all of guest RAM)
// plus its virtiofsd sidecars and their page cache for the shares, so a
// cap at exactly guest RAM would OOM-kill a healthy VM. The fraction
// bounds page-cache growth proportionally; the floor covers the roughly
// fixed VMM/virtiofsd overhead for small guests. high < max so reclaim
// pressure runs before the hard kill.
const (
	memHighDivisor = 8         // soft headroom ≈ guest/8 (12.5%)
	memMaxDivisor  = 4         // hard headroom ≈ guest/4 (25%)
	memHighFloor   = 128 << 20 // 128 MiB
	memMaxFloor    = 384 << 20 // 384 MiB
)

// cgroupLimits are the per-VM cgroup v2 caps derived from a VM's resource
// allocation. Both a soft and a hard bound per resource: the soft
// (memory.high / cpu.weight) applies reclaim / contention pressure first;
// the hard (memory.max / cpu.max) is the kill / throttle ceiling.
type cgroupLimits struct {
	memHigh   uint64 // memory.high bytes (soft)
	memMax    uint64 // memory.max bytes (hard)
	cpuWeight uint64 // cpu.weight 1..10000 (soft)
	cpuMax    string // cpu.max "<quota> <period>" (hard)
}

// limitsForConfig derives the per-VM caps from cfg's effective guest RAM
// and vCPU count (post-defaults). ok is false when cfg.Memory can't be
// parsed, so the caller skips limiting rather than writing a garbage cap.
func limitsForConfig(cfg Config) (lim cgroupLimits, ok bool) {
	guest, ok := parseSizeBytes(cfg.Memory)
	if !ok {
		return cgroupLimits{}, false
	}
	cpus := max(cfg.CPUs, 1)
	weight := min(uint64(cpus)*cpuWeightPerCore, cpuWeightMax)
	return cgroupLimits{
		memHigh:   guest + max(guest/memHighDivisor, memHighFloor),
		memMax:    guest + max(guest/memMaxDivisor, memMaxFloor),
		cpuWeight: weight,
		cpuMax:    fmt.Sprintf("%d %d", cpus*cpuMaxPeriodUsec, cpuMaxPeriodUsec),
	}, true
}

// applyVMCgroupLimits writes the per-VM cgroup's soft + hard memory and
// CPU caps derived from cfg. Best-effort: a write failure (e.g. a
// controller the parent scope didn't enable) leaves that one knob at the
// cgroup default and is logged, rather than aborting the boot — the cgroup
// still bounds the VM by whatever knobs did apply. The controllers
// themselves are a startup precondition (PrepareDelegatedSubtree), so in
// production every write here succeeds. A "" dir (unit tests with a stub
// cgroup creator) is a no-op.
func applyVMCgroupLimits(w io.Writer, dir string, cfg Config) {
	if dir == "" {
		return
	}
	lim, ok := limitsForConfig(cfg)
	if !ok {
		_, _ = fmt.Fprintf(w, "sandbox: cgroup limits skipped: unparseable memory %q\n", cfg.Memory)
		return
	}
	// memory.max before memory.high so the kernel never briefly sees
	// high > max; cpu order is independent.
	for _, kv := range []struct{ file, val string }{
		{"memory.max", strconv.FormatUint(lim.memMax, 10)},
		{"memory.high", strconv.FormatUint(lim.memHigh, 10)},
		{"cpu.max", lim.cpuMax},
		{"cpu.weight", strconv.FormatUint(lim.cpuWeight, 10)},
	} {
		if err := observability.WriteCgroupControl(dir, kv.file, kv.val); err != nil {
			_, _ = fmt.Fprintf(w, "sandbox: set cgroup %s=%s: %v\n", kv.file, kv.val, err)
		}
	}
}

// parseSizeBytes parses a guest-RAM size string — a decimal count with an
// optional binary K/M/G/T suffix (the form cloud-hypervisor's --memory
// size= and sandbox.Config.Memory use, e.g. "256M", "4G", "1073741824").
func parseSizeBytes(s string) (uint64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	mult := uint64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		mult = 1 << 10
	case 'M', 'm':
		mult = 1 << 20
	case 'G', 'g':
		mult = 1 << 30
	case 'T', 't':
		mult = 1 << 40
	}
	if mult != 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil || (n != 0 && n > (1<<64-1)/mult) { // overflow guard
		return 0, false
	}
	return n * mult, true
}
