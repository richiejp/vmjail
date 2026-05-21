// Package forloopbound hosts the analyzer that requires every
// bare `for { … }` loop (no init, no condition, no post) to carry
// a `//daft:lint:disable=forloopbound reason="…"` directive.
//
// Holzmann's rule 2 mandates a statically-provable upper bound on
// every loop's iteration count. Most Go loops satisfy this for
// free: `for i := 0; i < N; i++` advertises its bound; `for range
// slice` is bounded by the slice; even `for cond {}` usually has a
// condition that visibly terminates. The exception is `for { … }`
// — an infinite loop. These are legitimate (server accept loops,
// event loops, retry loops driven by `<-ctx.Done()`), but every
// one of them is a place where the author needs to have *thought*
// about termination. The analyzer enforces that thinking by
// requiring an explicit annotation with a non-empty reason.
//
// Suppression: place a same-line or preceding-line
// `//daft:lint:disable=forloopbound reason="…"` directive. The
// reason should name the termination signal (e.g. "exits on
// ctx.Done()", "exits when the registration stream closes").
package forloopbound

import (
	"go/ast"

	"golang.org/x/tools/go/analysis"

	"github.com/localai-org/darkaifactory/internal/lint/suppress"
)

// analyzerName drives Analyzer.Name and the //daft:lint:disable=
// match.
const analyzerName = "forloopbound"

// Analyzer is the forloopbound analyzer entry point.
//
//nolint:gochecknoglobals // go/analysis API contract.
var Analyzer = &analysis.Analyzer{
	Name: analyzerName,
	Doc:  "bare `for {}` loops require a //daft:lint:disable=forloopbound reason=\"…\" directive (Power of 10 rule 2)",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	sup := suppress.New(pass)
	for _, f := range pass.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			forStmt, ok := n.(*ast.ForStmt)
			if !ok {
				return true
			}
			// Only bare `for {}` (no init, cond, post) requires
			// the annotation. `for cond {}` and `for i, x := range
			// y` have structural bounds the analyzer trusts.
			if forStmt.Init != nil || forStmt.Cond != nil || forStmt.Post != nil {
				return true
			}
			if sup.Disabled(forStmt.Pos(), analyzerName) {
				return true
			}
			pass.Reportf(forStmt.Pos(),
				"bare `for {}` loop requires a //daft:lint:disable=forloopbound reason=\"…\" directive (Power of 10 rule 2); name the termination signal")
			return true
		})
	}
	//nolint:nilnil // standard x/tools analyzer pattern: no result, no error.
	return nil, nil
}
