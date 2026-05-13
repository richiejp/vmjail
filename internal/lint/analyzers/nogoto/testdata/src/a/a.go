// Package a is the fixture for nogoto's analysistest.
package a

// hasGoto contains a forbidden goto; analyzer must fire.
func hasGoto() {
	i := 0
loop:
	if i < 3 {
		i++
		goto loop // want `goto is forbidden`
	}
}

// suppressed pins that //daft:lint:disable=nogoto silences the
// diagnostic.
func suppressed() {
	i := 0
again:
	if i < 3 {
		i++
		//daft:lint:disable=nogoto reason="fixture exercises suppression"
		goto again
	}
}

// noGoto is a normal control-flow function; analyzer must not fire.
func noGoto() {
	for i := 0; i < 3; i++ {
		_ = i
	}
}
