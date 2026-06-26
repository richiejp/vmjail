//nolint:dupl // deliberately parallel to sleepsnoop.go / opensnoop.go: the three gadget wirings differ only by gadget type; the shared logic (resolveSelfPIDNS) is already factored out and a generic abstraction would obscure the per-gadget types. Kept side-by-side for review.
package agent

import (
	"context"
	"io"
	"os"

	agentv1 "github.com/richiejp/darkaifactory/internal/genpb/agent/v1"
	"github.com/richiejp/darkaifactory/internal/observability/ebpf/execsnoop"
)

// ExecsnoopReportPrefix labels the exec-snoop gadget's console status line.
const ExecsnoopReportPrefix = "execsnoop: "

// execsnoopDeps is the test-substitutable surface for the Tier-2 exec-snoop
// eBPF gadget — the parallel of opensnoopDeps. A zero value (run == nil)
// disables the gadget, so unit tests of runWith that don't wire it skip the
// BPF path. Loading BPF needs CAP_BPF, which `make check` lacks; the gadget
// only runs for real in the guest's init userns under the e2e.
type execsnoopDeps struct {
	// mountTracefs ensures tracefs is mounted so the tracepoint attach can
	// resolve the probe id (shared with open-snoop — an already-mounted
	// tracefs is success, so both gadgets calling it is fine).
	mountTracefs func() error
	// openLog creates/truncates the trace file at the guest path.
	openLog func(path string) (io.WriteCloser, error)
	// selfPIDNS returns the agent's own PID-namespace inode for the
	// container-workload filter; a zero return (or nil) disables the filter.
	selfPIDNS func() (uint32, error)
	// run drains the gadget into w until ctx is cancelled, folding each event
	// into agg. It calls onReady once its tracepoints are attached so the agent
	// can hold the container until the gadget is capturing. Production wires
	// execsnoop.RunAggregating.
	run func(ctx context.Context, w io.Writer, agg *execsnoop.Aggregator, selfPidns uint32, onReady func()) error
}

// defaultExecsnoopDeps wires the real gadget. ensureTracefs + readSelfPIDNS
// are shared with open-snoop (both Linux-only).
func defaultExecsnoopDeps() execsnoopDeps {
	return execsnoopDeps{
		mountTracefs: ensureTracefs,
		openLog: func(path string) (io.WriteCloser, error) {
			//nolint:gosec // G304: path is the host-operator-set daft.observe.execsnoop cmdline value — same trust boundary as daft.bundle.
			return os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		},
		selfPIDNS: readSelfPIDNS,
		run:       execsnoop.RunAggregating,
	}
}

// startExecsnoop resolves the exec-snoop log path from the GuestConfig's
// observe.execsnoop and, when set,
// opens the guest-side trace file and spawns the gadget's drain loop for the
// agent's lifetime. It returns the live aggregator so the caller can fold it
// into the build-progress summary alongside open-snoop's, or nil when the
// gadget was not requested / could not start. Like the other
// start* gadget helpers it reports one line per outcome and never fails
// the boot — observability is a debugging surface, not a correctness
// invariant.
//
// Called before the container, returning a readiness channel that closes once
// the tracepoints are attached; runWith waits on it so the workload's first
// exec can't outrun the gadget. nil agg + nil channel when the gadget was not
// requested or could not start.
//
//nolint:dupl // deliberately parallel to startOpensnoop; the two gadget wirings differ only by gadget type and the shared logic (resolveSelfPIDNS, spawnGadgetDrain) is already factored out — a generic abstraction would be less readable than the symmetry.
func startExecsnoop(ctx context.Context, w io.Writer, d runDeps) (agg *execsnoop.Aggregator, ready <-chan struct{}) {
	if d.execsnoop.run == nil {
		return nil, nil
	}
	path, ok := resolveObservePath(d.cfg, (*agentv1.GuestConfig_Observe).GetExecsnoop)
	if !ok {
		return nil, nil
	}
	if err := d.execsnoop.mountTracefs(); err != nil {
		writeReport(w, ExecsnoopReportPrefix, err.Error())
		return nil, nil
	}
	f, err := d.execsnoop.openLog(path)
	if err != nil {
		writeReport(w, ExecsnoopReportPrefix, err.Error())
		return nil, nil
	}
	selfPidns, scope := resolveSelfPIDNS(w, ExecsnoopReportPrefix, d.execsnoop.selfPIDNS)
	agg = execsnoop.NewAggregator()
	writeReport(w, ExecsnoopReportPrefix, "tracing "+scope+" execve → "+path)
	ready = spawnGadgetDrain(ctx, w, ExecsnoopReportPrefix, f, func(onReady func()) error {
		return d.execsnoop.run(ctx, f, agg, selfPidns, onReady)
	})
	return agg, ready
}
