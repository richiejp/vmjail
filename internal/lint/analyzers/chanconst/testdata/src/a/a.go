// Package a is the fixture for chanconst's analysistest.
package a

const bufSize = 16

// unbuffered: no diagnostic.
func unbuffered() {
	_ = make(chan int)
}

// literal: no diagnostic.
func literal() {
	_ = make(chan int, 5)
}

// namedConst: no diagnostic.
func namedConst() {
	_ = make(chan int, bufSize)
}

// constExpr: 5+3 folds; no diagnostic.
func constExpr() {
	_ = make(chan int, 5+3)
}

// runtimeParam: parameter-derived size; analyzer must fire.
func runtimeParam(n int) {
	_ = make(chan int, n) // want `typed constant`
}

// runtimeCall: function-call size; analyzer must fire.
func runtimeCall() {
	_ = make(chan int, computeSize()) // want `typed constant`
}

func computeSize() int { return 8 }

// runtimeLen: len of a slice; analyzer must fire.
func runtimeLen(s []int) {
	_ = make(chan int, len(s)) // want `typed constant`
}

// suppressed pins the directive silences the diagnostic.
func suppressed(n int) {
	//daft:lint:disable=chanconst reason="fixture exercises suppression"
	_ = make(chan int, n)
}

// otherMake: make([]T, n) is not a channel; analyzer must not fire.
func otherMake(n int) {
	_ = make([]int, n)
}
