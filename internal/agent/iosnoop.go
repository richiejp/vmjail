//nolint:dupl // deliberately parallel to sleepsnoop.go / execsnoop.go / opensnoop.go: the gadget wirings differ only by gadget type; the shared logic (resolveSelfPIDNS) is already factored out and a generic abstraction would obscure the per-gadget types. Kept side-by-side for review.
package agent

import (
	"context"
	"io"
	"os"

	agentv1 "github.com/localai-org/darkaifactory/internal/genpb/agent/v1"
	"github.com/localai-org/darkaifactory/internal/observability/ebpf/iosnoop"
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
	// snapshot into agg. Production wires iosnoop.RunAggregating.
	run func(ctx context.Context, w io.Writer, agg *iosnoop.Aggregator, selfPidns uint32) error
}

// defaultIosnoopDeps wires the real gadget. ensureTracefs + readSelfPIDNS are
// shared with the other gadgets (both Linux-only).
func defaultIosnoopDeps() iosnoopDeps {
	return iosnoopDeps{
		mountTracefs: ensureTracefs,
		openLog: func(path string) (io.WriteCloser, error) {
			//nolint:gosec // G304: path is the host-operator-set daft.observe.iosnoop cmdline value — same trust boundary as daft.bundle.
			return os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		},
		selfPIDNS: readSelfPIDNS,
		run:       iosnoop.RunAggregating,
	}
}

// startIosnoopFromCmdline resolves the io-snoop log path (the GuestConfig's
// observe.iosnoop, else the daft.observe.iosnoop cmdline token) and, when
// set, opens the
// guest-side trace file and spawns the gadget's drain loop for the agent's
// lifetime. It returns the live aggregator so the caller can fold it into the
// build-progress summary alongside the other gadgets', or nil when the gadget
// was not requested / could not start. Like the other start*FromCmdline helpers
// it reports one line per outcome and never fails the boot — observability is a
// debugging surface, not a correctness invariant.
//
// Called before the container so the tracepoints are attaching before the
// workload's first I/O.
//
//nolint:dupl // deliberately parallel to startSleepsnoopFromCmdline / startExecsnoopFromCmdline; the gadget wirings differ only by gadget type and the shared logic (resolveSelfPIDNS) is already factored out — a generic abstraction would be less readable than the symmetry.
func startIosnoopFromCmdline(ctx context.Context, w io.Writer, d runDeps, cmdline string) *iosnoop.Aggregator {
	if d.iosnoop.run == nil {
		return nil
	}
	path, ok, err := resolveObservePath(d.cfg, (*agentv1.GuestConfig_Observe).GetIosnoop, ParseIosnoopCmdline, cmdline)
	switch {
	case err != nil:
		writeReport(w, IosnoopReportPrefix, "cmdline: "+err.Error())
		return nil
	case !ok:
		return nil
	}
	if err := d.iosnoop.mountTracefs(); err != nil {
		writeReport(w, IosnoopReportPrefix, err.Error())
		return nil
	}
	f, err := d.iosnoop.openLog(path)
	if err != nil {
		writeReport(w, IosnoopReportPrefix, err.Error())
		return nil
	}
	selfPidns, scope := resolveSelfPIDNS(w, IosnoopReportPrefix, d.iosnoop.selfPIDNS)
	agg := iosnoop.NewAggregator()
	writeReport(w, IosnoopReportPrefix, "tracing "+scope+" fd I/O → "+path)
	go func() {
		defer func() { _ = f.Close() }()
		if rerr := d.iosnoop.run(ctx, f, agg, selfPidns); rerr != nil && ctx.Err() == nil {
			writeReport(w, IosnoopReportPrefix, rerr.Error())
		}
	}()
	return agg
}
