// Package a is the fixture for suppress.Set's analysistest. A probe
// analyzer reports "probe disabled" at every var name for which
// suppress.Disabled returns true; analysistest's want annotations
// verify the expected (file, line) → disabled mapping.
package a

//vmjail:lint:disable=probe reason="suppresses next line"
var prevLine = 1 // want `probe disabled`

var sameLine = 1 //vmjail:lint:disable=probe reason="suppresses same line" // want `probe disabled`

//vmjail:lint:disable=probe reason="too far to suppress"

var twoAway = 1 // not disabled — a blank line separates the directive

//vmjail:lint:disable=other reason="different analyzer"
var differentName = 1

//vmjail:lint:disable=probe reason=""
var malformedEmpty = 1

//vmjail:lint:disable=probe
var malformedMissing = 1

var noDirective = 1
