package trace

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// FormatText writes a human-readable rendering of spans to w,
// grouped by trace_id and indented by parent depth. Designed for
// test failure dumps and `daft trace dump` — not stable for
// programmatic parsing.
//
// Output shape per trace:
//
//	trace <trace-id>  (<n> spans, <duration> total)
//	  ├─ <name> [<kind>] <status> <duration>  attrs={...}
//	  │   ▸ <event>  attrs={...}
//	  │  └─ <child-name> [<kind>] <status> <duration>
//	  └─ <orphan-name> [<kind>] <status> <duration>
//
// Spans whose parent_span_id refers to a span not present in the
// input list are rendered as roots ("orphan") so a partial buffer
// still surfaces something useful.
func FormatText(w io.Writer, spans []*Span) error {
	if len(spans) == 0 {
		_, err := fmt.Fprintln(w, "(no spans)")
		return err
	}
	byTrace := groupByTrace(spans)
	traceIDs := make([]string, 0, len(byTrace))
	for id := range byTrace {
		traceIDs = append(traceIDs, id)
	}
	sort.Strings(traceIDs)
	for i, id := range traceIDs {
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		if err := formatTrace(w, id, byTrace[id]); err != nil {
			return err
		}
	}
	return nil
}

func groupByTrace(spans []*Span) map[string][]*Span {
	out := map[string][]*Span{}
	for _, s := range spans {
		if s == nil {
			continue
		}
		out[s.traceID] = append(out[s.traceID], s)
	}
	return out
}

func formatTrace(w io.Writer, traceID string, spans []*Span) error {
	earliest, latest := spanBounds(spans)
	total := latest.Sub(earliest)
	if _, err := fmt.Fprintf(w, "trace %s  (%d spans, %s total)\n",
		traceID, len(spans), total.Round(time.Microsecond)); err != nil {
		return err
	}
	roots, childrenOf := buildTree(spans)
	for i, root := range roots {
		last := i == len(roots)-1
		if err := writeSpan(w, root, childrenOf, "", last); err != nil {
			return err
		}
	}
	return nil
}

func spanBounds(spans []*Span) (earliest, latest time.Time) {
	for _, s := range spans {
		if earliest.IsZero() || s.start.Before(earliest) {
			earliest = s.start
		}
		end := s.end
		if end.IsZero() {
			end = s.start
		}
		if end.After(latest) {
			latest = end
		}
	}
	return earliest, latest
}

func buildTree(spans []*Span) (roots []*Span, childrenOf map[string][]*Span) {
	have := map[string]struct{}{}
	for _, s := range spans {
		have[s.spanID] = struct{}{}
	}
	childrenOf = map[string][]*Span{}
	for _, s := range spans {
		if _, parentPresent := have[s.parentID]; s.parentID == "" || !parentPresent {
			roots = append(roots, s)
			continue
		}
		childrenOf[s.parentID] = append(childrenOf[s.parentID], s)
	}
	sortByStart(roots)
	for k := range childrenOf {
		sortByStart(childrenOf[k])
	}
	return roots, childrenOf
}

func sortByStart(spans []*Span) {
	sort.Slice(spans, func(i, j int) bool { return spans[i].start.Before(spans[j].start) })
}

//daft:lint:disable=norecursion reason="tree walk over span children; depth bounded by the producer's trace nesting, which is itself capped by Span.deadline + the reaper"
func writeSpan(w io.Writer, s *Span, childrenOf map[string][]*Span, prefix string, last bool) error {
	branch := "├─ "
	contPrefix := prefix + "│  "
	if last {
		branch = "└─ "
		contPrefix = prefix + "   "
	}
	dur := s.end.Sub(s.start).Round(time.Microsecond)
	if s.end.IsZero() {
		dur = 0
	}
	if _, err := fmt.Fprintf(w, "%s%s%s [%s/%s] %s %s%s\n",
		prefix, branch, s.name, s.component, kindShort(s.kind),
		s.status.String(), dur, statusMsgSuffix(s)); err != nil {
		return err
	}
	if len(s.attrs) > 0 {
		if _, err := fmt.Fprintf(w, "%s   attrs: %s\n", contPrefix, formatAttrs(s.attrs)); err != nil {
			return err
		}
	}
	for _, ev := range s.events {
		if _, err := fmt.Fprintf(w, "%s   ▸ %s%s\n", contPrefix, ev.Name, eventAttrsSuffix(ev)); err != nil {
			return err
		}
	}
	children := childrenOf[s.spanID]
	for i, c := range children {
		clast := i == len(children)-1
		if err := writeSpan(w, c, childrenOf, contPrefix, clast); err != nil {
			return err
		}
	}
	return nil
}

func formatAttrs(kvs []KV) string {
	parts := make([]string, len(kvs))
	for i, kv := range kvs {
		parts[i] = fmt.Sprintf("%s=%q", kv.Key, kv.Value)
	}
	return strings.Join(parts, " ")
}

func kindShort(k SpanKind) string {
	switch k {
	case SpanKindClient:
		return "C"
	case SpanKindServer:
		return "S"
	default:
		return "I"
	}
}

func statusMsgSuffix(s *Span) string {
	if s.statusMsg == "" {
		return ""
	}
	return " — " + s.statusMsg
}

func eventAttrsSuffix(e Event) string {
	if len(e.Attrs) == 0 {
		return ""
	}
	return "  " + formatAttrs(e.Attrs)
}
