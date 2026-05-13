// Package a is the fixture for assertpure's analysistest run.
package a

import (
	"errors"

	"assert"
)

// ErrSentinel is a package-level error used to exercise the
// "named-error is fine" path.
var ErrSentinel = errors.New("sentinel")

func getErr() error { return nil }

func getBool() bool { return true }

func getSlice() []int { return nil }

// ok holds calls that must NOT trigger any diagnostic. If any of
// these start firing, the analyzer became too aggressive.
func ok(s []int, x int, err error) {
	assert.True(x > 0, "")
	assert.True(len(s) > 0, "")
	assert.True(cap(s) > 0, "")
	assert.True(s == nil || len(s) == 0, "")
	n := len(s)
	assert.True(n > 0, "")
	b := getBool()
	assert.True(b, "")
	assert.NoErr(err, "")
	assert.NoErr(ErrSentinel, "")
	// Type conversion in the condition is permitted.
	assert.True(int(x) > 0, "")
}

// bad holds calls that MUST trigger the analyzer. Each line has a
// matching analysistest annotation.
func bad(s []int, _ int) {
	assert.True(true, "")               // want `trivially true`
	assert.NoErr(nil, "")               // want `trivially nil`
	assert.True(1 == 1, "")             // want `trivially true`
	assert.True(getBool(), "")          // want `side-effect free`
	assert.NoErr(getErr(), "")          // want `side-effect free`
	assert.True(len(getSlice()) > 0, "") // want `side-effect free`
}

// suppressed pins that //daft:lint:disable=assertpure with a valid
// reason silences the diagnostic. Used as the integration test for
// the suppress.Set machinery — if the suppression stops working,
// these lines start failing analysistest.
func suppressed() {
	//daft:lint:disable=assertpure reason="documenting trivially-true literal in fixture"
	assert.True(true, "")

	// Same-line directive form.
	assert.NoErr(nil, "") //daft:lint:disable=assertpure reason="exercises trivial-nil path"
}
