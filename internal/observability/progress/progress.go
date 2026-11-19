// Package progress is the wire contract for the per-sandbox build-progress
// summary (OBSERVABILITY.md roadmap #6). The guest agent rolls the always-on
// gadgets' aggregates into a Summary and atomically rewrites it ~1 Hz onto
// the observe share; the host-side `vmjail sandbox observe` CLI reads it back.
//
// It is a dependency-free leaf on purpose: the summary types must be shared
// by the guest writer and the host reader without dragging the linux-only
// eBPF gadget packages (and cilium/ebpf) into the CLI's dependency graph.
package progress

import (
	"encoding/json"
	"fmt"
	"os"
)

// Counted is one ranked key/count entry in a gadget's heavy-hitter ranking.
type Counted struct {
	Key   string `json:"key"`
	Count uint64 `json:"count"`
}

// Opensnoop is the open-snoop gadget's section of the summary. Total and the
// rankings come straight from the (clock-free) aggregator; OpenatRate and
// LastEventAgeMs are time-derived and filled in by the agent's summary writer,
// which owns the wall-clock.
type Opensnoop struct {
	// Total is the exact number of openat(2) calls observed — the primary
	// liveness signal. A build that has stalled has a frozen Total.
	Total uint64 `json:"total"`
	// Errors is the exact number of observed openat(2) calls that failed
	// (negative return). Climbing Errors against a flat Total often means a
	// workload spinning on a path it can't open.
	Errors uint64 `json:"errors"`
	// OpenatRate is openat(2) calls per second over the last write window.
	OpenatRate float64 `json:"openat_rate_per_sec"`
	// LastEventAgeMs is how long ago (ms) Total last advanced — the stuck
	// signal. -1 means no openat has been observed yet.
	LastEventAgeMs int64 `json:"last_event_age_ms"`
	// TopComms/TopPaths/TopDirs are bounded approximate heavy hitters: the
	// processes, files, and directories the build is churning on.
	TopComms []Counted `json:"top_comms"`
	TopPaths []Counted `json:"top_paths"`
	TopDirs  []Counted `json:"top_dirs"`
	// TopMissing ranks the paths that most often returned ENOENT — the
	// cache-miss / missing-dependency signal that motivated the gadget
	// (an openat returning ENOENT on a path the build cared about).
	TopMissing []Counted `json:"top_missing"`
}

// ExecSnoop is the exec-snoop gadget's section of the summary: process
// launches (execve) the workload made. Total and the rankings come straight
// from the (clock-free) aggregator; ExecRate and LastEventAgeMs are
// time-derived and filled in by the agent's summary writer.
type ExecSnoop struct {
	// Total is the exact number of execve(2) calls observed.
	Total uint64 `json:"total"`
	// Failed is the exact number that returned an error (e.g. ENOENT —
	// command not found). A build re-running a failing exec shows here.
	Failed uint64 `json:"failed"`
	// ExecRate is execve(2) calls per second over the last write window.
	ExecRate float64 `json:"exec_rate_per_sec"`
	// LastEventAgeMs is how long ago (ms) Total last advanced. -1 means no
	// exec has been observed yet.
	LastEventAgeMs int64 `json:"last_event_age_ms"`
	// TopCommands ranks the program paths (argv[0]) the build exec'd most.
	TopCommands []Counted `json:"top_commands"`
	// TopFailed ranks the program paths whose exec failed — the
	// command-not-found / wrong-path signal.
	TopFailed []Counted `json:"top_failed"`
}

// SleepSnoop is the sleep-snoop gadget's section of the summary: the
// nanosleep(2) / clock_nanosleep(2) calls the workload made. Total and the
// rankings come straight from the (clock-free) aggregator; SleepRate and
// LastEventAgeMs are time-derived and filled in by the agent's summary writer.
// It is the poll-with-sleep retry-loop signal — a build that is slow because a
// process keeps sleeping-and-retrying instead of waiting on a real event.
type SleepSnoop struct {
	// Total is the exact number of sleep calls observed.
	Total uint64 `json:"total"`
	// TotalSleepMs is the cumulative *requested* sleep across all calls, in
	// milliseconds — the wall-clock the workload voluntarily gave up (modulo
	// early wakeups). Absolute-deadline sleeps contribute 0 (their duration is
	// not recorded), so this is a lower bound.
	TotalSleepMs uint64 `json:"total_sleep_ms"`
	// SleepRate is sleep calls per second over the last write window — a high
	// rate against little real progress is the tell-tale poll loop.
	SleepRate float64 `json:"sleep_rate_per_sec"`
	// LastEventAgeMs is how long ago (ms) Total last advanced. -1 means no
	// sleep has been observed yet.
	LastEventAgeMs int64 `json:"last_event_age_ms"`
	// TopCallers ranks the comms that called sleep most often — the tight
	// poll loops (many short sleeps).
	TopCallers []Counted `json:"top_callers"`
	// TopSleepers ranks the comms by cumulative requested sleep in
	// milliseconds (Counted.Count is milliseconds here, not a call count) —
	// who is voluntarily giving up the most wall-clock.
	TopSleepers []Counted `json:"top_sleepers"`
}

// IOSnoop is the io-snoop gadget's section of the summary: the bytes the
// workload moved through file descriptors (read/write families, socket
// send/recv, and the zero-copy fd→fd family). Total/Failed/ReadBytes/
// WriteBytes and the rankings come from the (clock-free) aggregator; IORate and
// LastEventAgeMs are time-derived and filled in by the agent's summary writer.
// It is the "what is this build shovelling data through" signal — a big
// artifact write or a chatty package socket — complementing open-snoop's
// "which paths" with "how much".
type IOSnoop struct {
	// Total is the exact number of fd-I/O syscalls observed (all traced
	// families), from the monotonic per-CPU global — the liveness signal.
	Total uint64 `json:"total"`
	// Failed is the exact number that returned an error (negative return) —
	// e.g. a socket whose peer went away, climbing against a flat Total.
	Failed uint64 `json:"failed"`
	// ReadBytes / WriteBytes are cumulative bytes charged to the read and write
	// sides. A zero-copy splice counts its bytes on both sides (read off the
	// source fd, written to the dest fd), so the two are not mutually exclusive.
	ReadBytes  uint64 `json:"read_bytes"`
	WriteBytes uint64 `json:"write_bytes"`
	// IORate is fd-I/O syscalls per second over the last write window.
	IORate float64 `json:"io_rate_per_sec"`
	// LastEventAgeMs is how long ago (ms) Total last advanced — the stuck
	// signal. -1 means no fd I/O has been observed yet.
	LastEventAgeMs int64 `json:"last_event_age_ms"`
	// TopReaders / TopWriters rank targets by bytes read / written
	// (Counted.Count is bytes here, not a call count) — the biggest data sinks
	// and sources. TopBusiest ranks targets by I/O call count — the chatty
	// many-small-ops fds (the I/O analogue of a poll loop).
	TopReaders []Counted `json:"top_readers"`
	TopWriters []Counted `json:"top_writers"`
	TopBusiest []Counted `json:"top_busiest"`
}

// Summary is the whole build-progress snapshot: a timestamp plus one section
// per enabled gadget. A nil section means that gadget was not running.
type Summary struct {
	// TakenAtUnixMs is the guest wall-clock at which the snapshot was taken,
	// so a reader can tell whether the writer itself has stalled (a stale
	// file) independently of the gadget's own activity age.
	TakenAtUnixMs int64       `json:"taken_at_unix_ms"`
	Opensnoop     *Opensnoop  `json:"opensnoop,omitempty"`
	ExecSnoop     *ExecSnoop  `json:"execsnoop,omitempty"`
	SleepSnoop    *SleepSnoop `json:"sleepsnoop,omitempty"`
	IOSnoop       *IOSnoop    `json:"iosnoop,omitempty"`
}

// Marshal renders a Summary as indented JSON with a trailing newline — the
// file is small (top-N lists) and is meant to be cat-able by a human or
// agent, so readability wins over compactness.
func Marshal(s Summary) ([]byte, error) {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("progress: marshal summary: %w", err)
	}
	return append(data, '\n'), nil
}

// Parse decodes a summary written by Marshal. Used by the host-side reader.
func Parse(data []byte) (Summary, error) {
	var s Summary
	if err := json.Unmarshal(data, &s); err != nil {
		return Summary{}, fmt.Errorf("progress: parse summary: %w", err)
	}
	return s, nil
}

// WriteAtomic rewrites the summary file in one step: it marshals to a sibling
// temp file and renames it over path, so a reader polling the file never
// observes a half-written summary. The temp file shares path's directory so
// the rename stays within one filesystem (an atomic same-dir rename on the
// observe virtiofs share).
func WriteAtomic(path string, s Summary) error {
	data, err := Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	//nolint:gosec // G306: a world-readable diagnostic summary, same 0644 as the raw opensnoop.log.
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("progress: write temp summary: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("progress: rename summary into place: %w", err)
	}
	return nil
}
