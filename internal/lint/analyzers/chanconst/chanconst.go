// Package chanconst hosts the analyzer that enforces a typed-constant
// buffer size on every buffered channel construction (Power of 10
// rule 3, Go form).
//
// In a GC'd language, Holzmann's literal "no dynamic memory after
// init" doesn't translate. What does translate is the *spirit*:
// bounded memory under load. Channel buffers sourced from runtime
// values are the easiest way for an untrusted producer to make us
// allocate without bound. Forcing the size to a typed constant
// keeps the bound visible to reviewers and to the type system.
//
// Allowed:
//   - `make(chan T)` (unbuffered).
//   - `make(chan T, 16)` (literal).
//   - `make(chan T, BufSize)` where BufSize is a typed `const`.
//   - `make(chan T, A+B)` where A and B are constants (constant
//     expressions fold during type-checking).
//
// Flagged: any non-constant expression in the buffer-size slot —
// function parameters, function calls, slice lengths, etc.
//
// Suppression: `//vmjail:lint:disable=chanconst reason="…"`. The
// reason should explain why a runtime-sized buffer is genuinely
// needed (e.g. "queue size derived from a config we validated
// against a hard ceiling at startup").
package chanconst

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"

	"github.com/richiejp/vmjail/internal/lint/suppress"
)

// analyzerName drives Analyzer.Name and the //vmjail:lint:disable=
// match; one constant keeps them in lock-step.
const analyzerName = "chanconst"

// Analyzer is the chanconst analyzer entry point.
//
//nolint:gochecknoglobals // go/analysis API contract.
var Analyzer = &analysis.Analyzer{
	Name: analyzerName,
	Doc:  "make(chan T, n) buffer size must be a typed constant or unbuffered (Power of 10 rule 3, Go form)",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	sup := suppress.New(pass)
	for _, f := range pass.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			checkCall(pass, sup, call)
			return true
		})
	}
	//nolint:nilnil // standard x/tools analyzer pattern: no result, no error.
	return nil, nil
}

// checkCall reports if call is a buffered make(chan ...) with a
// non-constant size argument and the position isn't suppressed.
func checkCall(pass *analysis.Pass, sup *suppress.Set, call *ast.CallExpr) {
	if !isChanMake(pass, call) {
		return
	}
	if len(call.Args) < 2 {
		return // unbuffered — allowed unconditionally.
	}
	size := call.Args[1]
	if tv, ok := pass.TypesInfo.Types[size]; ok && tv.Value != nil {
		return // constant expression — allowed.
	}
	if sup.Disabled(call.Pos(), analyzerName) {
		return
	}
	pass.Reportf(size.Pos(),
		"make(chan ...) buffer size must be a typed constant (Power of 10 rule 3, Go form); name the bound or use an unbuffered channel")
}

// isChanMake reports whether call is `make(chan T, …)` resolved
// against the universe-scope `make` builtin and the channel type
// expression. Type info plus the AST shape together rule out
// shadowed identifiers named "make" and array/map make calls.
//
// The `obj missing from Uses` and `len(call.Args) == 0` guards
// are elided: a type-checked program always has the builtin's
// identifier in Uses, and make() with zero args fails type-check
// well before the analyzer runs.
func isChanMake(pass *analysis.Pass, call *ast.CallExpr) bool {
	ident, ok := call.Fun.(*ast.Ident)
	if !ok {
		return false
	}
	builtin, ok := pass.TypesInfo.Uses[ident].(*types.Builtin)
	if !ok || builtin.Name() != "make" {
		return false
	}
	_, isChan := call.Args[0].(*ast.ChanType)
	return isChan
}
