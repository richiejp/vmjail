package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/richiejp/darkaifactory/internal/observability/progress"
)

const (
	observeStuckThreshold = 5 * time.Second
	observeStaleThreshold = 10 * time.Second
)

// RenderObserveSummary turns a raw local summary.json payload into the
// human/agent-readable report. Empty bytes (gadget still starting, or
// guest observation disabled) render as a clear "no summary
// yet" note rather than an error.
func RenderObserveSummary(now time.Time, sandbox string, raw []byte) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "sandbox:   %s\n", sandbox)
	if len(raw) == 0 {
		b.WriteString("summary:   none yet (sandbox starting, or Tier-2 observe disabled)\n")
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
		writeOpensnoop(&b, *s.Opensnoop, snapAge > observeStaleThreshold)
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
	b.WriteString("\ndrill down: read the local observer JSONL trace for per-event details\n")
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
func writeOpensnoop(b *strings.Builder, op progress.Opensnoop, snapshotStale bool) {
	b.WriteString("\nopen-snoop (file opens)\n")
	fmt.Fprintf(b, "  opens total : %d\n", op.Total)
	fmt.Fprintf(b, "  failed opens: %d\n", op.Errors)
	fmt.Fprintf(b, "  open rate   : %.1f/s\n", op.OpenatRate)
	fmt.Fprintf(b, "  last open   : %s\n", lastOpenAge(op.LastEventAgeMs))
	fmt.Fprintf(b, "  status      : %s\n", opensnoopStatus(op, snapshotStale))

	writeTopN(b, "top processes", op.TopComms)
	writeTopN(b, "top directories", op.TopDirs)
	writeTopN(b, "top files", op.TopPaths)
	writeTopN(b, "top missing (ENOENT)", op.TopMissing)
}

// opensnoopStatus is the coarse build-health verdict (6d will refine):
// no opens yet → starting; a frozen Total past the threshold → possibly
// stalled; otherwise active.
func opensnoopStatus(op progress.Opensnoop, snapshotStale bool) string {
	if snapshotStale {
		return "unknown — snapshot is stale; this is historical activity, not current liveness"
	}
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

func formatBytes(n uint64) string {
	const (
		kib = 1 << 10
		mib = 1 << 20
		gib = 1 << 30
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kib))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
