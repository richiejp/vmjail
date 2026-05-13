// Package assert provides runtime assertion helpers for invariants
// that the code itself must uphold.
//
// An assertion is a side-effect-free Boolean check of a condition
// that would mean a *programmer error* if violated — wrong call
// order, broken contract between two collaborating packages, a
// "this can never happen" branch that did. A failure is unrecoverable
// by design; it always panics with the project-wide "assert: " prefix
// so reviewers, log scrapers, and operators recognise the class
// instantly.
//
// Assertions are NOT for boundary input validation — for that, return
// an error. They are NOT for cleanup-path errors — for that, use
// internal/oops.
//
// The argument to True / NoErr must be side-effect free; that
// invariant is enforced by the assertpure analyzer under
// internal/lint/analyzers/assertpure. The point is that an
// assertion's evaluation must not be the only way a program reaches a
// state; if you disabled all assertions, the rest of the program
// would behave identically.
//
// See CONTRIBUTING.md "Use runtime assertions liberally" and
// "Power of 10 (Go)" for the project policy that motivates this
// package.
package assert

import "fmt"

// prefix tags every assertion-failure panic value. Greppable, and
// distinct from oops.Log's "oops:" prefix so log scanners can tell
// the two classes apart at a glance.
const prefix = "assert: "

// True panics if cond is false. The format / args pair is formatted
// with fmt.Sprintf and prefixed with "assert: " before being passed
// to panic.
//
// The cond argument must be side-effect free — no function calls,
// no channel receives, no map writes. Compute any value the
// condition depends on first, bind it to a local, and assert on
// that local. The assertpure analyzer enforces this; bypassing it
// requires `//daft:lint:disable=assertpure reason="…"`.
func True(cond bool, format string, args ...any) {
	if cond {
		return
	}
	panic(prefix + fmt.Sprintf(format, args...))
}

// NoErr panics if err is non-nil. The panic message is
// "assert: <format>: <err.Error()>" so the underlying error is
// preserved verbatim in the trace.
//
// Use this when the surrounding code's invariants make the error
// genuinely impossible — e.g. parsing a constant string with a
// parser that already validated it. For errors that *can* happen,
// return them.
//
// The err argument must be side-effect free, same as True. Bind
// the result of any fallible call to a local first, then assert.
func NoErr(err error, format string, args ...any) {
	if err == nil {
		return
	}
	panic(prefix + fmt.Sprintf(format, args...) + ": " + err.Error())
}
