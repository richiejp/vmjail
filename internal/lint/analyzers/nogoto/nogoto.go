// Package nogoto hosts the analyzer that bans `goto` statements
// in our Go code (Power of 10 rule 1a). The C-form rationale is
// preserved: an acyclic, structured control-flow graph is a hard
// prerequisite for static reasoning about termination and reach.
//
// Go's tolerance for `goto` is narrower than C's — break/continue
// + labelled statements cover the legitimate use cases — but the
// keyword still exists and the analyzer still has to enforce its
// absence in our packages.
//
// Suppression: `//vmjail:lint:disable=nogoto reason="…"` on the goto
// line or the line above. The reason has to justify why the
// jump-target structure is genuinely necessary; review pushes back
// on lazy ones.
package nogoto

import (
	"go/ast"
	"go/token"

	"golang.org/x/tools/go/analysis"

	"github.com/richiejp/vmjail/internal/lint/suppress"
)

// analyzerName is the value matched against //vmjail:lint:disable=
// directives; kept as a constant so it cannot drift from
// Analyzer.Name.
const analyzerName = "nogoto"

// Analyzer is the nogoto analyzer entry point.
var Analyzer = &analysis.Analyzer{
	Name: analyzerName,
	Doc:  "flag `goto` statements (Power of 10 rule 1a)",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	sup := suppress.New(pass)
	for _, f := range pass.Files {
		ast.Inspect(f, func(n ast.Node) bool {
			br, ok := n.(*ast.BranchStmt)
			if !ok || br.Tok != token.GOTO {
				return true
			}
			if sup.Disabled(br.Pos(), analyzerName) {
				return true
			}
			pass.Reportf(br.Pos(),
				"goto is forbidden by Power of 10 rule 1a; restructure with early returns or guard clauses")
			return true
		})
	}
	//nolint:nilnil // standard x/tools analyzer pattern: no result, no error.
	return nil, nil
}
