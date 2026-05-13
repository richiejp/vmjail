// Package a is the fixture for nodoubleptr's analysistest.
package a

// FieldDoubleP has a **int field; analyzer must fire.
type FieldDoubleP struct {
	p **int // want `double-pointer`
}

// paramDoubleP takes a **int; analyzer must fire.
func paramDoubleP(x **int) { _ = x } // want `double-pointer`

// resultDoubleP returns a **int; analyzer must fire.
func resultDoubleP() **int { return nil } // want `double-pointer`

// varDoubleP declares a **int; analyzer must fire.
var varDoubleP **int // want `double-pointer`

// sliceOfDoubleP nests **int inside a slice; analyzer must fire.
var sliceOfDoubleP []**int // want `double-pointer`

// Suppressed shows the directive silences the diagnostic when
// placed directly above the offending type expression.
type Suppressed struct {
	//daft:lint:disable=nodoubleptr reason="fixture exercises suppression"
	p **int
}

// Single-pointer cases must not fire.
type SinglePtr struct {
	p *int
}

func singlePtr(x *int) *int { return x }
