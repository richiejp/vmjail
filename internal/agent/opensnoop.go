package agent

import (
	"context"
	"io"
	"os"

	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
	"github.com/richiejp/vmjail/internal/observability/ebpf/opensnoop"
)

// OpensnoopReportPrefix labels the open-snoop gadget's console status
// line, mirroring the other per-channel report prefixes.
const OpensnoopReportPrefix = "opensnoop: "

// opensnoopDeps is the test-substitutable surface for the Tier-2
// open-snoop eBPF gadget: open the guest-side trace file and run the
// gadget's drain loop. Mirrors tapDeps — a zero value (run == nil)
// disables the gadget, so unit tests of runWith that don't wire it skip
// the BPF path entirely. Loading BPF needs CAP_BPF on a BPF-capable
// kernel, which `make check` lacks (unprivileged_bpf_disabled); the
// gadget only runs for real in the guest's init userns under the e2e.
type opensnoopDeps struct {
	// mountTracefs ensures tracefs is mounted at /sys/kernel/tracing.
	// cilium/ebpf resolves the tracepoint id from tracefs (or debugfs);
	// the minimal u-root guest mounts neither, so the attach fails
	// without this. An already-mounted tracefs is success.
	mountTracefs func() error
	// openLog creates/truncates the trace file at the guest path.
	openLog func(path string) (io.WriteCloser, error)
	// selfPIDNS returns the agent's own PID-namespace inode number, passed
	// to the gadget so it traces only the container workload (which crun
	// runs in a child PID namespace) and drops the agent / init / kernel-
	// thread openat noise that otherwise dominated the summary. A zero
	// return — or this being nil — disables the filter, so a host that
	// can't read the namespace degrades to the unfiltered firehose rather
	// than an empty trace.
	selfPIDNS func() (uint32, error)
	// run drains the gadget into w until ctx is cancelled, folding each
	// event into agg so the summary writer can snapshot the live aggregate.
	// It calls onReady once its tracepoints are attached so the agent can hold
	// the container until the gadget is capturing. Production wires
	// opensnoop.RunAggregating; tests inject a fake so the file + goroutine
	// wiring is covered without loading BPF.
	run func(ctx context.Context, w io.Writer, agg *opensnoop.Aggregator, selfPidns uint32, onReady func()) error
}

// defaultOpensnoopDeps wires the real gadget. opensnoop.Run loads the
// embedded BPF object, attaches the openat tracepoint, and drains the
// ring buffer — all of which need the guest init userns's CAP_BPF;
// ensureTracefs mounts the tracefs the tracepoint attach reads from.
func defaultOpensnoopDeps() opensnoopDeps {
	return opensnoopDeps{
		mountTracefs: ensureTracefs,
		openLog: func(path string) (io.WriteCloser, error) {
			//nolint:gosec // G304: path is the host-operator-set vmjail.observe.opensnoop cmdline value — same trust boundary as vmjail.bundle; no untrusted input.
			return os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		},
		selfPIDNS: readSelfPIDNS,
		run:       opensnoop.RunAggregating,
	}
}

// resolveSelfPIDNS resolves the agent's own PID-namespace inode for a
// gadget's container-workload filter, shared by open-snoop and exec-snoop. A
// failure (or a nil dep) leaves selfPidns 0 — the gadget reads that as "trace
// everything", a degraded but non-empty fallback so the build summary
// survives a host that can't read the namespace — and is reported on w. The
// returned scope ("container" / "all") labels the gadget's status line.
func resolveSelfPIDNS(w io.Writer, prefix string, fn func() (uint32, error)) (selfPidns uint32, scope string) {
	scope = "all"
	if fn != nil {
		if ns, err := fn(); err != nil {
			writeReport(w, prefix, "pidns: "+err.Error()+" (tracing all processes)")
		} else {
			selfPidns, scope = ns, "container"
		}
	}
	return selfPidns, scope
}

// startOpensnoop resolves the open-snoop log path from the GuestConfig's
// observe.opensnoop and, when set,
// opens the guest-side trace file and spawns the gadget's drain loop for
// the agent's lifetime (ctx cancel stops it and closes the file). It returns
// the live aggregator so the caller can fold it into the build-progress
// summary, or nil when the gadget was not requested / could not start. Like
// the other start* gadget helpers it reports one OpensnoopReportPrefix
// line per outcome and never fails the boot — observability is a debugging
// surface, not a correctness invariant.
//
// Called before the container, returning a readiness channel that closes once
// the tracepoints are attached; runWith waits on it so the workload's openat
// storm can't start before the gadget is capturing. nil agg + nil channel when
// the gadget was not requested or could not start.
//
//nolint:dupl // deliberately parallel to startExecsnoop; the two gadget wirings differ only by gadget type and the shared logic (resolveSelfPIDNS, spawnGadgetDrain) is already factored out — a generic abstraction would be less readable than the symmetry.
func startOpensnoop(ctx context.Context, w io.Writer, d runDeps) (agg *opensnoop.Aggregator, ready <-chan struct{}) {
	if d.opensnoop.run == nil {
		return nil, nil
	}
	path, ok := resolveObservePath(d.cfg, (*agentv1.GuestConfig_Observe).GetOpensnoop)
	if !ok {
		return nil, nil
	}
	if err := d.opensnoop.mountTracefs(); err != nil {
		writeReport(w, OpensnoopReportPrefix, err.Error())
		return nil, nil
	}
	f, err := d.opensnoop.openLog(path)
	if err != nil {
		writeReport(w, OpensnoopReportPrefix, err.Error())
		return nil, nil
	}
	selfPidns, scope := resolveSelfPIDNS(w, OpensnoopReportPrefix, d.opensnoop.selfPIDNS)
	agg = opensnoop.NewAggregator()
	writeReport(w, OpensnoopReportPrefix, "tracing "+scope+" openat → "+path)
	ready = spawnGadgetDrain(ctx, w, OpensnoopReportPrefix, f, func(onReady func()) error {
		return d.opensnoop.run(ctx, f, agg, selfPidns, onReady)
	})
	return agg, ready
}
