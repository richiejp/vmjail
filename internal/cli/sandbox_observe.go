package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	workspacev1 "github.com/localai-org/darkaifactory/internal/genpb/workspace/v1"
	"github.com/localai-org/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
	"github.com/localai-org/darkaifactory/internal/observability/progress"
)

// observeStuckThreshold is how long the open-snoop Total may sit frozen
// before the summary flags the build as possibly stalled. A real build
// opens files continuously; a multi-second gap means a hang (network
// wait, deadlock) far more often than legitimate quiet. 6d refines this
// into richer heuristics; 6c-3 ships the coarse stall flag.
const observeStuckThreshold = 5 * time.Second

// observeStaleThreshold flags the summary file itself as stale: if the
// guest writer's last snapshot is older than this, the writer (or the
// VM) has likely stopped, independent of gadget activity.
const observeStaleThreshold = 10 * time.Second

// observeOpts gathers `daft sandbox observe` flags + the optional
// workspace positional.
type observeOpts struct {
	orchestrator string
	configDir    string
	follow       bool
	interval     time.Duration
	workspace    string
}

func newSandboxObserveCmd() *cobra.Command {
	var opts observeOpts
	cmd := &cobra.Command{
		Use:   "observe [workspace]",
		Short: "Print a running sandbox's build-progress summary",
		Long: "Renders the per-sandbox build-progress summary for a workspace's\n" +
			"running sandbox, via the orchestrator's ObserveSummary RPC. The\n" +
			"summary is the always-on gadgets' live aggregate (today: open-snoop\n" +
			"file-open activity) plus a coarse stuck/slow flag — a quick probe\n" +
			"of whether an in-sandbox build is making progress or has hung.\n\n" +
			"Prints the current snapshot once and exits; --follow re-renders on\n" +
			"an interval. The workspace may be omitted when run from a workspace\n" +
			"dir (./.daft/config.json name, else the cwd basename).\n\n" +
			"For the raw per-event trace, drill down with\n" +
			"`daft sandbox observer-tail`. See OBSERVABILITY.md.",
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.workspace = args[0]
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(),
				os.Interrupt, syscall.SIGTERM)
			defer cancel()
			return runObserve(ctx, cmd.OutOrStdout(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.orchestrator, "orchestrator", "",
		"orchestrator URL (https://host:port)")
	cmd.Flags().StringVar(&opts.configDir, configDirFlag, "",
		"override the daft client config dir (default: $XDG_CONFIG_HOME/daft/client)")
	cmd.Flags().BoolVar(&opts.follow, "follow", false,
		"re-render the summary on --interval until interrupted (default: print once)")
	cmd.Flags().DurationVar(&opts.interval, "interval", time.Second,
		"poll interval for --follow")
	return cmd
}

// observeDeps factors the side-effect surfaces runObserve needs so tests
// can swap in in-process stubs and a fixed clock.
type observeDeps struct {
	dialOrchestrator func(configDirOverride, orchestrator string) (workspacev1connect.WorkspaceCatalogServiceClient, error)
	fetchSummary     func(ctx context.Context, client workspacev1connect.WorkspaceCatalogServiceClient, req *workspacev1.ObserveSummaryRequest) (*workspacev1.ObserveSummaryResponse, error)
	now              func() time.Time
}

func defaultObserveDeps() observeDeps {
	return observeDeps{
		dialOrchestrator: dialWorkspaceCatalogForForward,
		fetchSummary: func(ctx context.Context, client workspacev1connect.WorkspaceCatalogServiceClient, req *workspacev1.ObserveSummaryRequest) (*workspacev1.ObserveSummaryResponse, error) {
			resp, err := client.ObserveSummary(ctx, connect.NewRequest(req))
			if err != nil {
				return nil, err
			}
			return resp.Msg, nil
		},
		now: time.Now,
	}
}

func runObserve(ctx context.Context, stdout io.Writer, opts observeOpts) error {
	return runObserveWith(ctx, stdout, opts, defaultObserveDeps())
}

func runObserveWith(ctx context.Context, stdout io.Writer, opts observeOpts, deps observeDeps) error {
	if opts.workspace == "" {
		name, err := resolveWorkspaceNameDefault()
		if err != nil {
			return err
		}
		opts.workspace = name
	}
	orchestrator, err := resolveOrchestrator(opts.orchestrator, opts.configDir)
	if err != nil {
		return err
	}
	client, err := deps.dialOrchestrator(opts.configDir, orchestrator)
	if err != nil {
		return err
	}
	req := &workspacev1.ObserveSummaryRequest{
		Workspace: &workspacev1.WorkspaceHandle{
			Handle: &workspacev1.WorkspaceHandle_Name{Name: opts.workspace},
		},
	}
	renderOnce := func() error {
		resp, err := deps.fetchSummary(ctx, client, req)
		if err != nil {
			return err
		}
		out, err := renderObserveSummary(deps.now(), opts.workspace, resp.GetSummary())
		if err != nil {
			return err
		}
		_, err = io.WriteString(stdout, out)
		return err
	}
	if !opts.follow {
		return renderOnce()
	}
	ticker := time.NewTicker(opts.interval)
	defer ticker.Stop()
	//daft:lint:disable=forloopbound reason="exits when ctx is cancelled (SIGINT) between polls"
	for {
		if err := renderOnce(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// renderObserveSummary turns a raw ObserveSummary payload into the
// human/agent-readable report. Empty bytes (gadget still starting, or
// Tier-2 observe opted out on the node) render as a clear "no summary
// yet" note rather than an error.
func renderObserveSummary(now time.Time, workspace string, raw []byte) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "workspace: %s\n", workspace)
	if len(raw) == 0 {
		b.WriteString("summary:   none yet (sandbox starting, or Tier-2 observe disabled on the node)\n")
		return b.String(), nil
	}
	s, err := progress.Parse(raw)
	if err != nil {
		return "", err
	}

	snapAge := now.Sub(time.UnixMilli(s.TakenAtUnixMs))
	staleNote := ""
	if snapAge > observeStaleThreshold {
		staleNote = "  ⚠ stale — writer may have stopped"
	}
	fmt.Fprintf(&b, "snapshot:  %s ago%s\n", fmtDuration(snapAge), staleNote)

	if s.Opensnoop == nil && s.ExecSnoop == nil && s.SleepSnoop == nil && s.IOSnoop == nil {
		b.WriteString("\nno gadgets running\n")
		return b.String(), nil
	}
	if s.Opensnoop != nil {
		writeOpensnoop(&b, *s.Opensnoop)
	}
	if s.ExecSnoop != nil {
		writeExecsnoop(&b, *s.ExecSnoop)
	}
	if s.SleepSnoop != nil {
		writeSleepsnoop(&b, *s.SleepSnoop)
	}
	if s.IOSnoop != nil {
		writeIosnoop(&b, *s.IOSnoop)
	}
	b.WriteString("\ndrill down: daft sandbox observer-tail " + workspace +
		"   (raw Tier-1 per-event trace)\n")
	return b.String(), nil
}

// writeExecsnoop renders the exec-snoop section: the liveness headline
// (total / failed / rate / last-exec age + a coarse status) then the
// command rankings, with failed execs surfaced separately since a failing
// exec (command not found) is usually the actionable signal.
func writeExecsnoop(b *strings.Builder, ex progress.ExecSnoop) {
	b.WriteString("\nexec-snoop (process launches)\n")
	fmt.Fprintf(b, "  execs total : %d\n", ex.Total)
	fmt.Fprintf(b, "  failed execs: %d\n", ex.Failed)
	fmt.Fprintf(b, "  exec rate   : %.1f/s\n", ex.ExecRate)
	fmt.Fprintf(b, "  last exec   : %s\n", lastOpenAge(ex.LastEventAgeMs))

	writeTopN(b, "top commands", ex.TopCommands)
	writeTopN(b, "top failed (could not exec)", ex.TopFailed)
}

// writeSleepsnoop renders the sleep-snoop section: the liveness headline
// (call count / cumulative requested sleep / rate / last-sleep age) then the
// two rankings — who calls sleep most (tight poll loops) and who has slept the
// most wall-clock in aggregate (the build-blocking long waits). The sleeper
// ranking's counts are milliseconds, so they render as durations.
func writeSleepsnoop(b *strings.Builder, sl progress.SleepSnoop) {
	b.WriteString("\nsleep-snoop (voluntary sleeps)\n")
	fmt.Fprintf(b, "  sleep calls : %d\n", sl.Total)
	//nolint:gosec // G115: a cumulative-sleep ms total; never exceeds the int64 duration range in practice.
	fmt.Fprintf(b, "  total sleep : %s\n", fmtDuration(time.Duration(sl.TotalSleepMs)*time.Millisecond))
	fmt.Fprintf(b, "  sleep rate  : %.1f/s\n", sl.SleepRate)
	fmt.Fprintf(b, "  last sleep  : %s\n", lastOpenAge(sl.LastEventAgeMs))

	writeTopN(b, "top callers (by sleep count)", sl.TopCallers)
	writeTopMillis(b, "top sleepers (by total sleep)", sl.TopSleepers)
}

// writeIosnoop renders the io-snoop section: the liveness headline (I/O call
// count / failed calls / cumulative bytes read+written / rate / last-I/O age)
// then the three target rankings — biggest readers and writers (by bytes) and
// the chattiest fds (by call count). The reader/writer counts are bytes, so
// they render as sizes.
func writeIosnoop(b *strings.Builder, ios progress.IOSnoop) {
	b.WriteString("\nio-snoop (fd reads/writes)\n")
	fmt.Fprintf(b, "  io calls    : %d\n", ios.Total)
	fmt.Fprintf(b, "  failed calls: %d\n", ios.Failed)
	fmt.Fprintf(b, "  bytes r/w   : %s / %s\n", formatBytes(ios.ReadBytes), formatBytes(ios.WriteBytes))
	fmt.Fprintf(b, "  io rate     : %.1f/s\n", ios.IORate)
	fmt.Fprintf(b, "  last io     : %s\n", lastOpenAge(ios.LastEventAgeMs))

	writeTopBytes(b, "top readers (by bytes)", ios.TopReaders)
	writeTopBytes(b, "top writers (by bytes)", ios.TopWriters)
	writeTopN(b, "top busiest (by io calls)", ios.TopBusiest)
}

// writeTopBytes renders a ranking whose Counted.Count is a byte total (the
// io-snoop reader/writer rankings) as human-readable sizes.
func writeTopBytes(b *strings.Builder, title string, items []progress.Counted) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "  %s:\n", title)
	for _, c := range items {
		fmt.Fprintf(b, "    %s (%s)\n", c.Key, formatBytes(c.Count))
	}
}

// writeTopMillis renders a ranking whose Counted.Count is a millisecond total
// (the sleep-snoop duration ranking) as human-readable durations rather than a
// raw number.
func writeTopMillis(b *strings.Builder, title string, items []progress.Counted) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "  %s:\n", title)
	for _, c := range items {
		//nolint:gosec // G115: a cumulative-sleep ms count; never exceeds the int64 duration range in practice.
		fmt.Fprintf(b, "    %s (%s)\n", c.Key, fmtDuration(time.Duration(c.Count)*time.Millisecond))
	}
}

// writeOpensnoop renders the open-snoop section: the liveness headline
// (total / rate / last-open age + a coarse status), then the bounded
// heavy-hitter rankings.
func writeOpensnoop(b *strings.Builder, op progress.Opensnoop) {
	b.WriteString("\nopen-snoop (file opens)\n")
	fmt.Fprintf(b, "  opens total : %d\n", op.Total)
	fmt.Fprintf(b, "  failed opens: %d\n", op.Errors)
	fmt.Fprintf(b, "  open rate   : %.1f/s\n", op.OpenatRate)
	fmt.Fprintf(b, "  last open   : %s\n", lastOpenAge(op.LastEventAgeMs))
	fmt.Fprintf(b, "  status      : %s\n", opensnoopStatus(op))

	writeTopN(b, "top processes", op.TopComms)
	writeTopN(b, "top directories", op.TopDirs)
	writeTopN(b, "top files", op.TopPaths)
	writeTopN(b, "top missing (ENOENT)", op.TopMissing)
}

// opensnoopStatus is the coarse build-health verdict (6d will refine):
// no opens yet → starting; a frozen Total past the threshold → possibly
// stalled; otherwise active.
func opensnoopStatus(op progress.Opensnoop) string {
	if op.LastEventAgeMs < 0 {
		return "starting (no file opens observed yet)"
	}
	if age := time.Duration(op.LastEventAgeMs) * time.Millisecond; age > observeStuckThreshold {
		return "⚠ possibly stalled — no new file opens for " + fmtDuration(age)
	}
	return "active"
}

func lastOpenAge(ms int64) string {
	if ms < 0 {
		return "never"
	}
	return fmtDuration(time.Duration(ms)*time.Millisecond) + " ago"
}

// writeTopN renders one heavy-hitter ranking. Counts go in parentheses
// after the key so a long /nix/store path needs no column alignment.
func writeTopN(b *strings.Builder, title string, items []progress.Counted) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "  %s:\n", title)
	for _, c := range items {
		fmt.Fprintf(b, "    %s (%d)\n", c.Key, c.Count)
	}
}

// fmtDuration renders a build-scale age compactly: sub-minute as tenths
// of a second (0.1s), longer as whole seconds rounded (1m4s).
func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}
