// Package a is the fixture for norecursion's analysistest.
package a

// selfRec is directly recursive; analyzer must fire.
func selfRec() { // want `participates in a call cycle`
	selfRec()
}

// mutA and mutB are mutually recursive; both must fire.
func mutA() { mutB() } // want `participates in a call cycle`

func mutB() { mutA() } // want `participates in a call cycle`

// threeCycle: a → b → c → a. All three must fire.
func threeA() { threeB() } // want `participates in a call cycle`

func threeB() { threeC() } // want `participates in a call cycle`

func threeC() { threeA() } // want `participates in a call cycle`

// straight: a → b → c, no cycle. Analyzer must not fire.
func straightA() { straightB() }

func straightB() { straightC() }

func straightC() {}

// methodSelfRec: methods can also self-recurse; must fire.
type T struct{}

func (t T) methodSelf() { // want `participates in a call cycle`
	t.methodSelf()
}

// suppressed pins the directive silences the diagnostic.
//
//vmjail:lint:disable=norecursion reason="fixture exercises suppression"
func suppressedSelf() {
	suppressedSelf()
}

// callsOutsideCycle reaches the threeA cycle but is not itself
// in it. Analyzer must not fire here — only the cycle members.
func callsOutsideCycle() {
	threeA()
}

// builtinAndExternalCalls exercises resolveCallee's "not a
// *types.Func" / "not in our package" branches. None of these
// participate in cycles; analyzer must not fire.
func builtinAndExternalCalls(s []int) int {
	_ = len(s) // builtin
	return cap(s)
}

// indirectCall uses a function value; resolveCallee returns nil
// because the call is through a *types.Var, not a *types.Func.
// Analyzer must not fire even though the assigned function self-
// recurses — single-package, AST-only analysis can't see it.
func indirectCall() {
	fv := straightC
	fv()
}
