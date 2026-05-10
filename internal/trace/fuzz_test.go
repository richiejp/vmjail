package trace

import (
	"strings"
	"testing"
)

// FuzzIsValidTraceID covers the wire-header validator that gates
// trace-context propagation in the server interceptor. Because a
// peer can write any string into the Daft-Trace-Id header, the
// validator's job is to filter to just well-formed lowercase hex
// of the right length. Anything that survives this is then trusted
// downstream as a real trace ID.
func FuzzIsValidTraceID(f *testing.F) {
	f.Add(strings.Repeat("a", 32))
	f.Add(strings.Repeat("0", 32))
	f.Add("")
	f.Add("ABCDEF0123456789ABCDEF0123456789")
	f.Add("g" + strings.Repeat("a", 31))
	f.Add(strings.Repeat("a", 31))
	f.Add(strings.Repeat("a", 33))

	f.Fuzz(func(t *testing.T, s string) {
		got := IsValidTraceID(s)
		if !got {
			return
		}
		// Anything accepted must be exactly 32 lowercase hex
		// characters — the contract the server interceptor relies
		// on to safely parent its span.
		if len(s) != 32 {
			t.Fatalf("IsValidTraceID accepted len=%d: %q", len(s), s)
		}
		for i := range len(s) {
			c := s[i]
			ok := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
			if !ok {
				t.Fatalf("IsValidTraceID accepted non-hex char %q at %d: %q", c, i, s)
			}
		}
	})
}

// FuzzIsValidSpanID is the parallel target for span IDs (16 hex
// chars instead of 32).
func FuzzIsValidSpanID(f *testing.F) {
	f.Add(strings.Repeat("a", 16))
	f.Add("")
	f.Add("0123456789abcdef")
	f.Add("0123456789abcdeF")

	f.Fuzz(func(t *testing.T, s string) {
		got := IsValidSpanID(s)
		if !got {
			return
		}
		if len(s) != 16 {
			t.Fatalf("IsValidSpanID accepted len=%d: %q", len(s), s)
		}
		for i := range len(s) {
			c := s[i]
			ok := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
			if !ok {
				t.Fatalf("IsValidSpanID accepted non-hex char %q at %d: %q", c, i, s)
			}
		}
	})
}
