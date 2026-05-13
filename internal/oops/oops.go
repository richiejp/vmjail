// Package oops centralises "log this and continue" error handling
// for cases where there's no useful local recovery — failed
// best-effort cleanup, write to a console that may have closed,
// etc.
//
// The core invariant: we never silently drop an error. The only
// kind-of-discard is that we don't return it up the stack; instead
// we write it to a log sink. If even that write fails, we exit the
// process with a non-zero code, because the sink was our last
// chance to surface anything.
//
// This package compensates for one of the language's defaults Go
// makes too easy: `_ = f()` calls. Those are forbidden under the
// project ethos (see CONTRIBUTING.md "Handle every error"). Use
// oops.Log / oops.LogUnless / oops.Must when the caller can't
// usefully act on the error.
package oops

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// FatalExitCode is what the process exits with when the log sink
// itself fails. 2 matches Go's panic-runtime convention so test
// runners and shell pipelines treat it the same way.
const FatalExitCode = 2

// Package-level singletons backing SetSink/SetExitFn so the process
// has exactly one log destination and one exit hook.
//
//nolint:gochecknoglobals // intentional package singleton; mutated only by SetSink/SetExitFn under mu.
var (
	mu     sync.Mutex
	sink   io.Writer = os.Stderr
	exitFn           = os.Exit
)

// SetSink replaces the log destination. Returns the previous sink so
// callers can restore it after a test. The default is os.Stderr.
func SetSink(w io.Writer) (previous io.Writer) {
	mu.Lock()
	defer mu.Unlock()
	previous = sink
	sink = w
	return previous
}

// SetExitFn replaces the exit function called when sink writes fail.
// Tests can swap this for a recorder; production never touches it.
func SetExitFn(fn func(int)) (previous func(int)) {
	mu.Lock()
	defer mu.Unlock()
	previous = exitFn
	exitFn = fn
	return previous
}

// Log writes "label: <err>" to the sink. If err is nil, no-ops.
// If the write itself fails, exits the process with FatalExitCode.
func Log(label string, err error) {
	if err == nil {
		return
	}
	mu.Lock()
	dst := sink
	exit := exitFn
	mu.Unlock()
	if _, werr := fmt.Fprintf(dst, "oops: %s: %v\n", label, err); werr != nil {
		exit(FatalExitCode)
	}
}

// Skip handles cleanup-path errors with an allowlist of expected
// failure modes. Semantics:
//
//   - err == nil: no-op.
//   - err matches any of expected (errors.Is): silently continue.
//     Use this for genuinely benign cases like
//     `os.Remove` of a file that may already be gone
//     (`fs.ErrNotExist`) or `Process.Kill` of a process that
//     already exited (`os.ErrProcessDone`).
//   - err matches none of expected: Must — log it and exit. The
//     caller has already declared what they consider benign;
//     anything else is by definition a problem we can't recover
//     from. Permission errors, corrupted-fs errors, etc. all
//     end up here, which is what the project policy requires —
//     no quiet walk-past on cleanup paths.
//
// nil expected entries are ignored so callers can pass conditional
// values without filtering first.
func Skip(label string, err error, expected ...error) {
	if err == nil {
		return
	}
	for _, e := range expected {
		if e == nil {
			continue
		}
		if errors.Is(err, e) {
			return
		}
	}
	Must(label, err)
}

// Must logs label-prefixed err and exits with FatalExitCode when
// err is non-nil. Use sparingly — most paths should propagate
// errors up the stack instead.
func Must(label string, err error) {
	if err == nil {
		return
	}
	Log(label, err)
	mu.Lock()
	exit := exitFn
	mu.Unlock()
	exit(FatalExitCode)
}
