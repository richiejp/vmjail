// Package a is the fixture for forloopbound's analysistest.
package a

// bareFor fires — no annotation.
func bareFor() {
	for { // want `bare .for {}. loop requires`
		break
	}
}

// suppressedFor pins that the directive silences the diagnostic.
func suppressedFor() {
	//daft:lint:disable=forloopbound reason="fixture exercises suppression"
	for {
		break
	}
}

// boundedFor uses a counter; analyzer must not fire.
func boundedFor() {
	for i := 0; i < 10; i++ {
		_ = i
	}
}

// condFor uses a condition; analyzer must not fire.
func condFor(stop bool) {
	for !stop {
		stop = true
	}
}

// rangeFor uses range; analyzer must not fire.
func rangeFor(s []int) {
	for _, x := range s {
		_ = x
	}
}
