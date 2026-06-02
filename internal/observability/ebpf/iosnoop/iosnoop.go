//go:build linux

package iosnoop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

// tracefsMountpoint mirrors the agent's mount (opensnoop_linux.go). Kept local
// so the gadget package needn't import package agent.
const tracefsMountpoint = "/sys/kernel/tracing"

// tracefsSyscalls is where the kernel lists one sys_enter_<name> /
// sys_exit_<name> directory per syscall (CONFIG_FTRACE_SYSCALLS). The agent has
// already mounted tracefs by the time the gadget loads; we read this dir to
// learn which syscalls this guest kernel exposes a tracepoint for, and attach
// only to those (logging the rest — no silent gaps).
const tracefsSyscalls = tracefsMountpoint + "/events/syscalls"

// defaultInterval is how often the drain re-reads the BPF maps and rebuilds the
// aggregate — ~1 Hz, matching the summary writer it feeds.
const defaultInterval = time.Second

// sampleData is one read of the BPF maps: the monotonic global counter and the
// live per-fd entries (the ranking source).
type sampleData struct {
	totals Totals
	stats  []FDStat
}

// source is a loaded+attached gadget: a Sample closure over its maps, the
// syscalls it attached to / skipped (for the trace-file header), and a cleanup
// that detaches everything and unloads.
type source struct {
	sample   func() (sampleData, error)
	attached []string
	skipped  []string
	cleanup  func() error
}

// opener loads + attaches the gadget. openSourceFor binds the production
// implementation; tests pass a fake.
type opener func() (*source, error)

// resolveFunc turns a (pid, fd) into a target name. The production
// implementation reads /proc/<pid>/fd/<fd>; tests substitute a fake.
type resolveFunc func(pid uint32, fd int32) (string, error)

// Run loads the io-snoop gadget and drains it into w until ctx is cancelled.
// selfPidns is the caller's own PID-namespace inode: non-zero restricts the
// trace to the container workload (dropping the agent/init/crun), zero traces
// everything. Cancelling ctx is the normal stop and returns nil.
func Run(ctx context.Context, w io.Writer, selfPidns uint32) error {
	return run(ctx, w, nil, openSourceFor(selfPidns), defaultResolve, defaultInterval)
}

// RunAggregating is Run that additionally folds each map snapshot into agg, so
// the agent's summary writer can read the live aggregate while the same drain
// still writes the raw per-fd trace to w. A nil agg behaves like Run.
func RunAggregating(ctx context.Context, w io.Writer, agg *Aggregator, selfPidns uint32) error {
	return run(ctx, w, agg, openSourceFor(selfPidns), defaultResolve, defaultInterval)
}

// run is the testable core: open the source, write the trace header, then on
// each tick re-read the maps, resolve every fd to a target, emit a trace line
// for any fd whose call count advanced, and refresh agg. A final drain on ctx
// cancel captures the end state.
func run(ctx context.Context, w io.Writer, agg *Aggregator, open opener, resolve resolveFunc, interval time.Duration) error {
	src, err := open()
	if err != nil {
		return err
	}
	defer func() { _ = src.cleanup() }()

	writeHeader(w, src.attached, src.skipped)

	// Cross-tick state: cache fd→target so each fd is resolved once (and so a
	// since-closed fd keeps the name it had when first seen), and remember each
	// fd's last call count so the trace emits a line only on real activity.
	cache := map[uint64]string{}
	lastCalls := map[uint64]uint64{}

	t := time.NewTicker(interval)
	defer t.Stop()
	//daft:lint:disable=forloopbound reason="re-samples the BPF maps once per tick for the agent's lifetime; ctx cancel is the termination signal (drains a final snapshot and returns)"
	for {
		select {
		case <-ctx.Done():
			_ = drainOnce(w, agg, resolve, src, cache, lastCalls)
			return nil
		case <-t.C:
			if err := drainOnce(w, agg, resolve, src, cache, lastCalls); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

// drainOnce reads one map snapshot, resolves and emits, and refreshes agg. The
// cache/lastCalls maps are pruned to the fds present this tick so neither grows
// without bound over a long build.
func drainOnce(w io.Writer, agg *Aggregator, resolve resolveFunc, src *source, cache map[uint64]string, lastCalls map[uint64]uint64) error {
	smp, err := src.sample()
	if err != nil {
		return err
	}
	// Stable order so the trace file (and the tests) are deterministic.
	sort.Slice(smp.stats, func(i, j int) bool {
		if smp.stats[i].PID != smp.stats[j].PID {
			return smp.stats[i].PID < smp.stats[j].PID
		}
		return smp.stats[i].FD < smp.stats[j].FD
	})

	resolved := make([]Resolved, 0, len(smp.stats))
	seen := make(map[uint64]struct{}, len(smp.stats))
	for _, s := range smp.stats {
		key := fdKey(s.PID, s.FD)
		seen[key] = struct{}{}
		name, ok := cache[key]
		if !ok {
			name = resolveName(resolve, s.PID, s.FD)
			cache[key] = name
		}
		resolved = append(resolved, Resolved{Name: name, Stat: s})
		if s.Calls > lastCalls[key] {
			lastCalls[key] = s.Calls
			_, _ = io.WriteString(w, traceLine(s, name))
		}
	}
	pruneTo(cache, seen)
	pruneCounts(lastCalls, seen)

	if agg != nil {
		agg.Update(smp.totals, resolved)
	}
	return nil
}

// fdKey packs a (pid, fd) into the same u64 the BPF side uses as its fdstats
// key, so the cross-tick caches share one key space with the map.
func fdKey(pid uint32, fd int32) uint64 {
	//nolint:gosec // G115: fd is packed into the low 32 bits and unpacked symmetrically in decodeFDStat; the bit pattern is preserved by design.
	return uint64(pid)<<32 | uint64(uint32(fd))
}

// resolveName turns a (pid, fd) into a target via resolve, falling back to a
// synthetic "fd N (pid P)" when the fd is already gone (a since-closed fd whose
// accumulated bytes we still want to surface rather than drop).
func resolveName(resolve resolveFunc, pid uint32, fd int32) string {
	if name, err := resolve(pid, fd); err == nil && name != "" {
		return name
	}
	return fmt.Sprintf("fd %d (pid %d)", fd, pid)
}

// traceLine renders one per-fd activity line for the raw drill-down log.
func traceLine(s FDStat, name string) string {
	return fmt.Sprintf("pid=%d fd=%d %s target=%q r=%d w=%d calls=%d err=%d\n",
		s.PID, s.FD, roleString(s.Role), name, s.RdBytes, s.WrBytes, s.Calls, s.Errors)
}

// writeHeader records which syscalls the gadget attached to and which target
// syscalls this kernel lacks a tracepoint for — the no-silent-gaps audit line,
// kept in the raw trace rather than the console.
func writeHeader(w io.Writer, attached, skipped []string) {
	_, _ = fmt.Fprintf(w, "# iosnoop attached=%v skipped=%v\n", attached, skipped)
}

// pruneTo drops cache entries whose fd was not present in this tick's snapshot.
func pruneTo(cache map[uint64]string, seen map[uint64]struct{}) {
	for k := range cache {
		if _, ok := seen[k]; !ok {
			delete(cache, k)
		}
	}
}

// pruneCounts is pruneTo for the call-count map (a distinct value type).
func pruneCounts(counts map[uint64]uint64, seen map[uint64]struct{}) {
	for k := range counts {
		if _, ok := seen[k]; !ok {
			delete(counts, k)
		}
	}
}

// ---- the syscall→enter-program attach plan. Same-shaped syscalls share one
// enter program (read(2) and readv(2) both charge their args[0] fd); the shared
// exit program is attached to every entry's sys_exit. Kept in lockstep with
// iosnoop_bpf.c's enter hooks.
type enterEntry struct {
	syscall string
	prog    func(*iosnoopObjects) *ebpf.Program
}

func enterPlan() []enterEntry {
	rd := func(o *iosnoopObjects) *ebpf.Program { return o.HandleReadEnter }
	wr := func(o *iosnoopObjects) *ebpf.Program { return o.HandleWriteEnter }
	return []enterEntry{
		{"read", rd},
		{"pread64", rd},
		{"readv", rd},
		{"preadv", rd},
		{"preadv2", rd},
		{"recvfrom", rd},
		{"recvmsg", rd},
		{"write", wr},
		{"pwrite64", wr},
		{"writev", wr},
		{"pwritev", wr},
		{"pwritev2", wr},
		{"sendto", wr},
		{"sendmsg", wr},
		{"recvmmsg", func(o *iosnoopObjects) *ebpf.Program { return o.HandleRecvmmsgEnter }},
		{"sendmmsg", func(o *iosnoopObjects) *ebpf.Program { return o.HandleSendmmsgEnter }},
		{"connect", func(o *iosnoopObjects) *ebpf.Program { return o.HandleConnectEnter }},
		{"accept", func(o *iosnoopObjects) *ebpf.Program { return o.HandleAcceptEnter }},
		{"accept4", func(o *iosnoopObjects) *ebpf.Program { return o.HandleAcceptEnter }},
		{"sendfile64", func(o *iosnoopObjects) *ebpf.Program { return o.HandleSendfileEnter }},
		{"splice", func(o *iosnoopObjects) *ebpf.Program { return o.HandleSpliceEnter }},
		{"copy_file_range", func(o *iosnoopObjects) *ebpf.Program { return o.HandleSpliceEnter }},
		{"tee", func(o *iosnoopObjects) *ebpf.Program { return o.HandleTeeEnter }},
		{"vmsplice", func(o *iosnoopObjects) *ebpf.Program { return o.HandleVmspliceEnter }},
	}
}

// bpfDeps are the privileged BPF primitives openSourceWith composes. The real
// implementations need CAP_BPF and run in the guest under the e2e; tests inject
// fakes to cover the load/attach/sample wiring without loading BPF. The two map
// reads are split out (readTotals/iterFDStats) so the sample assembly
// (collectSample) stays unit-testable while only the raw kernel reads stay dark.
type bpfDeps struct {
	memlock     func() error
	load        func(o *iosnoopObjects, selfPidns uint32) error
	listEnter   func() (map[string]struct{}, error)
	attach      func(tracepoint string, prog *ebpf.Program) (func() error, error)
	readTotals  func(o *iosnoopObjects) ([]iosnoopIototals, error)
	iterFDStats func(o *iosnoopObjects, yield func(key uint64, v iosnoopFdstat)) error
}

// openSourceFor binds selfPidns into an opener so run's drain stays unaware of
// the filter (and the test fakes keep the no-arg opener shape).
func openSourceFor(selfPidns uint32) opener {
	return func() (*source, error) { return openSourceWith(defaultBPFDeps(), selfPidns) }
}

// openSourceWith composes the load path from injected primitives: bump memlock,
// load the objects (writing the container-workload filter into .rodata),
// enumerate the available syscall tracepoints, and attach the enter+exit pair
// for each target syscall this kernel exposes. It unwinds in reverse on any
// later failure, and fails only if no target syscall is traceable at all.
func openSourceWith(d bpfDeps, selfPidns uint32) (*source, error) {
	if err := d.memlock(); err != nil {
		return nil, fmt.Errorf("iosnoop: remove memlock: %w", err)
	}
	var objs iosnoopObjects
	if err := d.load(&objs, selfPidns); err != nil {
		return nil, fmt.Errorf("iosnoop: load objects: %w", err)
	}
	avail, err := d.listEnter()
	if err != nil {
		_ = objs.Close()
		return nil, fmt.Errorf("iosnoop: list syscall tracepoints: %w", err)
	}

	var detachers []func() error
	undo := func() {
		for i := len(detachers) - 1; i >= 0; i-- {
			_ = detachers[i]()
		}
		_ = objs.Close()
	}
	var attached, skipped []string
	for _, e := range enterPlan() {
		_, haveEnter := avail["sys_enter_"+e.syscall]
		_, haveExit := avail["sys_exit_"+e.syscall]
		if !haveEnter || !haveExit {
			skipped = append(skipped, e.syscall)
			continue
		}
		dEnter, err := d.attach("sys_enter_"+e.syscall, e.prog(&objs))
		if err != nil {
			undo()
			return nil, fmt.Errorf("iosnoop: attach sys_enter_%s: %w", e.syscall, err)
		}
		detachers = append(detachers, dEnter)
		dExit, err := d.attach("sys_exit_"+e.syscall, objs.HandleIoExit)
		if err != nil {
			undo()
			return nil, fmt.Errorf("iosnoop: attach sys_exit_%s: %w", e.syscall, err)
		}
		detachers = append(detachers, dExit)
		attached = append(attached, e.syscall)
	}
	if len(attached) == 0 {
		undo()
		return nil, errors.New("iosnoop: no traceable I/O syscalls available")
	}
	return &source{
		sample:   func() (sampleData, error) { return collectSample(d.readTotals, d.iterFDStats, &objs) },
		attached: attached,
		skipped:  skipped,
		cleanup:  func() error { undo(); return nil },
	}, nil
}

// collectSample assembles one sampleData from the two injected map reads: the
// per-CPU global summed into a monotonic Totals, and every live per-fd entry
// decoded. The arithmetic (sumTotals/decodeFDStat) is pure, so this assembly is
// unit-testable with fake reads; only the reads themselves need a kernel.
func collectSample(readTotals func(*iosnoopObjects) ([]iosnoopIototals, error), iterFDStats func(*iosnoopObjects, func(uint64, iosnoopFdstat)) error, o *iosnoopObjects) (sampleData, error) {
	var sd sampleData
	per, err := readTotals(o)
	if err != nil {
		return sd, fmt.Errorf("iosnoop: read totals: %w", err)
	}
	sd.totals = sumTotals(per)
	err = iterFDStats(o, func(key uint64, v iosnoopFdstat) {
		sd.stats = append(sd.stats, decodeFDStat(key, v))
	})
	if err != nil {
		return sd, fmt.Errorf("iosnoop: iterate fdstats: %w", err)
	}
	return sd, nil
}

// defaultBPFDeps wires the real cilium/ebpf calls. Its closure bodies are the
// only part of this file that needs CAP_BPF — they run in the guest under the
// e2e gadget test, not under make check.
func defaultBPFDeps() bpfDeps {
	return bpfDeps{
		memlock: rlimit.RemoveMemlock,
		load: func(o *iosnoopObjects, selfPidns uint32) error {
			spec, err := loadIosnoop()
			if err != nil {
				return fmt.Errorf("iosnoop: load spec: %w", err)
			}
			if v := spec.Variables["self_pidns"]; v != nil {
				_ = v.Set(selfPidns)
			}
			return spec.LoadAndAssign(o, nil)
		},
		listEnter: func() (map[string]struct{}, error) { return listTracepointSyscalls(tracefsSyscalls) },
		attach: func(tp string, prog *ebpf.Program) (func() error, error) {
			l, err := link.Tracepoint("syscalls", tp, prog, nil)
			if err != nil {
				return nil, err
			}
			return l.Close, nil
		},
		readTotals: func(o *iosnoopObjects) ([]iosnoopIototals, error) {
			var per []iosnoopIototals
			err := o.Totals.Lookup(uint32(0), &per)
			return per, err
		},
		iterFDStats: func(o *iosnoopObjects, yield func(uint64, iosnoopFdstat)) error {
			var key uint64
			var val iosnoopFdstat
			it := o.Fdstats.Iterate()
			for it.Next(&key, &val) {
				yield(key, val)
			}
			return it.Err()
		},
	}
}

// listTracepointSyscalls returns the set of tracepoint event names under dir
// (e.g. "sys_enter_read", "sys_exit_read") — the runtime source of truth for
// which syscalls this kernel can be traced on.
func listTracepointSyscalls(dir string) (map[string]struct{}, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(ents))
	for _, e := range ents {
		set[e.Name()] = struct{}{}
	}
	return set, nil
}

// sumTotals folds the per-CPU global counter (one element per online CPU) into
// a single monotonic Totals — the BPF side increments only the current CPU's
// copy, so the truth is the sum.
func sumTotals(per []iosnoopIototals) Totals {
	var t Totals
	for _, c := range per {
		t.Calls += c.Calls
		t.RdBytes += c.RdBytes
		t.WrBytes += c.WrBytes
		t.Errors += c.Errors
	}
	return t
}

// decodeFDStat unpacks one fdstats entry: the key is the (pid<<32)|fd pair the
// BPF side wrote, the value its accumulated counters.
func decodeFDStat(key uint64, v iosnoopFdstat) FDStat {
	return FDStat{
		PID: uint32(key >> 32),
		//nolint:gosec // G115: the low 32 bits are the fd that fdKey packed in; reinterpreting them as int32 restores the original value.
		FD:      int32(uint32(key)),
		RdBytes: v.RdBytes,
		WrBytes: v.WrBytes,
		Calls:   v.Calls,
		Errors:  v.Errors,
		Role:    v.Role,
	}
}

// defaultResolve reads /proc/<pid>/fd/<fd>, whose symlink target is the file
// path, "socket:[inode]", "pipe:[inode]", or "anon_inode:[...]" — a complete
// target identity without any in-kernel dentry walk.
func defaultResolve(pid uint32, fd int32) (string, error) {
	return os.Readlink("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/fd/" + strconv.FormatInt(int64(fd), 10))
}
