package iosnoop

import (
	"sort"
	"sync"

	"github.com/richiejp/darkaifactory/internal/observability/progress"
)

// defaultTopN is how many targets each ranking surfaces.
const defaultTopN = 10

// Resolved is one per-fd accumulator with its (pid,fd) already turned into a
// target name by the linux drain (e.g. "/work/out.o", "socket:[12345]"). The
// Aggregator sums these by name, so two fds onto the same file collapse into
// one ranked target.
type Resolved struct {
	Name string
	Stat FDStat
}

// target is the folded-by-name accumulator behind the rankings.
type target struct {
	rd, wr, calls, errors uint64
	role                  uint32
}

// Aggregator rolls the io-snoop BPF maps into a progress.IOSnoop. Unlike the
// other gadgets' incremental aggregators, io-snoop's per-fd map already holds
// cumulative values, so each drain tick rebuilds the whole snapshot from a
// fresh map read via Update — an fd that the LRU map evicted simply drops out
// of the rankings. The Total/Failed/byte totals are taken from the
// never-evicted per-CPU global passed to Update, so they stay monotonic even
// as the rankings churn.
//
// It is safe for the drain to Update while the summary writer takes a
// Snapshot.
type Aggregator struct {
	mu       sync.Mutex
	totals   Totals
	byTarget map[string]*target
}

// NewAggregator returns an empty Aggregator.
func NewAggregator() *Aggregator {
	return &Aggregator{byTarget: map[string]*target{}}
}

// Update replaces the snapshot with one built from the current BPF map
// contents: the monotonic global totals t and the resolved per-fd entries
// (entries sharing a target name are summed). Replacing rather than
// accumulating is correct because the per-fd values are themselves cumulative
// — re-reading the map each tick already reflects all prior I/O.
func (a *Aggregator) Update(t Totals, resolved []Resolved) {
	byTarget := make(map[string]*target, len(resolved))
	for _, r := range resolved {
		e := byTarget[r.Name]
		if e == nil {
			e = &target{}
			byTarget[r.Name] = e
		}
		e.rd += r.Stat.RdBytes
		e.wr += r.Stat.WrBytes
		e.calls += r.Stat.Calls
		e.errors += uint64(r.Stat.Errors)
		e.role |= r.Stat.Role
	}
	a.mu.Lock()
	a.totals = t
	a.byTarget = byTarget
	a.mu.Unlock()
}

// Snapshot returns the current aggregate as a progress.IOSnoop section. The
// time-derived fields (IORate, LastEventAgeMs) are left zero — this aggregate
// is clock-free; the summary writer fills them in.
func (a *Aggregator) Snapshot() progress.IOSnoop {
	a.mu.Lock()
	defer a.mu.Unlock()
	return progress.IOSnoop{
		Total:      a.totals.Calls,
		Failed:     a.totals.Errors,
		ReadBytes:  a.totals.RdBytes,
		WriteBytes: a.totals.WrBytes,
		TopReaders: rank(a.byTarget, func(t *target) uint64 { return t.rd }),
		TopWriters: rank(a.byTarget, func(t *target) uint64 { return t.wr }),
		TopBusiest: rank(a.byTarget, func(t *target) uint64 { return t.calls }),
	}
}

// rank turns the by-target map into a descending top-N ranking by metric,
// dropping zero-metric targets. Ties break on the name so the output is
// deterministic (a build replay and the tests get a stable order).
func rank(m map[string]*target, metric func(*target) uint64) []progress.Counted {
	out := make([]progress.Counted, 0, len(m))
	for name, e := range m {
		if v := metric(e); v > 0 {
			out = append(out, progress.Counted{Key: name, Count: v})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > defaultTopN {
		out = out[:defaultTopN]
	}
	return out
}
