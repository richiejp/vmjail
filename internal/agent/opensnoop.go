package agent

import (
	"context"
	"io"
	"os"

	agentv1 "github.com/localai-org/darkaifactory/internal/genpb/agent/v1"
	"github.com/localai-org/darkaifactory/internal/observability/ebpf/opensnoop"
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
	// Production wires opensnoop.RunAggregating; tests inject a fake so the
	// file + goroutine wiring is covered without loading BPF.
	run func(ctx context.Context, w io.Writer, agg *opensnoop.Aggregator, selfPidns uint32) error
}

// defaultOpensnoopDeps wires the real gadget. opensnoop.Run loads the
// embedded BPF object, attaches the openat tracepoint, and drains the
// ring buffer — all of which need the guest init userns's CAP_BPF;
// ensureTracefs mounts the tracefs the tracepoint attach reads from.
func defaultOpensnoopDeps() opensnoopDeps {
	return opensnoopDeps{
		mountTracefs: ensureTracefs,
		openLog: func(path string) (io.WriteCloser, error) {
			//nolint:gosec // G304: path is the host-operator-set daft.observe.opensnoop cmdline value — same trust boundary as daft.bundle; no untrusted input.
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

// startOpensnoopFromCmdline resolves the open-snoop log path (the
// GuestConfig's observe.opensnoop, else the daft.observe.opensnoop cmdline
// token) and, when set,
// opens the guest-side trace file and spawns the gadget's drain loop for
// the agent's lifetime (ctx cancel stops it and closes the file). It returns
// the live aggregator so the caller can fold it into the build-progress
// summary, or nil when the gadget was not requested / could not start. Like
// the other start*FromCmdline helpers it reports one OpensnoopReportPrefix
// line per outcome and never fails the boot — observability is a debugging
// surface, not a correctness invariant.
//
// Called before the container so the tracepoint is attaching before the
// workload's openat storm; the ring buffer absorbs events emitted while
// the drain goroutine is still being scheduled.
//
//nolint:dupl // deliberately parallel to startExecsnoopFromCmdline; the two gadget wirings differ only by gadget type and the shared logic (resolveSelfPIDNS) is already factored out — a generic abstraction would be less readable than the symmetry.
func startOpensnoopFromCmdline(ctx context.Context, w io.Writer, d runDeps, cmdline string) *opensnoop.Aggregator {
	if d.opensnoop.run == nil {
		return nil
	}
	path, ok, err := resolveObservePath(d.cfg, (*agentv1.GuestConfig_Observe).GetOpensnoop, ParseOpensnoopCmdline, cmdline)
	switch {
	case err != nil:
		writeReport(w, OpensnoopReportPrefix, "cmdline: "+err.Error())
		return nil
	case !ok:
		return nil
	}
	if err := d.opensnoop.mountTracefs(); err != nil {
		writeReport(w, OpensnoopReportPrefix, err.Error())
		return nil
	}
	f, err := d.opensnoop.openLog(path)
	if err != nil {
		writeReport(w, OpensnoopReportPrefix, err.Error())
		return nil
	}
	selfPidns, scope := resolveSelfPIDNS(w, OpensnoopReportPrefix, d.opensnoop.selfPIDNS)
	agg := opensnoop.NewAggregator()
	writeReport(w, OpensnoopReportPrefix, "tracing "+scope+" openat → "+path)
	go func() {
		defer func() { _ = f.Close() }()
		// ctx cancel is the normal stop (Run returns nil); only a real
		// mid-run failure is worth a console line, and only while the VM
		// is still up to carry it.
		if rerr := d.opensnoop.run(ctx, f, agg, selfPidns); rerr != nil && ctx.Err() == nil {
			writeReport(w, OpensnoopReportPrefix, rerr.Error())
		}
	}()
	return agg
}
