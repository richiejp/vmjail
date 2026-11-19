// Package nolinkname hosts the analyzer that bans `//go:linkname`
// pragmas in our Go code (Power of 10 rule 1e). The pragma lets a
// package call into private symbols of another package, defeating
// the language's normal visibility rules and the static call-graph
// guarantees that build on them.
//
// Suppression: `//vmjail:lint:disable=nolinkname reason="…"` on the
// same line as the pragma or the line above. Review pushes back on
// suppressions that don't justify why hiding the cross-package
// dependency from static analysis is worth the cost.
package nolinkname

import (
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/richiejp/vmjail/internal/lint/suppress"
)

// analyzerName matches the //vmjail:lint:disable= NAMES entry and the
// Analyzer.Name field; one constant for both keeps them in sync.
const analyzerName = "nolinkname"

// pragma is the prefix our analyzer recognises after stripping the
// comment opener and optional whitespace. Go's compiler is strict
// about the no-space form; we accept the loose form too because the
// intent is the same and we want a useful diagnostic either way.
const pragma = "go:linkname"

// Analyzer is the nolinkname analyzer entry point.
var Analyzer = &analysis.Analyzer{
	Name: analyzerName,
	Doc:  "flag //go:linkname pragmas (Power of 10 rule 1e)",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	sup := suppress.New(pass)
	for _, f := range pass.Files {
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				text := strings.TrimPrefix(c.Text, "//")
				text = strings.TrimPrefix(text, "/*")
				text = strings.TrimLeft(text, " \t")
				if !strings.HasPrefix(text, pragma) {
					continue
				}
				if sup.Disabled(c.Pos(), analyzerName) {
					continue
				}
				pass.Reportf(c.Pos(),
					"//go:linkname is forbidden by Power of 10 rule 1e (no hidden cross-package linkage)")
			}
		}
	}
	//nolint:nilnil // standard x/tools analyzer pattern: no result, no error.
	return nil, nil
}
