//nolint:dupl // deliberately parallel to execsnoop.go / opensnoop.go: the three gadget wirings differ only by gadget type; the shared logic (resolveSelfPIDNS) is already factored out and a generic abstraction would obscure the per-gadget types. Kept side-by-side for review.
package agent

import (
	"context"
	"io"
	"os"

	agentv1 "github.com/localai-org/darkaifactory/internal/genpb/agent/v1"
	"github.com/localai-org/darkaifactory/internal/observability/ebpf/sleepsnoop"
)

// SleepsnoopReportPrefix labels the sleep-snoop gadget's console status line.
const SleepsnoopReportPrefix = "sleepsnoop: "

// sleepsnoopDeps is the test-substitutable surface for the Tier-2 sleep-snoop
// eBPF gadget — the parallel of opensnoopDeps / execsnoopDeps. A zero value
// (run == nil) disables the gadget, so unit tests of runWith that don't wire it
// skip the BPF path. Loading BPF needs CAP_BPF, which `make check` lacks; the
// gadget only runs for real in the guest's init userns under the e2e.
type sleepsnoopDeps struct {
	// mountTracefs ensures tracefs is mounted so the tracepoint attach can
	// resolve the probe id (shared with the other gadgets — an already-mounted
	// tracefs is success, so all three calling it is fine).
	mountTracefs func() error
	// openLog creates/truncates the trace file at the guest path.
	openLog func(path string) (io.WriteCloser, error)
	// selfPIDNS returns the agent's own PID-namespace inode for the
	// container-workload filter; a zero return (or nil) disables the filter.
	selfPIDNS func() (uint32, error)
	// run drains the gadget into w until ctx is cancelled, folding each event
	// into agg. It calls onReady once its tracepoints are attached so the agent
	// can hold the container until the gadget is capturing. Production wires
	// sleepsnoop.RunAggregating.
	run func(ctx context.Context, w io.Writer, agg *sleepsnoop.Aggregator, selfPidns uint32, onReady func()) error
}

// defaultSleepsnoopDeps wires the real gadget. ensureTracefs + readSelfPIDNS
// are shared with the other gadgets (both Linux-only).
func defaultSleepsnoopDeps() sleepsnoopDeps {
	return sleepsnoopDeps{
		mountTracefs: ensureTracefs,
		openLog: func(path string) (io.WriteCloser, error) {
			//nolint:gosec // G304: path is the host-operator-set daft.observe.sleepsnoop cmdline value — same trust boundary as daft.bundle.
			return os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		},
		selfPIDNS: readSelfPIDNS,
		run:       sleepsnoop.RunAggregating,
	}
}

// startSleepsnoop resolves the sleep-snoop log path from the GuestConfig's
// observe.sleepsnoop and, when set,
// opens the guest-side trace file and spawns the gadget's drain loop for the
// agent's lifetime. It returns the live aggregator so the caller can fold it
// into the build-progress summary alongside the other gadgets', or nil when the
// gadget was not requested / could not start. Like the other start* gadget
// helpers it reports one line per outcome and never fails the boot —
// observability is a debugging surface, not a correctness invariant.
//
// Called before the container, returning a readiness channel that closes once
// the tracepoints are attached; runWith waits on it so the workload's first
// sleep can't outrun the gadget. nil agg + nil channel when the gadget was not
// requested or could not start.
//
//nolint:dupl // deliberately parallel to startOpensnoop / startExecsnoop; the gadget wirings differ only by gadget type and the shared logic (resolveSelfPIDNS, spawnGadgetDrain) is already factored out — a generic abstraction would be less readable than the symmetry.
func startSleepsnoop(ctx context.Context, w io.Writer, d runDeps) (agg *sleepsnoop.Aggregator, ready <-chan struct{}) {
	if d.sleepsnoop.run == nil {
		return nil, nil
	}
	path, ok := resolveObservePath(d.cfg, (*agentv1.GuestConfig_Observe).GetSleepsnoop)
	if !ok {
		return nil, nil
	}
	if err := d.sleepsnoop.mountTracefs(); err != nil {
		writeReport(w, SleepsnoopReportPrefix, err.Error())
		return nil, nil
	}
	f, err := d.sleepsnoop.openLog(path)
	if err != nil {
		writeReport(w, SleepsnoopReportPrefix, err.Error())
		return nil, nil
	}
	selfPidns, scope := resolveSelfPIDNS(w, SleepsnoopReportPrefix, d.sleepsnoop.selfPIDNS)
	agg = sleepsnoop.NewAggregator()
	writeReport(w, SleepsnoopReportPrefix, "tracing "+scope+" nanosleep → "+path)
	ready = spawnGadgetDrain(ctx, w, SleepsnoopReportPrefix, f, func(onReady func()) error {
		return d.sleepsnoop.run(ctx, f, agg, selfPidns, onReady)
	})
	return agg, ready
}
