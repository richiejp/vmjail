// Package assertpure hosts the analyzer that enforces the project's
// rule that arguments to internal/assert helpers are side-effect
// free and non-trivial.
//
// What the analyzer flags, for every call to assert.True or
// assert.NoErr:
//
//   - The first argument contains a function call other than a
//     pure builtin (`len`, `cap`, `min`, `max`) or a type
//     conversion. Bind the result to a local first and assert on
//     the local — that way the assertion is genuinely a check of a
//     known value rather than the place where the value is computed.
//   - The first argument is a trivially-true constant (`assert.True(true)`)
//     or a trivially-nil constant (`assert.NoErr(nil)`). Asserts
//     that can never fail are noise; remove them or write a real
//     condition.
//
// The "assert" package is identified by its declared package name
// (`package assert`), so test fixtures can substitute their own
// local stub. See CONTRIBUTING.md "Power of 10 (Go)" for the
// rationale.
package assertpure

import (
	"go/ast"
	"go/constant"
	"go/types"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"

	"github.com/richiejp/darkaifactory/internal/lint/suppress"
)

// Analyzer is the assertpure analyzer entry point.
var Analyzer = &analysis.Analyzer{
	Name:     "assertpure",
	Doc:      "assert.True/NoErr first argument must be side-effect free and non-trivial",
	Run:      run,
	Requires: []*analysis.Analyzer{inspect.Analyzer},
}

// pureBuiltins names the Go universe-scope builtins that are pure
// by language definition and therefore allowed inside assertion
// conditions. The list is intentionally closed; new entries need a
// review-time decision.
var pureBuiltins = map[string]bool{
	"len": true,
	"cap": true,
	"min": true,
	"max": true,
}

// analyzerName is the value matched against //daft:lint:disable=
// directives. Kept as a constant so it can't drift from the
// Analyzer.Name field.
const analyzerName = "assertpure"

func run(pass *analysis.Pass) (any, error) {
	insp, ok := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	if !ok {
		// inspect.Analyzer is in Requires; if its result isn't an
		// *inspector.Inspector the framework is broken, not us.
		//nolint:forbidigo // x/tools/go/analysis framework invariant violation; the analyzer pipeline is broken if we reach here.
		panic("assertpure: inspect.Analyzer result has wrong type")
	}
	sup := suppress.New(pass)
	insp.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		fn := assertCallee(pass, call)
		if fn == nil {
			return
		}
		name := fn.Name()
		if name != "True" && name != "NoErr" {
			return
		}
		if len(call.Args) == 0 {
			return
		}
		if sup.Disabled(call.Pos(), analyzerName) {
			return
		}
		checkArg(pass, name, call.Args[0])
	})
	//nolint:nilnil // standard x/tools analyzer pattern: no result, no error.
	return nil, nil
}

// assertCallee returns the *types.Func behind a call expression iff
// the callee is a function in a package named "assert". Returns nil
// for any non-assert call, dynamic dispatch, or unresolved name.
func assertCallee(pass *analysis.Pass, call *ast.CallExpr) *types.Func {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	obj, ok := pass.TypesInfo.Uses[sel.Sel]
	if !ok {
		return nil
	}
	fn, ok := obj.(*types.Func)
	if !ok {
		return nil
	}
	if fn.Pkg() == nil || fn.Pkg().Name() != "assert" {
		return nil
	}
	return fn
}

// checkArg validates a single first-argument expression against the
// purity + non-triviality rules and reports any violations on pass.
func checkArg(pass *analysis.Pass, fnName string, arg ast.Expr) {
	if reportTrivial(pass, fnName, arg) {
		return
	}
	if containsImpureCall(pass, arg) {
		pass.Reportf(arg.Pos(),
			"assert.%s argument must be side-effect free (no function calls); bind to a local first",
			fnName)
	}
}

// reportTrivial flags constant-true assertions on True and
// untyped-nil assertions on NoErr. Returns true iff a diagnostic
// fired, so the caller can skip the call-presence check (the same
// expression rarely earns two complaints' worth of clarity).
func reportTrivial(pass *analysis.Pass, fnName string, arg ast.Expr) bool {
	switch fnName {
	case "True":
		if tv, ok := pass.TypesInfo.Types[arg]; ok && tv.Value != nil {
			if tv.Value.Kind() == constant.Bool && constant.BoolVal(tv.Value) {
				pass.Reportf(arg.Pos(), "assert.True argument is trivially true")
				return true
			}
		}
	case "NoErr":
		if ident, ok := arg.(*ast.Ident); ok && ident.Name == "nil" {
			pass.Reportf(arg.Pos(), "assert.NoErr argument is trivially nil")
			return true
		}
	}
	return false
}

// containsImpureCall reports whether arg (recursively) contains any
// *ast.CallExpr that is not a pure builtin or a type conversion.
// Pure builtins and type conversions are allowed at the syntactic
// level; their *arguments* are still walked, so `len(foo())` still
// trips on the inner foo() call.
func containsImpureCall(pass *analysis.Pass, arg ast.Expr) bool {
	var seen bool
	ast.Inspect(arg, func(n ast.Node) bool {
		if seen {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isPureCall(pass, call) {
			seen = true
			return false
		}
		// Pure call: keep descending so impure work in its
		// arguments (e.g. len(foo())) is still caught.
		return true
	})
	return seen
}

// isPureCall returns true for calls that are either pure builtins
// or type conversions. Type conversions are recognised via
// pass.TypesInfo — call.Fun is itself a type expression in that
// case.
func isPureCall(pass *analysis.Pass, call *ast.CallExpr) bool {
	if tv, ok := pass.TypesInfo.Types[call.Fun]; ok && tv.IsType() {
		return true
	}
	ident, ok := call.Fun.(*ast.Ident)
	if !ok {
		return false
	}
	obj, ok := pass.TypesInfo.Uses[ident]
	if !ok {
		return false
	}
	builtin, ok := obj.(*types.Builtin)
	if !ok {
		return false
	}
	return pureBuiltins[builtin.Name()]
}
