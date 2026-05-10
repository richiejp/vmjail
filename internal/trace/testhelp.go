package trace

import (
	"bytes"
)

// TB is the subset of testing.TB we need for LogOnFailure. Defining
// it here lets the package stay free of a testing dependency while
// callers (including non-_test.go integration helpers) plug in
// either testing.T directly or a fake.
type TB interface {
	Cleanup(func())
	Failed() bool
	Logf(format string, args ...any)
}

// LogOnFailure registers a Cleanup that, on test failure, dumps the
// tracer's current Snapshot via tb.Logf in human-readable form.
// Safe to call multiple times per test (each registers an
// independent Cleanup) but typically called once at test setup.
//
// The dump runs inside Cleanup, so it captures spans created
// throughout the test including any opened-but-not-ended ones
// (Shutdown is invoked first to abandon them — that's how a hung
// span surfaces in the failure log).
func LogOnFailure(tb TB, tr *Tracer) {
	if tb == nil || tr == nil {
		return
	}
	tb.Cleanup(func() {
		if !tb.Failed() {
			return
		}
		tr.abandonAll(tr.nowFn())
		var buf bytes.Buffer
		if err := FormatText(&buf, tr.Snapshot()); err != nil {
			tb.Logf("trace: FormatText: %v", err)
			return
		}
		tb.Logf("trace dump:\n%s", buf.String())
	})
}
