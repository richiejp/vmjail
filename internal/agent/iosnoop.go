//nolint:dupl // deliberately parallel to sleepsnoop.go / execsnoop.go / opensnoop.go: the gadget wirings differ only by gadget type; the shared logic (resolveSelfPIDNS) is already factored out and a generic abstraction would obscure the per-gadget types. Kept side-by-side for review.
package agent

import (
	"context"
	"io"
	"os"

	agentv1 "github.com/richiejp/vmjail/internal/genpb/agent/v1"
	"github.com/richiejp/vmjail/internal/observability/ebpf/iosnoop"
)

// IosnoopReportPrefix labels the io-snoop gadget's console status line.
const IosnoopReportPrefix = "iosnoop: "

// iosnoopDeps is the test-substitutable surface for the Tier-2 io-snoop eBPF
// gadget — the parallel of opensnoopDeps / sleepsnoopDeps. A zero value
// (run == nil) disables the gadget, so unit tests of runWith that don't wire it
// skip the BPF path. Loading BPF needs CAP_BPF, which `make check` lacks; the
// gadget only runs for real in the guest's init userns under the e2e.
type iosnoopDeps struct {
	// mountTracefs ensures tracefs is mounted so the loader can enumerate and
	// attach the I/O syscall tracepoints (shared with the other gadgets — an
	// already-mounted tracefs is success, so all of them calling it is fine).
	mountTracefs func() error
	// openLog creates/truncates the trace file at the guest path.
	openLog func(path string) (io.WriteCloser, error)
	// selfPIDNS returns the agent's own PID-namespace inode for the
	// container-workload filter; a zero return (or nil) disables the filter.
	selfPIDNS func() (uint32, error)
	// run drains the gadget into w until ctx is cancelled, folding each map
	// snapshot into agg. It calls onReady once its tracepoints are attached so
	// the agent can hold the container until the gadget is capturing. Production
	// wires iosnoop.RunAggregating.
	run func(ctx context.Context, w io.Writer, agg *iosnoop.Aggregator, selfPidns uint32, onReady func()) error
}

// defaultIosnoopDeps wires the real gadget. ensureTracefs + readSelfPIDNS are
// shared with the other gadgets (both Linux-only).
func defaultIosnoopDeps() iosnoopDeps {
	return iosnoopDeps{
		mountTracefs: ensureTracefs,
		openLog: func(path string) (io.WriteCloser, error) {
			//nolint:gosec // G304: path is the host-operator-set vmjail.observe.iosnoop cmdline value — same trust boundary as vmjail.bundle.
			return os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		},
		selfPIDNS: readSelfPIDNS,
		run:       iosnoop.RunAggregating,
	}
}

// startIosnoop resolves the io-snoop log path from the GuestConfig's
// observe.iosnoop and, when
// set, opens the
// guest-side trace file and spawns the gadget's drain loop for the agent's
// lifetime. It returns the live aggregator so the caller can fold it into the
// build-progress summary alongside the other gadgets', or nil when the gadget
// was not requested / could not start. Like the other start* gadget helpers
// it reports one line per outcome and never fails the boot — observability is a
// debugging surface, not a correctness invariant.
//
// Called before the container, returning a readiness channel that closes once
// the tracepoints are attached; runWith waits on it so the workload's first
// I/O can't outrun the gadget. nil agg + nil channel when the gadget was not
// requested or could not start.
//
//nolint:dupl // deliberately parallel to startSleepsnoop / startExecsnoop; the gadget wirings differ only by gadget type and the shared logic (resolveSelfPIDNS, spawnGadgetDrain) is already factored out — a generic abstraction would be less readable than the symmetry.
func startIosnoop(ctx context.Context, w io.Writer, d runDeps) (agg *iosnoop.Aggregator, ready <-chan struct{}) {
	if d.iosnoop.run == nil {
		return nil, nil
	}
	path, ok := resolveObservePath(d.cfg, (*agentv1.GuestConfig_Observe).GetIosnoop)
	if !ok {
		return nil, nil
	}
	if err := d.iosnoop.mountTracefs(); err != nil {
		writeReport(w, IosnoopReportPrefix, err.Error())
		return nil, nil
	}
	f, err := d.iosnoop.openLog(path)
	if err != nil {
		writeReport(w, IosnoopReportPrefix, err.Error())
		return nil, nil
	}
	selfPidns, scope := resolveSelfPIDNS(w, IosnoopReportPrefix, d.iosnoop.selfPIDNS)
	agg = iosnoop.NewAggregator()
	writeReport(w, IosnoopReportPrefix, "tracing "+scope+" fd I/O → "+path)
	ready = spawnGadgetDrain(ctx, w, IosnoopReportPrefix, f, func(onReady func()) error {
		return d.iosnoop.run(ctx, f, agg, selfPidns, onReady)
	})
	return agg, ready
}
