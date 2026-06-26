package suppress_test

import (
	"go/ast"
	"testing"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/richiejp/darkaifactory/internal/lint/suppress"
)

// probeAnalyzer reports "probe disabled" at every var declaration
// for which suppress.New(pass).Disabled(name.Pos(), "probe")
// returns true. It exists only to drive the unit test of Set, by
// turning each disable-decision into an analysistest diagnostic
// that the testdata fixture can pin via `// want` markers.
var probeAnalyzer = &analysis.Analyzer{
	Name: "probe",
	Doc:  "internal probe used by suppress unit tests",
	Run: func(pass *analysis.Pass) (any, error) {
		sup := suppress.New(pass)
		for _, f := range pass.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				vs, ok := n.(*ast.ValueSpec)
				if !ok {
					return true
				}
				for _, name := range vs.Names {
					if sup.Disabled(name.Pos(), "probe") {
						pass.Reportf(name.Pos(), "probe disabled")
					}
				}
				return true
			})
		}
		//nolint:nilnil // standard x/tools analyzer pattern: no result, no error.
		return nil, nil
	},
}

// TestSet drives the suppress.Set machinery via the probe analyzer
// against testdata/src/a. Coverage on Set.New, Set.Disabled, and
// the lineKey logic comes from the want-annotated fixture.
func TestSet(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), probeAnalyzer, "a")
}
