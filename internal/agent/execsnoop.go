//nolint:dupl // deliberately parallel to sleepsnoop.go / opensnoop.go: the three gadget wirings differ only by gadget type; the shared logic (resolveSelfPIDNS) is already factored out and a generic abstraction would obscure the per-gadget types. Kept side-by-side for review.
package agent

import (
	"context"
	"io"
	"os"

	agentv1 "github.com/localai-org/darkaifactory/internal/genpb/agent/v1"
	"github.com/localai-org/darkaifactory/internal/observability/ebpf/execsnoop"
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
	// into agg. Production wires execsnoop.RunAggregating.
	run func(ctx context.Context, w io.Writer, agg *execsnoop.Aggregator, selfPidns uint32) error
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

// startExecsnoopFromCmdline resolves the exec-snoop log path (the
// GuestConfig's observe.execsnoop, else the daft.observe.execsnoop cmdline
// token) and, when set,
// opens the guest-side trace file and spawns the gadget's drain loop for the
// agent's lifetime. It returns the live aggregator so the caller can fold it
// into the build-progress summary alongside open-snoop's, or nil when the
// gadget was not requested / could not start. Like the other
// start*FromCmdline helpers it reports one line per outcome and never fails
// the boot — observability is a debugging surface, not a correctness
// invariant.
//
// Called before the container so the tracepoint is attaching before the
// workload's first exec.
//
//nolint:dupl // deliberately parallel to startOpensnoopFromCmdline; the two gadget wirings differ only by gadget type and the shared logic (resolveSelfPIDNS) is already factored out — a generic abstraction would be less readable than the symmetry.
func startExecsnoopFromCmdline(ctx context.Context, w io.Writer, d runDeps, cmdline string) *execsnoop.Aggregator {
	if d.execsnoop.run == nil {
		return nil
	}
	path, ok, err := resolveObservePath(d.cfg, (*agentv1.GuestConfig_Observe).GetExecsnoop, ParseExecsnoopCmdline, cmdline)
	switch {
	case err != nil:
		writeReport(w, ExecsnoopReportPrefix, "cmdline: "+err.Error())
		return nil
	case !ok:
		return nil
	}
	if err := d.execsnoop.mountTracefs(); err != nil {
		writeReport(w, ExecsnoopReportPrefix, err.Error())
		return nil
	}
	f, err := d.execsnoop.openLog(path)
	if err != nil {
		writeReport(w, ExecsnoopReportPrefix, err.Error())
		return nil
	}
	selfPidns, scope := resolveSelfPIDNS(w, ExecsnoopReportPrefix, d.execsnoop.selfPIDNS)
	agg := execsnoop.NewAggregator()
	writeReport(w, ExecsnoopReportPrefix, "tracing "+scope+" execve → "+path)
	go func() {
		defer func() { _ = f.Close() }()
		if rerr := d.execsnoop.run(ctx, f, agg, selfPidns); rerr != nil && ctx.Err() == nil {
			writeReport(w, ExecsnoopReportPrefix, rerr.Error())
		}
	}()
	return agg
}
