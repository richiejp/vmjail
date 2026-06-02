// Package progress is the wire contract for the per-sandbox build-progress
// summary (OBSERVABILITY.md roadmap #6). The guest agent rolls the always-on
// gadgets' aggregates into a Summary and atomically rewrites it ~1 Hz onto
// the observe share; the host-side `daft sandbox observe` CLI reads it back.
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

// Summary is the whole build-progress snapshot: a timestamp plus one section
// per enabled gadget. A nil section means that gadget was not running.
type Summary struct {
	// TakenAtUnixMs is the guest wall-clock at which the snapshot was taken,
	// so a reader can tell whether the writer itself has stalled (a stale
	// file) independently of the gadget's own activity age.
	TakenAtUnixMs int64      `json:"taken_at_unix_ms"`
	Opensnoop     *Opensnoop `json:"opensnoop,omitempty"`
	ExecSnoop     *ExecSnoop `json:"execsnoop,omitempty"`
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
