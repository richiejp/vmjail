package iosnoop

import "testing"

// res is a terse Resolved constructor for the table cases: a target name plus
// the counters charged to it.
func res(name string, rd, wr, calls uint64, role uint32) Resolved {
	return Resolved{Name: name, Stat: FDStat{RdBytes: rd, WrBytes: wr, Calls: calls, Role: role}}
}

func TestAggregatorFoldsByTargetAndTotals(t *testing.T) {
	a := NewAggregator()
	// Two fds onto the same file collapse into one ranked target; a socket and
	// a pipe rank separately.
	a.Update(Totals{Calls: 9, RdBytes: 300, WrBytes: 120, Errors: 2}, []Resolved{
		res("/work/out.o", 0, 100, 3, roleWrite),
		res("/work/out.o", 0, 20, 1, roleWrite),
		res("socket:[42]", 200, 0, 4, roleRead),
		res("pipe:[7]", 100, 0, 1, roleRead),
	})
	s := a.Snapshot()

	if s.Total != 9 || s.Failed != 2 || s.ReadBytes != 300 || s.WriteBytes != 120 {
		t.Fatalf("totals: %+v", s)
	}
	// TopWriters: out.o summed to 120 bytes, the only writer.
	if len(s.TopWriters) != 1 || s.TopWriters[0].Key != "/work/out.o" || s.TopWriters[0].Count != 120 {
		t.Fatalf("TopWriters = %+v", s.TopWriters)
	}
	// TopReaders ranked by bytes: socket (200) before pipe (100).
	if len(s.TopReaders) != 2 || s.TopReaders[0].Key != "socket:[42]" || s.TopReaders[0].Count != 200 {
		t.Fatalf("TopReaders = %+v", s.TopReaders)
	}
	// TopBusiest by calls: socket(4), out.o(4), pipe(1) — tie on 4 breaks on key.
	if len(s.TopBusiest) != 3 || s.TopBusiest[0].Count != 4 || s.TopBusiest[0].Key != "/work/out.o" {
		t.Fatalf("TopBusiest = %+v", s.TopBusiest)
	}
}

func TestAggregatorDropsZeroMetricAndReplacesSnapshot(t *testing.T) {
	a := NewAggregator()
	a.Update(Totals{Calls: 1}, []Resolved{res("/a", 50, 0, 1, roleRead)})
	if got := a.Snapshot().TopWriters; len(got) != 0 {
		t.Fatalf("a read-only target must not appear in TopWriters: %+v", got)
	}
	// A fresh Update fully replaces the prior snapshot (cumulative map re-read),
	// so the earlier target is gone.
	a.Update(Totals{Calls: 2}, []Resolved{res("/b", 0, 10, 2, roleWrite)})
	s := a.Snapshot()
	if len(s.TopReaders) != 0 || len(s.TopWriters) != 1 || s.TopWriters[0].Key != "/b" {
		t.Fatalf("snapshot not replaced: %+v", s)
	}
}

func TestAggregatorTopNCap(t *testing.T) {
	a := NewAggregator()
	in := make([]Resolved, 0, defaultTopN+5)
	for i := range defaultTopN + 5 {
		// Distinct names, descending byte counts so order is determinate.
		in = append(in, res(string(rune('a'+i)), uint64(1000-i), 0, 1, roleRead))
	}
	a.Update(Totals{}, in)
	if got := len(a.Snapshot().TopReaders); got != defaultTopN {
		t.Fatalf("TopReaders capped at %d, got %d", defaultTopN, got)
	}
}

func TestAggregatorEmpty(t *testing.T) {
	s := NewAggregator().Snapshot()
	if s.Total != 0 || len(s.TopReaders) != 0 || len(s.TopWriters) != 0 || len(s.TopBusiest) != 0 {
		t.Fatalf("empty aggregator snapshot not empty: %+v", s)
	}
}

func TestRoleString(t *testing.T) {
	cases := map[uint32]string{
		0:                    "-",
		roleRead:             "r",
		roleWrite:            "w",
		roleRead | roleWrite: "rw",
		roleRead | 0x100:     "r", // unrelated high bits ignored
	}
	for role, want := range cases {
		if got := roleString(role); got != want {
			t.Fatalf("roleString(%#x) = %q, want %q", role, got, want)
		}
	}
}
