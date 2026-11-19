// Package nodoubleptr hosts the analyzer that bans `**T`
// (double-pointer) types anywhere they appear in our Go code
// (Power of 10 rule 9, Go-shaped form).
//
// Holzmann's rule 9 outright bans pointer dereferences of more
// than one level. In Go most of that rule doesn't transfer — the
// language's interface dispatch and closures are the main
// abstraction tool, and banning them would be a death sentence
// for the type system. What does transfer is the `**T` ban: a
// double-pointer in a function signature or struct field almost
// always means data flow that's hard to follow, and there is a
// straightforward alternative (a struct, an interface, or a
// dedicated wrapper type) for every legitimate case I can think
// of.
//
// Suppression: `//vmjail:lint:disable=nodoubleptr reason="…"`. The
// reason should name the legitimate case — the most common is C
// interop where the underlying API demands it.
package nodoubleptr

import (
	"go/ast"

	"golang.org/x/tools/go/analysis"

	"github.com/richiejp/vmjail/internal/lint/suppress"
)

// analyzerName drives both Analyzer.Name and the //vmjail:lint:disable=
// match; one constant keeps them in lock-step.
const analyzerName = "nodoubleptr"

// Analyzer is the nodoubleptr analyzer entry point.
var Analyzer = &analysis.Analyzer{
	Name: analyzerName,
	Doc:  "flag **T (double-pointer) types (Power of 10 rule 9, Go form)",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	sup := suppress.New(pass)
	for _, f := range pass.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			outer, ok := n.(*ast.StarExpr)
			if !ok {
				return true
			}
			if _, inner := outer.X.(*ast.StarExpr); !inner {
				return true
			}
			if sup.Disabled(outer.Pos(), analyzerName) {
				return true
			}
			pass.Reportf(outer.Pos(),
				"**T (double-pointer) types are forbidden by Power of 10 rule 9; wrap through a struct or interface")
			return true
		})
	}
	//nolint:nilnil // standard x/tools analyzer pattern: no result, no error.
	return nil, nil
}
