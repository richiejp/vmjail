// Package tagcatalog hosts the analyzer that constrains
// `//go:build` (and the legacy `// +build`) constraint identifiers
// to a closed allowlist.
//
// Holzmann's rule 8 bans most of the C preprocessor for a simple
// reason: every conditional-compilation directive doubles the
// number of code paths to test. Go has no `#ifdef`, but build tags
// are the equivalent: a novel tag silently bifurcates the build
// surface. The analyzer therefore requires every tag identifier
// to come from a small, intentional catalog.
//
// Current catalog: linux, darwin, integration. These cover the
// platform split (`linux` / `!linux` / `darwin`) and the
// e2e-vs-unit split (`integration`). Adding a new tag is a
// deliberate review-time decision; update `allowedTags` and
// CONTRIBUTING.md in the same change.
//
// Suppression: `//daft:lint:disable=tagcatalog reason="…"`. Use
// sparingly — the right answer to "I need a new tag" is usually
// to update the catalog, not to suppress the gate.
package tagcatalog

import (
	"go/build/constraint"

	"golang.org/x/tools/go/analysis"

	"github.com/richiejp/darkaifactory/internal/lint/suppress"
)

// analyzerName drives Analyzer.Name and the //daft:lint:disable=
// match.
const analyzerName = "tagcatalog"

// allowedTags is the closed catalog of build constraint
// identifiers permitted in our packages. The map's value is a
// short explanation of the tag's intent — kept on the same line
// so a reviewer adding a tag has to write down *why*.
//
//nolint:gochecknoglobals // closed allowlist; data, not state.
var allowedTags = map[string]string{
	"linux":       "platform: Linux-only files (vsock, FUSE, the in-VM agent)",
	"darwin":      "platform: macOS-only files (currently unused; reserved for IDE/cross-platform stubs)",
	"integration": "e2e tests behind `make integration` — real Cloud Hypervisor against /dev/kvm",
}

// Analyzer is the tagcatalog analyzer entry point.
//
//nolint:gochecknoglobals // go/analysis API contract.
var Analyzer = &analysis.Analyzer{
	Name: analyzerName,
	Doc:  "//go:build tag identifiers must come from a closed catalog (Power of 10 rule 8)",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	sup := suppress.New(pass)
	for _, f := range pass.Files {
		// Build-constraint comments must appear in the leading
		// comment group of the file (above the package decl).
		// f.Comments includes every comment in the file; we
		// filter to actual build directives via the constraint
		// package.
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				expr, ok := parseBuildConstraint(c.Text)
				if !ok {
					continue
				}
				badTags := unknownTags(expr)
				if len(badTags) == 0 {
					continue
				}
				// Report at the file's package keyword: the
				// build-constraint line is consumed by Go's
				// constraint parser and can't carry trailing
				// `// want …` markers in testdata, so analysistest
				// pins expectations on the package line. The
				// diagnostic message names the offending tag so
				// the reviewer can locate the constraint quickly.
				pos := f.Package
				if sup.Disabled(pos, analyzerName) || sup.Disabled(c.Pos(), analyzerName) {
					continue
				}
				for _, t := range badTags {
					pass.Reportf(pos,
						"build tag %q not in tagcatalog allowlist (Power of 10 rule 8); add to allowedTags or use a sanctioned tag",
						t)
				}
			}
		}
	}
	//nolint:nilnil // standard x/tools analyzer pattern: no result, no error.
	return nil, nil
}

// parseBuildConstraint returns the parsed expression iff text is a
// well-formed //go:build or // +build directive. Malformed
// directives are not our concern — the go vet / build pipeline
// flags them separately.
func parseBuildConstraint(text string) (constraint.Expr, bool) {
	if !constraint.IsGoBuild(text) && !constraint.IsPlusBuild(text) {
		return nil, false
	}
	expr, err := constraint.Parse(text)
	if err != nil {
		return nil, false
	}
	return expr, true
}

// unknownTags returns every TagExpr identifier appearing in expr
// that is not in allowedTags. Order is left-to-right; duplicates
// collapse so the reviewer doesn't see the same name twice.
func unknownTags(expr constraint.Expr) []string {
	var bad []string
	seen := map[string]bool{}
	visit := func(tag string) {
		if _, ok := allowedTags[tag]; ok {
			return
		}
		if seen[tag] {
			return
		}
		seen[tag] = true
		bad = append(bad, tag)
	}
	walkTags(expr, visit)
	return bad
}

// walkTags walks the constraint tree, invoking visit for every
// TagExpr leaf. constraint.Expr exposes no built-in walker; the
// type switch is exhaustive across the four concrete types the
// package defines. Recursive on purpose — the constraint tree is
// trivially bounded by the source line's length (Go's constraint
// parser caps expressions at one line per directive), so the
// recursion depth is small and statically bounded.
func walkTags(expr constraint.Expr, visit func(string)) {
	switch e := expr.(type) {
	case *constraint.TagExpr:
		visit(e.Tag)
	case *constraint.NotExpr:
		walkTags(e.X, visit)
	case *constraint.AndExpr:
		walkTags(e.X, visit)
		walkTags(e.Y, visit)
	case *constraint.OrExpr:
		walkTags(e.X, visit)
		walkTags(e.Y, visit)
	}
}
